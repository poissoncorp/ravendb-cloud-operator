package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// productIDIndex answers "which CloudCluster is bound to this RavenDB Cloud
// cluster", which is what stops two objects managing one cluster.
const productIDIndex = ".status.productID"

// indexClustersByProductID indexes a CloudCluster by the cluster it is bound
// to.
func indexClustersByProductID(obj client.Object) []string {
	cluster, ok := obj.(*v1alpha1.CloudCluster)
	if !ok || cluster.Status.ProductID == "" {
		return nil
	}
	return []string{cluster.Status.ProductID}
}

// bind resolves which RavenDB Cloud cluster this object manages: the one it is
// already bound to, the one its display name finds, or a new one.
//
// The name is a safe identity because RavenDB Cloud keeps display names unique
// per account and offers no way to rename a cluster after creation.
//
// An empty id with a nil error means "not bound yet, requeue as instructed".
func (r *CloudClusterReconciler) bind(
	ctx context.Context,
	original, cluster *v1alpha1.CloudCluster,
	account *v1alpha1.CloudAccount,
	cloud cloudapi.Client,
) (string, ctrl.Result, error) {
	// status.productID outranks anything in spec.
	if cluster.Status.ProductID != "" {
		return cluster.Status.ProductID, ctrl.Result{}, nil
	}

	name := lookupName(cluster)
	ref, err := cloud.FindClusterByName(ctx, name)
	switch {
	case err == nil:
		return r.takeOver(ctx, cluster, cloud, ref)
	case cloudapi.IsAmbiguous(err):
		// Binding to an arbitrary one of two would be worse than doing nothing.
		return r.notBound(ctx, cluster, sentence("Not bound: "+cloudSaid(err))+
			" RavenDB Cloud cannot rename a cluster, so terminate or hide in the portal the one this object "+
			"should not manage, and the next pass binds the other.")
	case cloudapi.IsNotFound(err):
		return r.createUnlessForbidden(ctx, original, cluster, account, cloud, name)
	}
	result, err := r.cloudUnavailable(ctx, cluster, fmt.Sprintf("look for RavenDB Cloud cluster %q", name), err)
	return "", result, err
}

// lookupName is the name to find this object's cluster by: the one a create
// was sent with, if any, and the spec's otherwise. Trusting spec over the
// submitted name is how an object ends up with a stranger's cluster.
func lookupName(cluster *v1alpha1.CloudCluster) string {
	return cmp.Or(cluster.Status.ProvisionAttemptedName, cluster.EffectiveDisplayName())
}

// notBound says why the name did not bind this object, and polls again.
func (r *CloudClusterReconciler) notBound(
	ctx context.Context, cluster *v1alpha1.CloudCluster, message string,
) (string, ctrl.Result, error) {
	r.ready(ctx, cluster, v1alpha1.ReasonNameTaken, message)
	return "", ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}, nil
}

// takeOver binds to a cluster the name found, unless it is not this object's to
// bind.
func (r *CloudClusterReconciler) takeOver(
	ctx context.Context, cluster *v1alpha1.CloudCluster, cloud cloudapi.Client, ref *cloudapi.ClusterRef,
) (string, ctrl.Result, error) {
	holder, err := r.holderOf(ctx, cluster, ref.ID)
	if err != nil {
		return "", ctrl.Result{}, err
	}
	if holder != "" {
		return r.notBound(ctx, cluster, fmt.Sprintf(
			"RavenDB Cloud cluster %q (%s) is already managed by CloudCluster %s, and two objects managing one "+
				"cluster would fight over it, so this one is not bound. Delete one of the two.",
			ref.Name, ref.ID, holder))
	}

	observed, err := cloud.GetCluster(ctx, ref.ID)
	if err != nil {
		result, err := r.cloudUnavailable(ctx, cluster, fmt.Sprintf("read RavenDB Cloud cluster %q (%s)", ref.Name, ref.ID), err)
		return "", result, err
	}
	// The lookup skips a Terminated cluster but not a Terminating one, which
	// would leave this object reporting DeletedInCloud for good.
	if observed.Status == cloudapi.StatusTerminating {
		return r.notBound(ctx, cluster, fmt.Sprintf(
			"RavenDB Cloud cluster %q (%s) is being terminated, so this object was not bound to it. One this "+
				"operator terminates gives up its name once termination completes, and this object looks again "+
				"every %s; one terminated in the portal keeps its name until it is hidden there. To use another "+
				"name now, set spec.displayName.", ref.Name, ref.ID, cluster.EffectivePollInterval()))
	}
	if differs := identityDifferences(&cluster.Spec, observed); len(differs) > 0 {
		return r.notBound(ctx, cluster, fmt.Sprintf(
			"A RavenDB Cloud cluster named %q exists, but %s, so this object was not bound to it and nothing was "+
				"changed. Set spec.displayName to another name to create a new cluster, or those fields to the "+
				"cluster's own values to manage that one.",
			ref.Name, strings.Join(differs, ", and ")))
	}

	// An interrupted create of this object's own, managed as a created cluster.
	// Checked after the identity: a create that may have landed is no reason to
	// take a cluster this manifest would not have made.
	if cluster.Status.ProvisionAttemptedAt != nil {
		emit(r.Recorder, cluster, corev1.EventTypeNormal, v1alpha1.EventCreated,
			"Found the cluster an interrupted create made: %q (%s).", ref.Name, ref.ID)
		return ref.ID, ctrl.Result{}, nil
	}
	if cluster.EffectiveCreationMode() == v1alpha1.CreationCreateOnly {
		return r.notBound(ctx, cluster, fmt.Sprintf(
			"A RavenDB Cloud cluster named %q (%s) already exists, and creationMode CreateOnly never takes one over, "+
				"so this object was not bound and nothing was changed. Set spec.displayName to another name to create "+
				"a new cluster, or spec.creationMode to TakeOverOnly to manage that one.",
			ref.Name, ref.ID))
	}

	cluster.Status.Adopted = true
	if cluster.EffectiveCreationMode() == v1alpha1.CreationTakeOverOnly {
		emit(r.Recorder, cluster, corev1.EventTypeNormal, v1alpha1.EventAdopted,
			"Found RavenDB Cloud cluster %q (%s) already running; %s.", ref.Name, ref.ID, howManaged(cluster))
		return ref.ID, ctrl.Result{}, nil
	}
	// A Warning, because nothing in the manifest asked for this: the same
	// manifest would have created a cluster had the name been free, and this
	// object now publishes the found cluster's certificate.
	emit(r.Recorder, cluster, corev1.EventTypeWarning, v1alpha1.EventAdopted,
		"Found RavenDB Cloud cluster %q (%s) already running and took it over; %s. Nothing in the manifest asked "+
			"for an existing cluster: set spec.creationMode to TakeOverOnly to confirm this one is meant. %s",
		ref.Name, ref.ID, howManaged(cluster), notMeantWayOut(cluster))
	return ref.ID, ctrl.Result{}, nil
}

// notMeantWayOut says how to let go of a cluster taken over by mistake. Under
// Terminate the plain way out, deleting the object, would end the cluster.
func notMeantWayOut(cluster *v1alpha1.CloudCluster) string {
	if cluster.EffectiveDeletionPolicy() == v1alpha1.DeletionTerminate {
		return "If it is not, set spec.deletionPolicy to Retain before deleting this object: deletionPolicy is " +
			"Terminate, so deleting it now terminates that cluster."
	}
	return "If it is not, delete this object, which leaves the cluster as it is, and apply it again with another " +
		"displayName."
}

// howManaged says what a found cluster is managed with, and how to change it
// when that is the Observe default.
func howManaged(cluster *v1alpha1.CloudCluster) string {
	if cluster.Spec.ManagementPolicy == "" {
		return "managing it with managementPolicy Observe, which changes nothing, until spec.managementPolicy is Full"
	}
	return fmt.Sprintf("managing it with managementPolicy %s", cluster.EffectiveManagementPolicy())
}

// identityDifferences lists what a found cluster is that the manifest says it
// is not, among the fields no change can fix: tier, and provider and region
// where the manifest names them.
func identityDifferences(spec *v1alpha1.CloudClusterSpec, observed *cloudapi.Cluster) []string {
	var out []string
	if v1alpha1.Tier(observed.Tier) != spec.Tier {
		out = append(out, fmt.Sprintf("it is %s and spec.tier says %s", observed.Tier, spec.Tier))
	}
	if spec.Provider != "" && v1alpha1.Provider(observed.Provider) != spec.Provider {
		out = append(out, fmt.Sprintf("it runs on %s and spec.provider says %s", observed.Provider, spec.Provider))
	}
	if spec.Region != "" && observed.Region != spec.Region {
		out = append(out, fmt.Sprintf("it is in %s and spec.region says %s", observed.Region, spec.Region))
	}
	return out
}

// holderOf names another CloudCluster already bound to this RavenDB Cloud
// cluster, or returns "" when there is none.
func (r *CloudClusterReconciler) holderOf(ctx context.Context, cluster *v1alpha1.CloudCluster, productID string) (string, error) {
	var list v1alpha1.CloudClusterList
	if err := r.List(ctx, &list, client.MatchingFields{productIDIndex: productID}); err != nil {
		return "", fmt.Errorf("listing the CloudClusters bound to %s: %w", productID, err)
	}
	for _, other := range list.Items {
		if other.Namespace == cluster.Namespace && other.Name == cluster.Name {
			continue
		}
		if leavingWithRetain(&other) {
			continue
		}
		return other.Namespace + "/" + other.Name, nil
	}
	return "", nil
}

// leavingWithRetain reports a CloudCluster being deleted with deletionPolicy
// Retain, which lets go of its cluster without touching it. One leaving with
// Terminate still holds it: it is about to terminate that cluster.
func leavingWithRetain(cluster *v1alpha1.CloudCluster) bool {
	return !cluster.DeletionTimestamp.IsZero() && cluster.EffectiveDeletionPolicy() == v1alpha1.DeletionRetain
}

// createUnlessForbidden creates the cluster the name did not find, when this
// object may create one.
func (r *CloudClusterReconciler) createUnlessForbidden(
	ctx context.Context,
	original, cluster *v1alpha1.CloudCluster,
	account *v1alpha1.CloudAccount,
	cloud cloudapi.Client,
	name string,
) (string, ctrl.Result, error) {
	poll := ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}
	switch {
	case cluster.EffectiveCreationMode() == v1alpha1.CreationTakeOverOnly:
		r.ready(ctx, cluster, v1alpha1.ReasonClusterNotFound, fmt.Sprintf(
			"No RavenDB Cloud cluster named %q in this account, and creationMode TakeOverOnly never creates one. "+
				"If the name is wrong, correct spec.displayName; nothing was created.", name))
		return "", poll, nil
	case cluster.Spec.ManagementPolicy == v1alpha1.ManagementObserve:
		r.ready(ctx, cluster, v1alpha1.ReasonClusterNotFound, fmt.Sprintf(
			"No RavenDB Cloud cluster named %q in this account, and managementPolicy Observe never creates one. "+
				"Set spec.managementPolicy to Full to create it. If the name is wrong, correct spec.displayName; "+
				"nothing was created.", name))
		return "", poll, nil
	case cluster.Spec.Tier == v1alpha1.TierFree:
		// The public API refuses to create a Free product.
		r.ready(ctx, cluster, v1alpha1.ReasonClusterNotFound, fmt.Sprintf(
			"No Free cluster named %q in this account, and Free clusters can only be created in the RavenDB Cloud "+
				"portal. Create one named %q there and this object finds it within %s.",
			name, name, cluster.EffectivePollInterval()))
		return "", poll, nil
	}
	return r.create(ctx, original, cluster, account, cloud)
}

// create asks RavenDB Cloud for a new cluster.
func (r *CloudClusterReconciler) create(
	ctx context.Context,
	original, cluster *v1alpha1.CloudCluster,
	account *v1alpha1.CloudAccount,
	cloud cloudapi.Client,
) (string, ctrl.Result, error) {
	where, err := placementFor(cluster, account)
	if err != nil {
		return "", r.invalidSpec(ctx, cluster, err), nil
	}
	// Instance types are not mirrored into the account's status, so they are
	// read live.
	instanceTypes, err := cloud.ListInstanceTypes(ctx, cloudapi.Provider(where.provider), where.region)
	if err != nil {
		result, err := r.cloudUnavailable(ctx, cluster,
			fmt.Sprintf("list the instance types RavenDB Cloud offers in %s/%s", where.provider, where.region), err)
		return "", result, err
	}
	request, err := buildCreateRequest(cluster, where, instanceTypes)
	if err != nil {
		return "", r.invalidSpec(ctx, cluster, err), nil
	}

	// Persisted before the call, so a crash right after it is recoverable. The
	// name goes with it: recovery has to know what was submitted.
	attempted := metav1.Now()
	cluster.Status.ProvisionAttemptedAt = &attempted
	cluster.Status.ProvisionAttemptedName = request.DisplayName
	r.ready(ctx, cluster, v1alpha1.ReasonCreating, "Asking RavenDB Cloud for a new cluster.")
	if err := r.writeStatusNow(ctx, original, cluster); err != nil {
		return "", ctrl.Result{}, err
	}

	productID, err := cloud.CreateCluster(ctx, request)
	if err != nil {
		if !cloudapi.MayHaveApplied(err) {
			// A definite no: nothing was created, and a marker left behind would
			// make the next cluster to take this name look like this object's
			// own.
			cluster.Status.ProvisionAttemptedAt = nil
			cluster.Status.ProvisionAttemptedName = ""
		}
		result, err := r.createFailure(ctx, cluster, err)
		return "", result, err
	}
	emit(r.Recorder, cluster, corev1.EventTypeNormal, v1alpha1.EventCreated,
		"Created RavenDB Cloud cluster %q (%s) in %s/%s.", request.DisplayName, productID, request.Provider, request.Region)
	log.FromContext(ctx).Info("created a RavenDB Cloud cluster",
		"productID", productID, "displayName", request.DisplayName)
	r.warnIfOpenToTheInternet(cluster, request.AllowedIPs)
	return productID, ctrl.Result{}, nil
}

// invalidSpec reports a spec the operator will not send, and waits for the
// ordinary poll: a bad request is the user's to fix.
func (r *CloudClusterReconciler) invalidSpec(ctx context.Context, cluster *v1alpha1.CloudCluster, err error) ctrl.Result {
	r.ready(ctx, cluster, v1alpha1.ReasonInvalidSpec, sentence(err.Error()))
	return ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}
}

// buildCreateRequest resolves the spec, with its placement already layered over
// the account's defaults, into a create request, validating it against what
// RavenDB Cloud sells there so a failure reads like advice.
func buildCreateRequest(
	cluster *v1alpha1.CloudCluster,
	where placement,
	instanceTypes []cloudapi.InstanceType,
) (cloudapi.CreateClusterRequest, error) {
	spec := &cluster.Spec
	displayName := cluster.EffectiveDisplayName()
	var empty cloudapi.CreateClusterRequest

	instanceType, err := chooseInstanceType(instanceTypes, spec)
	if err != nil {
		return empty, err
	}
	if err := validateStorage(instanceTypes, instanceType, spec.Storage, where.provider); err != nil {
		return empty, err
	}
	sizeGB := spec.Storage.SizeGB
	if sizeGB == 0 {
		sizeGB = smallestDiskFor(instanceTypes, instanceType)
	}
	if err := validateRootDiskSize(where.provider, sizeGB); err != nil {
		return empty, err
	}
	if err := validateAllowedIPs(where.provider, where.allowedIPs); err != nil {
		return empty, err
	}

	// The OpenAPI document marks subdomainName nullable, but RavenDB Cloud's
	// domain-name validator rejects an empty one, so it is derived when unset.
	subdomain, err := resolveSubdomain(spec.Subdomain, displayName)
	if err != nil {
		return empty, err
	}

	return cloudapi.CreateClusterRequest{
		DisplayName:    displayName,
		Provider:       cloudapi.Provider(where.provider),
		Region:         where.region,
		Tier:           cloudapi.Tier(spec.Tier),
		InstanceType:   instanceType,
		ReleaseChannel: where.channel,
		SubdomainName:  subdomain,
		// The only deployment type this operator supports.
		DeploymentType: cloudapi.DeploymentStandard,
		Storage: cloudapi.Storage{
			SizeGiB:        sizeGB,
			Type:           cloudapi.StorageType(spec.Storage.EffectiveStorageType(where.provider)),
			IOPS:           spec.Storage.IOPS,
			ThroughputMBps: throughputToAPI(spec.Storage.ThroughputMBps),
		},
		AllowedIPs: where.allowedIPs,
	}, nil
}

// chooseInstanceType is the spec's instance type, checked against the tier, or
// the tier's smallest when the spec names none.
func chooseInstanceType(available []cloudapi.InstanceType, spec *v1alpha1.CloudClusterSpec) (string, error) {
	if spec.InstanceType == "" {
		return smallestForTier(available, spec.Tier)
	}
	return spec.InstanceType, validateInstanceType(available, spec.InstanceType, spec.Tier)
}

// warnIfOpenToTheInternet says so when a cluster was created reachable from
// anywhere. A warning rather than a refusal, because 0.0.0.0/0 is right for a
// lab; it is also the value people copy out of a quickstart and forget.
func (r *CloudClusterReconciler) warnIfOpenToTheInternet(cluster *v1alpha1.CloudCluster, allowedIPs []string) {
	for _, entry := range allowedIPs {
		if canonicalCIDR(entry) == "0.0.0.0/0" {
			emit(r.Recorder, cluster, corev1.EventTypeWarning, v1alpha1.EventOpenAllowList,
				"allowedIPs contains %s, so this cluster accepts connections from any address. "+
					"That is fine for a lab and worth narrowing before it holds anything real.", entry)
			return
		}
	}
}

// placement is where a cluster goes and who may reach it: the four fields a
// CloudAccount can supply a house default for.
type placement struct {
	provider   v1alpha1.Provider
	region     string
	channel    string
	allowedIPs []string
}

// placementFor layers the cluster's spec over the account's defaults and checks
// the result against what the account says RavenDB Cloud offers. Each field is
// checked before the next is resolved, so a manifest wrong in two places
// reports the earlier field.
func placementFor(cluster *v1alpha1.CloudCluster, account *v1alpha1.CloudAccount) (placement, error) {
	spec := &cluster.Spec
	var empty placement
	defaults := account.Spec.Defaults
	if defaults == nil {
		defaults = &v1alpha1.AccountDefaults{}
	}

	provider := cmp.Or(spec.Provider, defaults.Provider)
	if provider == "" {
		return empty, errors.New("spec.provider is not set and the CloudAccount has no default provider")
	}

	region := cmp.Or(spec.Region, defaults.Region)
	if region == "" {
		return empty, errors.New("spec.region is not set and the CloudAccount has no default region")
	}
	if spec.Region == "" {
		if err := validateRegion(account, provider, region,
			fmt.Sprintf("spec.defaults.region of CloudAccount %q", account.Name)); err != nil {
			return empty, err
		}
	} else if err := validateRegion(account, provider, region, "spec.region"); err != nil {
		return empty, fmt.Errorf("%w. Set spec.region to one of those; nothing was created", err)
	}

	channel := cmp.Or(spec.ReleaseChannel, defaults.ReleaseChannel, account.Status.DefaultReleaseChannel)
	if channel == "" {
		return empty, errors.New("no release channel set and the CloudAccount has not reported a default yet")
	}
	channelField := "spec.releaseChannel"
	switch {
	case spec.ReleaseChannel != "":
	case defaults.ReleaseChannel != "":
		channelField = fmt.Sprintf("spec.defaults.releaseChannel of CloudAccount %q", account.Name)
	default:
		channelField = "the account's default release channel"
	}
	if err := validateReleaseChannel(account, channel, channelField); err != nil {
		return empty, err
	}

	allowedIPs := spec.Networking.AllowedIPs
	if len(allowedIPs) == 0 {
		allowedIPs = defaults.AllowedIPs
	}
	if len(allowedIPs) == 0 {
		return empty, errors.New("no allowed IPs set: RavenDB Cloud requires at least one entry in spec.networking.allowedIPs or the CloudAccount defaults")
	}

	return placement{provider: provider, region: region, channel: channel, allowedIPs: allowedIPs}, nil
}
