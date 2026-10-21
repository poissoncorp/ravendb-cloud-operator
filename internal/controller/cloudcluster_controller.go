package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
)

const (
	// clusterFinalizer holds the object until its deletion policy is honoured.
	clusterFinalizer = "cloud.ravendb.net/finalizer"

	// pendingRequeue is how long to wait on a prerequisite, such as a
	// CloudAccount that is not Ready yet.
	pendingRequeue = 20 * time.Second

	// quotaRetryInterval is how long to wait before retrying a create the
	// account had no room for. Slow, because the fix is somebody terminating
	// another cluster.
	quotaRetryInterval = 10 * time.Minute
)

// CloudClusterReconciler drives one RavenDB Cloud cluster towards spec.
//
// A cluster this object created and one it found already running share every
// step after binding. The one difference is the default management policy,
// which is Observe for a found cluster.
type CloudClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clients  *cloudclients.Cache
	Recorder events.EventRecorder

	// warned remembers which long waits already raised their Warning.
	warned sync.Map
}

// No create and no delete: the operator never makes or removes a CloudCluster,
// and delete would let a compromised operator remove the objects whose
// deletionPolicy decides whether a paid database is terminated.
//
// patch, not update, is the finalizer: an update sends back a spec
// round-tripped through Go. The finalizers subresource is what
// OwnerReferencesPermissionEnforcement demands before the connection Secret's
// owner reference may set blockOwnerDeletion. The Secret goes out as a
// server-side apply, which is a PATCH, so it needs no update either.
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudclusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudclusters/status,verbs=get;update
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;patch

// Reconcile brings one CloudCluster in line with RavenDB Cloud.
func (r *CloudClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster v1alpha1.CloudCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Before anything is computed into status, as on CloudAccount: the patch
	// answer replaces the object in hand. Stored before any call to RavenDB
	// Cloud, so no cluster is ever created without it.
	if cluster.DeletionTimestamp.IsZero() {
		if err := r.addFinalizer(ctx, &cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	original := cluster.DeepCopy()
	cluster.Status.ObservedGeneration = cluster.Generation
	cluster.Status.ManagementPolicy = cluster.EffectiveManagementPolicy()

	// Terminating needs a credential too, so the account comes first.
	account, cloud, result, err := r.resolveAccount(ctx, &cluster)
	if err != nil || account == nil {
		if !cluster.DeletionTimestamp.IsZero() {
			return r.deleteWithoutAccount(ctx, original, &cluster, result, err)
		}
		return r.finish(ctx, original, &cluster, result, err)
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, original, &cluster, cloud)
	}

	return r.reconcileNormal(ctx, original, &cluster, account, cloud)
}

// resolveAccount returns the cluster's CloudAccount and its client, or says on
// the object what it waits for and when it looks again.
func (r *CloudClusterReconciler) resolveAccount(ctx context.Context, cluster *v1alpha1.CloudCluster) (*v1alpha1.CloudAccount, cloudapi.Client, ctrl.Result, error) {
	ref := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.AccountRef.Name}
	account, cloud, wait, err := usableAccount(ctx, r.Client, r.Clients, ref)
	switch {
	case err != nil:
		return nil, nil, ctrl.Result{}, err
	case wait == nil:
		return account, cloud, ctrl.Result{}, nil
	case wait.keyMissing && !cluster.DeletionTimestamp.IsZero():
		// Deleting the Secret together with the account and its clusters is
		// an ordinary teardown, and the client built before still holds the
		// key a Terminate needs.
		if last, ok := r.Clients.LastFor(account); ok {
			return account, last, ctrl.Result{}, nil
		}
	}

	message := wait.message
	if wait.absent {
		message += " " + missingAccountWayOut(cluster)
	}
	r.waitForAccount(ctx, cluster, wait.reason, message)
	return nil, nil, ctrl.Result{RequeueAfter: pendingRequeue}, nil
}

// missingAccountWayOut says what a missing CloudAccount means for this object.
// accountRef cannot be edited, so before anything was created the way out of a
// typo is to apply the object again.
func missingAccountWayOut(cluster *v1alpha1.CloudCluster) string {
	if cluster.Status.ProductID == "" {
		return "If the name is wrong, delete this object and apply it again with the right spec.accountRef; nothing was created."
	}
	return "The cluster keeps running, and the operator picks it up again once the account exists."
}

// waitForAccount says what the cluster is waiting for in its account, unless
// the object is being deleted: then the deletion says what it waits for, and
// saying both would log two transitions on every pass.
func (r *CloudClusterReconciler) waitForAccount(ctx context.Context, cluster *v1alpha1.CloudCluster, reason, message string) {
	if !cluster.DeletionTimestamp.IsZero() {
		return
	}
	r.ready(ctx, cluster, reason, message)
}

// reconcileNormal is the steady-state path: bind, read, publish, converge.
func (r *CloudClusterReconciler) reconcileNormal(
	ctx context.Context,
	original, cluster *v1alpha1.CloudCluster,
	account *v1alpha1.CloudAccount,
	cloud cloudapi.Client,
) (ctrl.Result, error) {
	productID, result, err := r.bind(ctx, original, cluster, account, cloud)
	if err != nil || productID == "" {
		return r.finish(ctx, original, cluster, result, err)
	}
	r.recordBinding(ctx, cluster, productID)

	observed, err := cloud.GetCluster(ctx, productID)
	if err != nil {
		result, err := r.reportUnreadable(ctx, cluster, productID, err)
		return r.finish(ctx, original, cluster, result, err)
	}
	r.recordObserved(cluster, observed)

	if serving, result := r.reportUnlessServing(ctx, cluster, observed); !serving {
		return r.finish(ctx, original, cluster, result, nil)
	}

	// Before converging, so an application waiting on the Secret does not wait
	// behind a change as well.
	published, err := r.publishConnectionSecret(ctx, cluster, observed, cloud)
	if err != nil {
		result, err := r.reportSecretFailure(ctx, cluster, err)
		return r.finish(ctx, original, cluster, result, err)
	}

	// Set before converging, which may say something more pressing: Updating,
	// or KeyRejected when a change was refused for the key.
	r.ready(ctx, cluster, published.reason, published.message)
	result, applied, err := r.converge(ctx, cluster, observed, cloud)
	if applied != nil {
		r.ready(ctx, cluster, v1alpha1.ReasonUpdating, sentence(applied.event))
	}
	return r.finish(ctx, original, cluster, result, err)
}

// recordBinding stores the cloud identity this object just acquired, and the
// policy that identity decides.
func (r *CloudClusterReconciler) recordBinding(ctx context.Context, cluster *v1alpha1.CloudCluster, productID string) {
	if cluster.Status.ProductID == "" {
		log.FromContext(ctx).Info("bound to a RavenDB Cloud cluster",
			"productID", productID, "adopted", cluster.Status.Adopted,
			"policy", cluster.EffectiveManagementPolicy())
	}
	cluster.Status.ProductID = productID
	cluster.Status.ManagementPolicy = cluster.EffectiveManagementPolicy()
}

// reportUnreadable turns a failed read of the bound cluster into a result. A
// 404 is reported rather than re-created, which could double a bill.
func (r *CloudClusterReconciler) reportUnreadable(
	ctx context.Context, cluster *v1alpha1.CloudCluster, productID string, err error,
) (ctrl.Result, error) {
	if cloudapi.IsNotFound(err) {
		forgetChanges(cluster)
		r.ready(ctx, cluster, v1alpha1.ReasonDeletedInCloud, fmt.Sprintf(
			"RavenDB Cloud has no cluster %s any more: it was deleted outside this operator. "+
				"Delete this object; to get a new cluster, apply it again.", productID))
		return ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}, nil
	}
	return r.cloudUnavailable(ctx, cluster, "read RavenDB Cloud cluster "+productID, err)
}

// cloudUnavailable reports a call that got no usable answer, which says nothing
// about the cluster itself, so Ready goes Unknown. A failure another attempt
// could fix goes back to controller-runtime for backoff; one it could not waits
// for the ordinary poll.
func (r *CloudClusterReconciler) cloudUnavailable(
	ctx context.Context, cluster *v1alpha1.CloudCluster, doing string, err error,
) (ctrl.Result, error) {
	if cloudapi.IsUnauthorized(err) {
		// The account verifies on its refresh interval, so its Ready may still
		// say True: this message cannot point at the account's.
		r.ready(ctx, cluster, v1alpha1.ReasonKeyRejected, fmt.Sprintf(
			"Could not %s: RavenDB Cloud does not accept the API key CloudAccount %q uses. The key is "+
				"mistyped, deactivated or past its expiry date. Create a key in the RavenDB Cloud portal and put "+
				"it in the Secret that account's spec.apiKeySecretRef names; the account verifies it again as soon "+
				"as the Secret changes.",
			doing, cluster.Spec.AccountRef.Name))
		return ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}, nil
	}

	message := sentence(fmt.Sprintf("Could not %s: %s", doing, cloudSaid(err)))
	if cloudapi.IsRetryable(err) {
		r.ready(ctx, cluster, v1alpha1.ReasonCloudUnreachable, message+" Retrying.")
		return ctrl.Result{}, err
	}
	r.ready(ctx, cluster, v1alpha1.ReasonUnexpectedResponse, message+" Trying again at the next poll.")
	return ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}, nil
}

// recordObserved copies the live cluster into status.
func (r *CloudClusterReconciler) recordObserved(cluster *v1alpha1.CloudCluster, observed *cloudapi.Cluster) {
	applyObservedState(cluster, observed)
	now := metav1.Now()
	cluster.Status.LastSyncTime = &now
}

// reportUnlessServing describes a cluster that cannot be used yet and answers
// whether it is serving after all. A transitional status polls fast, because
// it is about to change on its own.
func (r *CloudClusterReconciler) reportUnlessServing(
	ctx context.Context, cluster *v1alpha1.CloudCluster, observed *cloudapi.Cluster,
) (serving bool, result ctrl.Result) {
	poll := cluster.EffectivePollInterval()
	soon := cluster.EffectiveTransitionalInterval()

	switch observed.Status {
	case cloudapi.StatusActive:
		return true, ctrl.Result{}
	case cloudapi.StatusCreating:
		r.ready(ctx, cluster, v1alpha1.ReasonCreating, "RavenDB Cloud is creating the cluster.")
		return false, ctrl.Result{RequeueAfter: soon}
	case cloudapi.StatusDeployingChanges:
		r.ready(ctx, cluster, v1alpha1.ReasonUpdating, "RavenDB Cloud is applying a change.")
		return false, ctrl.Result{RequeueAfter: soon}
	case cloudapi.StatusTerminating, cloudapi.StatusTerminated:
		// Not the operator's own terminate: a deleted object takes the
		// deletion path.
		forgetChanges(cluster)
		r.ready(ctx, cluster, v1alpha1.ReasonDeletedInCloud, deletedInCloudMessage(observed))
		return false, ctrl.Result{RequeueAfter: poll}
	case cloudapi.StatusAwaitingPayment:
		r.ready(ctx, cluster, v1alpha1.ReasonAwaitingPayment, fmt.Sprintf(
			"RavenDB Cloud is waiting for payment for this cluster. Complete it in the RavenDB Cloud portal; "+
				"the operator notices within %s.", poll))
		return false, ctrl.Result{RequeueAfter: poll}
	case cloudapi.StatusError:
		// RavenDB Cloud sets Error only when provisioning fails, and opens a
		// support request at the same time.
		r.ready(ctx, cluster, v1alpha1.ReasonCreationFailed, fmt.Sprintf(
			"RavenDB Cloud could not create this cluster and opened a support request for it. Nothing in "+
				"this manifest can fix that; RavenDB support follows up, and the operator looks again every %s.", poll))
		return false, ctrl.Result{RequeueAfter: poll}
	}
	r.ready(ctx, cluster, v1alpha1.ReasonUnexpectedResponse, fmt.Sprintf(
		"RavenDB Cloud reports status %q, which this version of the operator does not know. "+
			"Upgrading the operator should fix this; it looks again every %s.", observed.Status, poll))
	return false, ctrl.Result{RequeueAfter: poll}
}

// forgetChanges drops Synced from a cluster that is gone, so CHANGES does not
// claim Applied beside DeletedInCloud.
func forgetChanges(cluster *v1alpha1.CloudCluster) {
	meta.RemoveStatusCondition(&cluster.Status.Conditions, v1alpha1.ConditionSynced)
}

// deletedInCloudMessage says the cluster went away without this object, and
// what applying the object again will run into: a terminated cluster keeps its
// name until it is hidden in the portal, because RavenDB Cloud counts every
// product not hidden when it decides a name is taken.
func deletedInCloudMessage(observed *cloudapi.Cluster) string {
	what := "was terminated"
	if observed.Status == cloudapi.StatusTerminating {
		what = "is being terminated"
	}
	return fmt.Sprintf(
		"RavenDB Cloud cluster %s %s outside this operator. Delete this object; to get a new cluster, apply it "+
			"again, after hiding the terminated one in the RavenDB Cloud portal, since it keeps the name %q until then.",
		observed.ID, what, observed.DisplayName)
}

// reportSecretFailure decides what a failed publication costs the object. The
// cluster serves either way, but Running means published too.
func (r *CloudClusterReconciler) reportSecretFailure(
	ctx context.Context, cluster *v1alpha1.CloudCluster, err error,
) (ctrl.Result, error) {
	poll := ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}
	name := cluster.ConnectionSecretName()

	var noAddresses *noEndpointsError
	switch {
	case errors.Is(err, errSecretNotOurs):
		// Still true next pass, so backoff would retry it forever.
		r.ready(ctx, cluster, v1alpha1.ReasonNameTaken, fmt.Sprintf(
			"The cluster is Active, but Secret %q already exists and was not created for this cluster. "+
				"Set spec.connectionSecret.name to a name nothing else owns, or delete that Secret.", name))
		return poll, nil
	case errors.As(err, &noAddresses):
		r.ready(ctx, cluster, v1alpha1.ReasonUnexpectedResponse, noAddresses.Error())
		return poll, nil
	case errors.Is(err, errCertificateDownload):
		return r.cloudUnavailable(ctx, cluster,
			fmt.Sprintf("download the client certificate for Secret %q", name), err)
	}

	// Not logged: controller-runtime logs the returned error.
	r.ready(ctx, cluster, v1alpha1.ReasonSecretNotWritten, sentence(fmt.Sprintf(
		"The cluster is Active, but Secret %q could not be written: %s", name, err.Error()))+" Retrying.")
	return ctrl.Result{}, err
}

// converge carries the desired shape through where it is allowed to, and says
// on Synced what it did or left alone. It hands back the change it applied,
// because that is the pass on which Ready says Updating. A refused or postponed
// change leaves the cluster serving, so none of this touches Ready.
func (r *CloudClusterReconciler) converge(
	ctx context.Context,
	cluster *v1alpha1.CloudCluster,
	observed *cloudapi.Cluster,
	cloud cloudapi.Client,
) (ctrl.Result, *shapeDifference, error) {
	poll := ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}

	// Asking again would get the same refusal, one call each poll, for as long
	// as the spec says the same thing.
	if refusalStands(cluster) && stillDiffers(cluster, observed, cloud) {
		return poll, nil, nil
	}

	pass, err := r.reconcileDrift(ctx, cluster, observed, cloud)
	if err != nil {
		result, err := r.changeFailure(ctx, cluster, pass.failed, err)
		return result, nil, err
	}
	if pass.applied != nil {
		r.synced(ctx, cluster, v1alpha1.ReasonApplying, sentence(pass.applied.event))
		return ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}, pass.applied, nil
	}
	if pass.refused != nil {
		return r.changeRefused(ctx, cluster, pass.refused, pass.refusal), nil, nil
	}

	switch {
	case len(pass.deferred) > 0:
		message := strings.Join(pass.deferred, " ")
		if len(pass.drift) > 0 {
			message += " Also outstanding: " + sentence(strings.Join(pass.drift, "; "))
		}
		r.synced(ctx, cluster, v1alpha1.ReasonWaiting, message)
	case len(pass.drift) > 0:
		r.synced(ctx, cluster, v1alpha1.ReasonNotApplied, sentence(strings.Join(pass.drift, "; ")))
	default:
		r.synced(ctx, cluster, v1alpha1.ReasonApplied, "Everything this manifest sets matches RavenDB Cloud.")
	}
	return poll, nil, nil
}

// refusalStands reports whether RavenDB Cloud already refused a change this
// exact spec asks for. Synced's observedGeneration is the spec it was written
// for, so any spec edit lets the operator ask again.
func refusalStands(cluster *v1alpha1.CloudCluster) bool {
	synced := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionSynced)
	return synced != nil && synced.Reason == v1alpha1.ReasonRefused && synced.ObservedGeneration == cluster.Generation
}

// stillDiffers reports whether the live cluster is still some way from the
// spec, so a refusal stops standing once the change is made another way, in
// the portal for instance. Comparing sends nothing: shapeDifferences only
// builds the changes.
func stillDiffers(cluster *v1alpha1.CloudCluster, observed *cloudapi.Cluster, cloud cloudapi.Client) bool {
	return len(shapeDifferences(&cluster.Spec, observed, cloud)) > 0
}

// changeFailure reports a change that could not be sent and is retried. The
// cluster still serves, so Synced carries it, unless the failure was the key,
// which belongs on Ready.
func (r *CloudClusterReconciler) changeFailure(
	ctx context.Context, cluster *v1alpha1.CloudCluster, change *shapeDifference, err error,
) (ctrl.Result, error) {
	what := lowerFirst(change.event)
	if cloudapi.IsUnauthorized(err) {
		return r.cloudUnavailable(ctx, cluster, what, err)
	}
	r.synced(ctx, cluster, v1alpha1.ReasonApplying,
		sentence(fmt.Sprintf("Could not send a change (%s): %s", what, cloudSaid(err)))+" Retrying.")
	return ctrl.Result{}, err
}

// changeRefused reports a change RavenDB Cloud refused for good. "the spec",
// not "the object": a label does not move the generation refusalStands
// compares.
func (r *CloudClusterReconciler) changeRefused(
	ctx context.Context, cluster *v1alpha1.CloudCluster, change *shapeDifference, err error,
) ctrl.Result {
	r.synced(ctx, cluster, v1alpha1.ReasonRefused,
		sentence(fmt.Sprintf("RavenDB Cloud refused a change (%s): %s", lowerFirst(change.event), cloudSaid(err)))+
			" The operator does not ask again until the spec changes.")
	return ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}
}

func applyObservedState(cluster *v1alpha1.CloudCluster, observed *cloudapi.Cluster) {
	cluster.Status.CloudStatus = string(observed.Status)
	cluster.Status.DisplayName = observed.DisplayName
	cluster.Status.Provider = v1alpha1.Provider(observed.Provider)
	cluster.Status.Region = observed.Region
	cluster.Status.Tier = v1alpha1.Tier(observed.Tier)
	cluster.Status.InstanceType = observed.InstanceType
	cluster.Status.ReleaseChannel = observed.ReleaseChannel
	cluster.Status.URLs = observed.URLs
	cluster.Status.NodeTags = observed.NodeTags
	cluster.Status.Nodes = observed.NodeCount()
	cluster.Status.AllowedIPs = observed.AllowedIPs
	if len(observed.URLs) > 0 {
		cluster.Status.StudioURL = studioURL(observed.URLs[0])
	}
	cluster.Status.Hardware = &v1alpha1.HardwareStatus{
		CPUs:        observed.Hardware.CPUs,
		CPUPriority: observed.Hardware.CPUPriority,
		RAMMB:       int32(math.Round(observed.Hardware.RAMGiB * 1024)),
		Storage: &v1alpha1.StorageStatus{
			SizeGB:         observed.Hardware.Storage.SizeGiB,
			Type:           v1alpha1.StorageType(observed.Hardware.Storage.Type),
			IOPS:           observed.Hardware.Storage.IOPS,
			ThroughputMBps: throughputFromAPI(observed.Hardware.Storage.ThroughputMBps),
		},
	}
}

// createFailure decides whether a failed create is worth trying again.
//
// A 4xx will be answered the same way for as long as the spec says the same
// thing, and backoff would retry it forever, so a refusal waits for a spec
// change. A change to an existing cluster is
// changeFailure's.
func (r *CloudClusterReconciler) createFailure(ctx context.Context, cluster *v1alpha1.CloudCluster, err error) (ctrl.Result, error) {
	if cloudapi.IsRetryable(err) || cloudapi.IsUnauthorized(err) {
		// When the create may have landed, provisionAttemptedAt is still
		// stamped, so the next pass finds that cluster by name rather than
		// making a second.
		return r.cloudUnavailable(ctx, cluster, "create this cluster", err)
	}

	// The quota can lift without anybody touching this object, and nothing the
	// operator watches sees it happen, so this one keeps a slow timer.
	if cloudapi.IsQuotaReached(err) {
		r.ready(ctx, cluster, v1alpha1.ReasonQuotaReached,
			sentence("RavenDB Cloud refused to create this cluster because the account is at its product quota: "+
				cloudSaid(err))+" Terminate a cluster or raise the quota; this retries on its own every "+
				quotaRetryInterval.String()+".")
		return ctrl.Result{RequeueAfter: quotaRetryInterval}, nil
	}

	// Not "until the spec changes": a create is judged on fields the CRD marks
	// immutable, so that would name a fix the API server refuses.
	message := sentence("RavenDB Cloud refused to create this cluster: " + cloudSaid(err))
	if refusedField(err, "displayName") {
		// The lookup found no live cluster, so a terminated one holds the name.
		message += " A terminated cluster keeps its name until it is hidden in the RavenDB Cloud portal."
	}
	message += " This is not retried on a timer; the operator acts again when the object changes. " +
		"Most spec fields cannot change once set, so a refusal naming one of those needs the object deleted and " +
		"applied again; nothing was created."
	r.ready(ctx, cluster, v1alpha1.ReasonRefused, message)
	return ctrl.Result{}, nil
}

// writeStatusNow persists status in the middle of a pass and moves the pass's
// starting point with it, so the write that ends the pass compares with what is
// stored, not with what was read. Otherwise a pass that ends where it began
// writes nothing and leaves the mid-pass status behind.
func (r *CloudClusterReconciler) writeStatusNow(ctx context.Context, original, cluster *v1alpha1.CloudCluster) error {
	if err := r.Status().Update(ctx, cluster); err != nil {
		return err
	}
	original.Status = *cluster.Status.DeepCopy()
	return nil
}

func (r *CloudClusterReconciler) finish(
	ctx context.Context, original, cluster *v1alpha1.CloudCluster, result ctrl.Result, err error,
) (ctrl.Result, error) {
	return writeStatus(ctx, r.Client, cluster, &original.Status, &cluster.Status, result, err)
}

// SetupWithManager wires the controller and its owned Secret.
func (r *CloudClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&v1alpha1.CloudCluster{}, productIDIndex, indexClustersByProductID); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.CloudCluster{}, builder.WithPredicates(ignoreStatusOnlyUpdates())).
		// Deletions only. Creates and updates are this controller's own writes,
		// and reacting to them started a second pass against a cache that had
		// not caught up with its status. An edited Secret is put right at the
		// next poll.
		Owns(&corev1.Secret{}, builder.WithPredicates(deletionsOnly()), builder.OnlyMetadata).
		WithOptions(controllerOptions()).
		Named("cloudcluster").
		Complete(r)
}
