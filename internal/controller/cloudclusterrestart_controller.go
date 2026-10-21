package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
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

// clusterRefIndex answers "which restarts are waiting on this cluster".
const clusterRefIndex = ".spec.clusterRef.name"

// CloudClusterRestartReconciler restarts one node, once.
//
// Two things carry the "once": an immutable spec, so the request cannot be
// re-aimed at another node, and issuedAt, persisted before the call so a crash
// cannot lose the fact that a node is already rebooting.
type CloudClusterRestartReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clients  *cloudclients.Cache
	Recorder events.EventRecorder

	// warned remembers which long waits already raised their Warning.
	warned sync.Map
}

// No create and no delete: the operator never makes a restart request and never
// removes the record of one. patch is the owner reference it adds, so a deleted
// CloudCluster takes its restart records with it.
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudclusterrestarts,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=cloud.ravendb.net,resources=cloudclusterrestarts/status,verbs=get;update

// Reconcile carries one restart from requested to finished.
func (r *CloudClusterRestartReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var restart v1alpha1.CloudClusterRestart
	if err := r.Get(ctx, req.NamespacedName, &restart); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A finished record costs no work and no status write when its cluster's
	// next poll wakes it. issuedAt, not this, is what makes a restart happen
	// once.
	if restart.IsFinished() || !restart.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	original := restart.DeepCopy()
	restart.Status.ObservedGeneration = restart.Generation

	cluster, cloud, result, err := r.resolveCluster(ctx, &restart)
	if err != nil || cluster == nil {
		return r.finish(ctx, original, &restart, result, err)
	}

	// Before anything else writes status: the patch response replaces the
	// object in hand with the server's copy.
	if err := r.adopt(ctx, &restart, cluster); err != nil {
		return ctrl.Result{}, err
	}
	original = restart.DeepCopy()
	restart.Status.ObservedGeneration = restart.Generation

	if restart.Status.IssuedAt != nil {
		result, err = r.awaitCompletion(ctx, &restart, cluster, cloud)
	} else {
		result, err = r.issue(ctx, original, &restart, cluster, cloud)
	}
	return r.finish(ctx, original, &restart, result, err)
}

// resolveCluster finds the CloudCluster this restart names and a client for
// it. A nil cluster with a nil error means "not actionable yet, the status
// says why".
//
// Every reason not to act yet is a wait, never a failure: a restart applied
// with its cluster can be seen first, and one held back by the cluster's policy
// runs once the policy allows it. The cluster watch wakes it either way.
func (r *CloudClusterRestartReconciler) resolveCluster(
	ctx context.Context, restart *v1alpha1.CloudClusterRestart,
) (*v1alpha1.CloudCluster, cloudapi.Client, ctrl.Result, error) {
	ref := types.NamespacedName{Namespace: restart.Namespace, Name: restart.Spec.ClusterRef.Name}

	var cluster v1alpha1.CloudCluster
	if err := r.Get(ctx, ref, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, r.waiting(ctx, restart, fmt.Sprintf(
				"CloudCluster %q not found in namespace %q. This runs once it exists; if the name is wrong, "+
					"delete this CloudClusterRestart and create one naming the right cluster.", ref.Name, ref.Namespace)), nil
		}
		return nil, nil, ctrl.Result{}, err
	}

	// A restart changes the cluster, so the policy applies here too.
	if !cluster.MayMutate() {
		r.complete(ctx, restart, v1alpha1.ReasonWaiting, mayNotMutateMessage(&cluster))
		return nil, nil, ctrl.Result{}, nil
	}

	if cluster.Status.ProductID == "" {
		return nil, nil, r.waiting(ctx, restart,
			fmt.Sprintf("CloudCluster %q is not bound to a RavenDB Cloud cluster yet.", cluster.Name)), nil
	}

	accountRef := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.AccountRef.Name}
	_, cloud, wait, err := usableAccount(ctx, r.Client, r.Clients, accountRef)
	switch {
	case err != nil:
		return nil, nil, ctrl.Result{}, err
	case wait != nil:
		return nil, nil, r.waiting(ctx, restart, wait.message), nil
	}
	return &cluster, cloud, ctrl.Result{}, nil
}

// waiting says what the restart waits for, and looks again after pendingRequeue.
func (r *CloudClusterRestartReconciler) waiting(
	ctx context.Context, restart *v1alpha1.CloudClusterRestart, message string,
) ctrl.Result {
	r.complete(ctx, restart, v1alpha1.ReasonWaiting, message)
	return ctrl.Result{RequeueAfter: pendingRequeue}
}

// mayNotMutateMessage says that the cluster's policy holds the restart back.
func mayNotMutateMessage(cluster *v1alpha1.CloudCluster) string {
	return fmt.Sprintf(
		"CloudCluster %q has managementPolicy %s, so the operator may not change it. Set spec.managementPolicy to Full and this runs.",
		cluster.Name, cluster.EffectiveManagementPolicy())
}

// issue sends the restart, once.
func (r *CloudClusterRestartReconciler) issue(
	ctx context.Context,
	original, restart *v1alpha1.CloudClusterRestart,
	cluster *v1alpha1.CloudCluster,
	cloud cloudapi.Client,
) (ctrl.Result, error) {
	// RavenDB Cloud refuses a restart on a cluster that is not Active, and
	// status.nodeTags is only populated once the cluster has been read.
	if !cluster.IsRunning() {
		r.complete(ctx, restart, v1alpha1.ReasonWaiting, fmt.Sprintf(
			"CloudCluster %q is %s, and RavenDB Cloud only restarts a node on a cluster that is Running.",
			cluster.Name, orNotReconciled(v1alpha1.ConditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady))))
		return ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}, nil
	}

	if !slices.Contains(cluster.Status.NodeTags, restart.Spec.NodeTag) {
		r.complete(ctx, restart, v1alpha1.ReasonFailed, fmt.Sprintf(
			"CloudCluster %q has no node %q; it has %v. A CloudClusterRestart cannot be edited, so create a new "+
				"one naming one of those.", cluster.Name, restart.Spec.NodeTag, cluster.Status.NodeTags))
		return ctrl.Result{}, nil
	}

	if other, err := r.restartUnderWay(ctx, restart); err != nil || other != "" {
		if err != nil {
			return ctrl.Result{}, err
		}
		r.complete(ctx, restart, v1alpha1.ReasonWaiting, fmt.Sprintf(
			"CloudClusterRestart %q is restarting a node of CloudCluster %q, so this one waits for it to finish.",
			other, cluster.Name))
		return ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}, nil
	}

	// Read live: the CloudCluster's status is up to a poll old, and a restart
	// or a change sent since leaves the cluster busy for minutes.
	observed, err := cloud.GetCluster(ctx, cluster.Status.ProductID)
	if err != nil {
		r.complete(ctx, restart, v1alpha1.ReasonWaiting, sentence(fmt.Sprintf(
			"Could not read CloudCluster %q from RavenDB Cloud: %s", cluster.Name, cloudSaid(err)))+" Retrying.")
		return ctrl.Result{}, err
	}
	if !observed.Status.IsServing() {
		r.complete(ctx, restart, v1alpha1.ReasonWaiting, fmt.Sprintf(
			"RavenDB Cloud reports CloudCluster %q %s, and restarts a node only on an Active cluster.",
			cluster.Name, observed.Status))
		return ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}, nil
	}

	// Persisted before the call: a crash must not lose that a node is already
	// rebooting, or it would be restarted again while coming back up. The
	// pass's starting point moves with it, so the pass's own end compares with
	// what is stored.
	issued := metav1.Now()
	restart.Status.IssuedAt = &issued
	r.complete(ctx, restart, v1alpha1.ReasonRestarting,
		fmt.Sprintf("Asking RavenDB Cloud to restart node %s of %s.", restart.Spec.NodeTag, cluster.Status.ProductID))
	if err := r.Status().Update(ctx, restart); err != nil {
		return ctrl.Result{}, err
	}
	original.Status = *restart.Status.DeepCopy()

	if err := cloud.RestartNode(ctx, cluster.Status.ProductID, restart.Spec.NodeTag); err != nil {
		return r.restartFailure(ctx, restart, cluster, err)
	}

	emit(r.Recorder, restart, corev1.EventTypeNormal, v1alpha1.EventRestarting,
		"Restarting node %s of CloudCluster %q (%s).", restart.Spec.NodeTag, cluster.Name, cluster.Status.ProductID)
	log.FromContext(ctx).Info("restart issued",
		"cluster", cluster.Name, "productID", cluster.Status.ProductID, "node", restart.Spec.NodeTag)
	return ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}, nil
}

// awaitCompletion asks RavenDB Cloud whether the node came back.
//
// It asks the control plane, not the CloudCluster's status: status is only as
// fresh as the cluster's last poll, five minutes by default, and reading it
// reported Succeeded while the node was still rebooting. A restart is rare and
// short, so one request per pass is the right price.
func (r *CloudClusterRestartReconciler) awaitCompletion(
	ctx context.Context,
	restart *v1alpha1.CloudClusterRestart,
	cluster *v1alpha1.CloudCluster,
	cloud cloudapi.Client,
) (ctrl.Result, error) {
	observed, err := cloud.GetCluster(ctx, cluster.Status.ProductID)
	if err != nil {
		return r.completionUnknown(ctx, restart, cluster, err)
	}

	switch {
	case observed.Status.IsTransitional():
		r.complete(ctx, restart, v1alpha1.ReasonRestarting, fmt.Sprintf(
			"Node %s is restarting; RavenDB Cloud reports %s.", restart.Spec.NodeTag, observed.Status))
		return ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}, nil
	case !observed.Status.IsServing():
		// That is the restart's outcome, not a reason to keep waiting.
		r.complete(ctx, restart, v1alpha1.ReasonFailed, fmt.Sprintf(
			"Node %s was restarted and RavenDB Cloud now reports the cluster %s; CloudCluster %q says what that means.",
			restart.Spec.NodeTag, observed.Status, cluster.Name))
		return ctrl.Result{}, nil
	}

	completed := metav1.Now()
	restart.Status.CompletedAt = &completed
	r.complete(ctx, restart, v1alpha1.ReasonSucceeded, fmt.Sprintf(
		"Node %s restarted; RavenDB Cloud reports the cluster %s.", restart.Spec.NodeTag, observed.Status))
	emit(r.Recorder, restart, corev1.EventTypeNormal, v1alpha1.EventRestarted,
		"Node %s of CloudCluster %q restarted.", restart.Spec.NodeTag, cluster.Name)
	return ctrl.Result{}, nil
}

// completionUnknown handles a failed read of a restarting cluster. Only a
// cluster that is gone settles the restart; any other failure says nothing
// about the node, so the restart keeps waiting.
func (r *CloudClusterRestartReconciler) completionUnknown(
	ctx context.Context, restart *v1alpha1.CloudClusterRestart, cluster *v1alpha1.CloudCluster, err error,
) (ctrl.Result, error) {
	if cloudapi.IsNotFound(err) {
		r.complete(ctx, restart, v1alpha1.ReasonFailed, fmt.Sprintf(
			"RavenDB Cloud has no cluster %s any more, so whether node %s came back cannot be established.",
			cluster.Status.ProductID, restart.Spec.NodeTag))
		return ctrl.Result{}, nil
	}
	r.complete(ctx, restart, v1alpha1.ReasonRestarting, sentence(fmt.Sprintf(
		"Could not ask RavenDB Cloud whether node %s came back: %s", restart.Spec.NodeTag, cloudSaid(err)))+" Still waiting.")
	if cloudapi.IsRetryable(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}, nil
}

// restartFailure decides what a failed restart call means. One that may have
// gone through is looked for, never sent again, since a second one would reboot
// the node twice. A busy cluster and a call that never left are definite noes,
// so they are unstamped and tried again. Any other 4xx will be refused again,
// and the spec cannot change, so it is terminal.
func (r *CloudClusterRestartReconciler) restartFailure(
	ctx context.Context, restart *v1alpha1.CloudClusterRestart, cluster *v1alpha1.CloudCluster, err error,
) (ctrl.Result, error) {
	soon := ctrl.Result{RequeueAfter: cluster.EffectiveTransitionalInterval()}
	switch {
	case cloudapi.MayHaveApplied(err):
		r.complete(ctx, restart, v1alpha1.ReasonRestarting, sentence(fmt.Sprintf(
			"RavenDB Cloud did not confirm the restart of node %s: %s", restart.Spec.NodeTag, cloudSaid(err)))+
			" Checking whether it is under way rather than asking again.")
		return soon, nil
	case cloudapi.IsBusy(err):
		restart.Status.IssuedAt = nil
		r.complete(ctx, restart, v1alpha1.ReasonWaiting, sentence(fmt.Sprintf(
			"RavenDB Cloud will not restart node %s while the cluster is busy: %s", restart.Spec.NodeTag, cloudSaid(err)))+
			" Asking again once it is Active.")
		return soon, nil
	case cloudapi.IsRetryable(err):
		restart.Status.IssuedAt = nil
		r.complete(ctx, restart, v1alpha1.ReasonWaiting, sentence(fmt.Sprintf(
			"Could not ask RavenDB Cloud to restart node %s: %s", restart.Spec.NodeTag, cloudSaid(err)))+" Retrying.")
		return ctrl.Result{}, err
	}

	r.complete(ctx, restart, v1alpha1.ReasonFailed,
		sentence(fmt.Sprintf("RavenDB Cloud refused to restart node %s: %s", restart.Spec.NodeTag, cloudSaid(err)))+
			" A CloudClusterRestart runs once, so create a new one to try again.")
	return ctrl.Result{}, nil
}

// restartUnderWay names another restart of the same cluster that was issued
// and has not finished, or returns "". RavenDB Cloud restarts one node at a
// time, and a second request while the first runs is refused.
func (r *CloudClusterRestartReconciler) restartUnderWay(
	ctx context.Context, restart *v1alpha1.CloudClusterRestart,
) (string, error) {
	var restarts v1alpha1.CloudClusterRestartList
	if err := r.List(ctx, &restarts, client.InNamespace(restart.Namespace)); err != nil {
		return "", err
	}
	for _, other := range restarts.Items {
		if other.Name != restart.Name && other.Spec.ClusterRef.Name == restart.Spec.ClusterRef.Name &&
			other.Status.IssuedAt != nil && !other.IsFinished() {
			return other.Name, nil
		}
	}
	return "", nil
}

// adopt makes the restart a child of the cluster it acts on, so deleting the
// cluster collects its records.
func (r *CloudClusterRestartReconciler) adopt(
	ctx context.Context, restart *v1alpha1.CloudClusterRestart, cluster *v1alpha1.CloudCluster,
) error {
	if metav1.IsControlledBy(restart, cluster) {
		return nil
	}
	patch := lockedPatch(restart)
	if err := controllerutil.SetControllerReference(cluster, restart, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, restart, patch)
}

func (r *CloudClusterRestartReconciler) finish(
	ctx context.Context, original, restart *v1alpha1.CloudClusterRestart, result ctrl.Result, err error,
) (ctrl.Result, error) {
	return writeStatus(ctx, r.Client, restart, &original.Status, &restart.Status, result, err)
}

// orNotReconciled keeps a message readable before the cluster has any status.
func orNotReconciled(reason string) string {
	if reason == "" {
		return "not reconciled yet"
	}
	return reason
}

// indexRestartsByClusterRef indexes a CloudClusterRestart by the cluster it
// names.
func indexRestartsByClusterRef(obj client.Object) []string {
	restart, ok := obj.(*v1alpha1.CloudClusterRestart)
	if !ok || restart.Spec.ClusterRef.Name == "" {
		return nil
	}
	return []string{restart.Spec.ClusterRef.Name}
}

// SetupWithManager wires the controller and the cluster watch it waits on.
func (r *CloudClusterRestartReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&v1alpha1.CloudClusterRestart{}, clusterRefIndex, indexRestartsByClusterRef); err != nil {
		return err
	}

	// One at a time, so two restarts of one cluster are never issued in the
	// same instant; restartUnderWay covers one issued on an earlier pass.
	options := controllerOptions()
	options.MaxConcurrentReconciles = 1

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.CloudClusterRestart{}, builder.WithPredicates(ignoreStatusOnlyUpdates())).
		// A waiting restart waits for its cluster to become Ready. Not Owns: the
		// cluster is the owner here.
		Watches(&v1alpha1.CloudCluster{}, handler.EnqueueRequestsFromMapFunc(r.restartsWaitingOn)).
		WithOptions(options).
		Named("cloudclusterrestart").
		Complete(r)
}

// restartsWaitingOn enqueues the restarts a cluster change could move. Finished
// ones are skipped: a cluster writes status every poll, and every restart it
// ever had would otherwise be woken each time.
func (r *CloudClusterRestartReconciler) restartsWaitingOn(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.CloudClusterRestartList
	if err := r.List(ctx, &list,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(clusterRefIndex, obj.GetName())},
	); err != nil {
		// A map function can only return nothing, so this is logged: the
		// restart then waits out its own timer.
		log.FromContext(ctx).Error(err, "listing the CloudClusterRestarts waiting on a cluster",
			"cluster", client.ObjectKeyFromObject(obj))
		return nil
	}

	var requests []reconcile.Request
	for i := range list.Items {
		if list.Items[i].IsFinished() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name},
		})
	}
	return requests
}
