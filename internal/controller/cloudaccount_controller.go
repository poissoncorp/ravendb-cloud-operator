package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
)

// secretRefIndex lets a Secret change wake the CloudAccounts that use it, so a
// rotated API key takes effect at once.
const secretRefIndex = ".spec.apiKeySecretRef.name"

// accountRefIndex answers "which CloudClusters use this account", the one
// question the deletion path has to be sure about.
const accountRefIndex = ".spec.accountRef.name"

// accountFinalizer holds a CloudAccount open while a CloudCluster still names
// it: under deletionPolicy Terminate, a cluster needs the credential to finish
// deleting.
const accountFinalizer = "cloud.ravendb.net/finalizer"

const (
	// credentialWaitRequeue is how long to wait for a Secret that has not
	// appeared yet. The Secret watch normally beats it; the timer covers a
	// namespace the operator cannot watch.
	credentialWaitRequeue = 30 * time.Second

	// credentialRejectedRequeue is how long to wait after RavenDB Cloud rejects
	// the key. Only a human editing the Secret fixes that, and the Secret watch
	// wakes the account when they do.
	credentialRejectedRequeue = 5 * time.Minute

	// catalogueRetryRequeue is how soon to retry a failed catalogue refresh
	// when there is no earlier catalogue to fall back on.
	catalogueRetryRequeue = time.Minute
)

// CloudAccountReconciler verifies a RavenDB Cloud credential and caches the
// region and release channel lists in status.
//
// It does nothing else, so one bad credential produces one clear failure rather
// than a confusing one on every cluster that references it.
type CloudAccountReconciler struct {
	client.Client
	Clients  *cloudclients.Cache
	Recorder events.EventRecorder

	// warned remembers which long waits already raised their Warning.
	warned sync.Map
}

// No create, no delete, and no verb that edits a spec, so nothing here can point
// an account at a different credential Secret. patch is the finalizer, a patch
// rather than an update because an update sends back the whole spec. The read
// on cloudclusters is the deletion check.
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudaccounts,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudaccounts/status,verbs=get;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile verifies the credential and refreshes the cached lists.
func (r *CloudAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var account v1alpha1.CloudAccount
	if err := r.Get(ctx, req.NamespacedName, &account); err != nil {
		if apierrors.IsNotFound(err) {
			// So a deleted account's credential does not stay in memory.
			r.Clients.Forget(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !account.DeletionTimestamp.IsZero() {
		original := account.DeepCopy()
		account.Status.ObservedGeneration = account.Generation
		return r.reconcileDelete(ctx, original, &account)
	}

	// Before anything is computed into status: the patch response replaces the
	// object in hand, and would silently roll back status set before it. No
	// requeue, unlike the CloudCluster path: nothing external is made here.
	if err := r.addFinalizer(ctx, &account); err != nil {
		return ctrl.Result{}, err
	}

	original := account.DeepCopy()
	account.Status.ObservedGeneration = account.Generation

	cloud, err := r.Clients.ClientFor(ctx, &account)
	if err != nil {
		result, err := r.keyUnreadable(ctx, &account, err)
		return r.finish(ctx, original, &account, result, err)
	}

	info, err := cloud.GetAccountInfo(ctx)
	if err != nil {
		result, err := r.verificationFailed(ctx, &account, err)
		return r.finish(ctx, original, &account, result, err)
	}

	account.Status.DomainName = info.DomainName
	now := metav1.Now()
	account.Status.LastVerifiedTime = &now
	r.ready(ctx, &account, v1alpha1.ReasonVerified,
		fmt.Sprintf("RavenDB Cloud accepted the API key for account %s.", info.DomainName))

	requeue := r.refreshCatalogue(ctx, cloud, original, &account)
	return r.finish(ctx, original, &account, ctrl.Result{RequeueAfter: requeue}, nil)
}

// addFinalizer adds the finalizer as a merge patch of the difference, never an
// Update: see addFinalizer on the CloudCluster reconciler.
func (r *CloudAccountReconciler) addFinalizer(ctx context.Context, account *v1alpha1.CloudAccount) error {
	if controllerutil.ContainsFinalizer(account, accountFinalizer) {
		return nil
	}
	patch := lockedPatch(account)
	controllerutil.AddFinalizer(account, accountFinalizer)
	return r.Patch(ctx, account, patch)
}

// keyUnreadable reports an API key the operator could not read. A missing
// Secret usually arrives moments after the account, so it waits for the watch
// rather than burning retries.
func (r *CloudAccountReconciler) keyUnreadable(ctx context.Context, account *v1alpha1.CloudAccount, err error) (ctrl.Result, error) {
	var missing *cloudclients.CredentialMissingError
	if errors.As(err, &missing) {
		r.ready(ctx, account, v1alpha1.ReasonKeyMissing, sentence(missing.Error()))
		return ctrl.Result{RequeueAfter: credentialWaitRequeue}, nil
	}
	r.ready(ctx, account, v1alpha1.ReasonKeyMissing, sentence("Could not read the API key: "+err.Error())+" Retrying.")
	return ctrl.Result{}, err
}

// verificationFailed reports a key RavenDB Cloud did not confirm.
func (r *CloudAccountReconciler) verificationFailed(ctx context.Context, account *v1alpha1.CloudAccount, err error) (ctrl.Result, error) {
	switch {
	case cloudapi.IsUnauthorized(err):
		answered := cloudSaid(err)
		var apiErr *cloudapi.APIError
		if errors.As(err, &apiErr) {
			answered = apiErr.Status()
		}
		r.ready(ctx, account, v1alpha1.ReasonKeyRejected, keyRejectedMessage(account, answered))
		return ctrl.Result{RequeueAfter: credentialRejectedRequeue}, nil
	case cloudapi.IsRetryable(err):
		r.ready(ctx, account, v1alpha1.ReasonCloudUnreachable,
			sentence("Could not verify the API key: "+cloudSaid(err))+" Retrying.")
		return ctrl.Result{}, err
	}
	// A no that does not say the key is wrong, such as a 404 from something
	// that is not RavenDB Cloud. It will say the same next time.
	r.ready(ctx, account, v1alpha1.ReasonUnexpectedResponse,
		sentence("Could not verify the API key: "+cloudSaid(err))+
			" Trying again in "+credentialRejectedRequeue.String()+".")
	return ctrl.Result{RequeueAfter: credentialRejectedRequeue}, nil
}

// refreshCatalogue refreshes the mirrored regions and release channels when
// they are due, and returns when to look again. A failed refresh does not make
// the account unusable, so it is logged rather than put on Ready.
func (r *CloudAccountReconciler) refreshCatalogue(
	ctx context.Context, cloud cloudapi.Client, original, account *v1alpha1.CloudAccount,
) time.Duration {
	interval := account.EffectiveRefreshInterval()
	if catalogueMoved(original, account) {
		// Lists from another account are worse than none: an empty list lets
		// RavenDB Cloud judge, and makes the next pass refresh again.
		account.Status.Providers = nil
		account.Status.ReleaseChannels = nil
		account.Status.DefaultReleaseChannel = ""
	}
	if !needsRefresh(account, interval) {
		return interval
	}
	err := r.refresh(ctx, cloud, account)
	if err == nil {
		return interval
	}
	// Without a default channel, no cluster that leaves spec.releaseChannel out
	// can be created, and an hour is too long to wait for that.
	requeue := interval
	if len(account.Status.ReleaseChannels) == 0 {
		requeue = catalogueRetryRequeue
	}
	log.FromContext(ctx).Error(err, "refreshing the regions and release channels RavenDB Cloud offers",
		"kept", len(account.Status.ReleaseChannels) > 0, "retryIn", requeue)
	return requeue
}

// reconcileDelete releases the account once nothing depends on it. No timer
// while it waits: the CloudCluster watch wakes it when the last one goes.
func (r *CloudAccountReconciler) reconcileDelete(
	ctx context.Context, original, account *v1alpha1.CloudAccount,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(account, accountFinalizer) {
		return ctrl.Result{}, nil
	}

	users, err := r.clustersUsing(ctx, account)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(users) > 0 {
		message := fmt.Sprintf(
			"%d CloudCluster(s) still reference this account (%s). Delete them first: without this credential they cannot reconcile, and one with deletionPolicy Terminate could not terminate its cluster either.",
			len(users), strings.Join(users, ", "))
		r.markInUse(ctx, account, message)
		return r.finish(ctx, original, account, ctrl.Result{}, nil)
	}

	r.Clients.Forget(account.Namespace, account.Name)
	patch := lockedPatch(account)
	controllerutil.RemoveFinalizer(account, accountFinalizer)
	if err := r.Patch(ctx, account, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// clustersUsing names the CloudClusters that reference this account, sorted so
// the message does not reshuffle between reconciles.
func (r *CloudAccountReconciler) clustersUsing(
	ctx context.Context, account *v1alpha1.CloudAccount,
) ([]string, error) {
	var list v1alpha1.CloudClusterList
	if err := r.List(ctx, &list,
		client.InNamespace(account.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(accountRefIndex, account.Name)},
	); err != nil {
		return nil, fmt.Errorf("listing the CloudClusters using %s: %w", account.Name, err)
	}
	names := make([]string, 0, len(list.Items))
	for _, cluster := range list.Items {
		names = append(names, cluster.Name)
	}
	slices.Sort(names)
	return names, nil
}

// catalogueMoved reports whether the mirrored catalogue came from a different
// account or control plane than the one this pass verified against.
//
// The credential is verified every pass and the catalogue only on its
// interval, so without this a new Secret or key would sit beside the
// old account's regions and default channel for up to an hour. A spec edit
// that moved nothing costs one refresh.
func catalogueMoved(original, account *v1alpha1.CloudAccount) bool {
	if original.Status.RefreshedAt == nil {
		return false
	}
	return original.Status.ObservedGeneration != account.Generation ||
		original.Status.DomainName != account.Status.DomainName
}

func needsRefresh(account *v1alpha1.CloudAccount, interval time.Duration) bool {
	return len(account.Status.Providers) == 0 || len(account.Status.ReleaseChannels) == 0 ||
		account.Status.RefreshedAt == nil || time.Since(account.Status.RefreshedAt.Time) >= interval
}

func (r *CloudAccountReconciler) refresh(ctx context.Context, cloud cloudapi.Client, account *v1alpha1.CloudAccount) error {
	channels, err := cloud.ListReleaseChannels(ctx)
	if err != nil {
		return err
	}
	account.Status.DefaultReleaseChannel = channels.Default
	account.Status.ReleaseChannels = nil
	for _, ch := range channels.Channels {
		account.Status.ReleaseChannels = append(account.Status.ReleaseChannels, v1alpha1.ReleaseChannelStatus{
			Name:         ch.Name,
			RavenVersion: ch.RavenVersion,
			BuildType:    ch.BuildType,
		})
	}

	var providers []v1alpha1.ProviderRegions
	for _, provider := range []v1alpha1.Provider{v1alpha1.ProviderAWS, v1alpha1.ProviderAzure, v1alpha1.ProviderGCP} {
		regions, err := cloud.ListRegions(ctx, cloudapi.Provider(provider))
		if err != nil {
			return err
		}
		providers = append(providers, v1alpha1.ProviderRegions{Provider: provider, Regions: regions})
	}
	account.Status.Providers = providers
	refreshed := metav1.Now()
	account.Status.RefreshedAt = &refreshed
	return nil
}

// keyRejectedMessage says what a rejected key can mean and how to replace it.
//
// RavenDB Cloud answers an unknown, deactivated and expired key alike, a 401
// with no body, so the message lists all three. The public API has no endpoint
// to create a key (it serves account, metadata and products only), so the fix
// is always a new key from the portal.
func keyRejectedMessage(account *v1alpha1.CloudAccount, answered string) string {
	return fmt.Sprintf(
		"RavenDB Cloud answered %s: it does not accept the API key in Secret %s/%s, key %q. "+
			"The key is mistyped, deactivated or past its expiry date. Create a key in the RavenDB Cloud portal "+
			"and put it in that Secret; the account verifies it again as soon as the Secret changes.",
		answered, account.Namespace, account.Spec.APIKeySecretRef.Name, account.EffectiveSecretKey())
}

// markInUse says what is holding a deleted account open, and leaves Ready's
// status as the last verification left it: being deleted does not change
// whether the key works.
func (r *CloudAccountReconciler) markInUse(ctx context.Context, account *v1alpha1.CloudAccount, message string) {
	if !meta.IsStatusConditionTrue(account.Status.Conditions, v1alpha1.ConditionReady) {
		message += " The credential is not usable either, so those clusters are waiting on it too."
	}
	r.ready(ctx, account, v1alpha1.ReasonInUse, message)
}

func (r *CloudAccountReconciler) finish(
	ctx context.Context, original, account *v1alpha1.CloudAccount, result ctrl.Result, err error,
) (ctrl.Result, error) {
	return writeStatus(ctx, r.Client, account, &original.Status, &account.Status, result, err)
}

// indexClustersByAccountRef indexes a CloudCluster by the account it names.
func indexClustersByAccountRef(obj client.Object) []string {
	cluster, ok := obj.(*v1alpha1.CloudCluster)
	if !ok || cluster.Spec.AccountRef.Name == "" {
		return nil
	}
	return []string{cluster.Spec.AccountRef.Name}
}

// indexAccountsBySecretRef indexes a CloudAccount by the name of the Secret it
// reads its API key from.
func indexAccountsBySecretRef(obj client.Object) []string {
	account, ok := obj.(*v1alpha1.CloudAccount)
	if !ok || account.Spec.APIKeySecretRef.Name == "" {
		return nil
	}
	return []string{account.Spec.APIKeySecretRef.Name}
}

// SetupWithManager wires the controller, including the Secret watch that makes
// key rotation take effect at once.
func (r *CloudAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&v1alpha1.CloudAccount{}, secretRefIndex, indexAccountsBySecretRef); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&v1alpha1.CloudCluster{}, accountRefIndex, indexClustersByAccountRef); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.CloudAccount{}, builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.accountsUsingSecret), builder.OnlyMetadata).
		// Clusters arriving and leaving change who holds the account open.
		Watches(&v1alpha1.CloudCluster{},
			handler.EnqueueRequestsFromMapFunc(r.accountAwaitingCluster),
			builder.WithPredicates(arrivalsAndDepartures())).
		WithOptions(controllerOptions()).
		Named("cloudaccount").
		Complete(r)
}

// accountAwaitingCluster enqueues the account a cluster is holding open, and
// only while that account is waiting to be deleted.
func (r *CloudAccountReconciler) accountAwaitingCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*v1alpha1.CloudCluster)
	if !ok || cluster.Spec.AccountRef.Name == "" {
		return nil
	}
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.AccountRef.Name}

	var account v1alpha1.CloudAccount
	if err := r.Get(ctx, key, &account); err != nil {
		// A map function can only return nothing, so a failure other than the
		// account being gone is logged.
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "reading the CloudAccount a cluster names", "account", key)
		}
		return nil
	}
	if account.DeletionTimestamp.IsZero() {
		return nil
	}
	return []reconcile.Request{{NamespacedName: key}}
}

func (r *CloudAccountReconciler) accountsUsingSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.CloudAccountList
	if err := r.List(ctx, &list,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(secretRefIndex, obj.GetName())},
	); err != nil {
		// A map function can only return nothing, so this is logged: otherwise
		// nothing explains a rotated key waiting out the hourly resync.
		log.FromContext(ctx).Error(err, "listing CloudAccounts for a Secret change",
			"secret", client.ObjectKeyFromObject(obj))
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, account := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: account.Namespace, Name: account.Name},
		})
	}
	return requests
}
