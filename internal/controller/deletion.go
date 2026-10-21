package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// reconcileDelete honours the deletion policy and then releases the finalizer.
func (r *CloudClusterReconciler) reconcileDelete(
	ctx context.Context,
	original, cluster *v1alpha1.CloudCluster,
	cloud cloudapi.Client,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		return ctrl.Result{}, nil
	}

	if nothingToTerminate(cluster) {
		switch {
		case goneFromCloud(cluster):
			log.FromContext(ctx).Info("the RavenDB Cloud cluster was already deleted outside this operator",
				"productID", cluster.Status.ProductID)
		case keepsWhatItTookOver(cluster) && cluster.EffectiveDeletionPolicy() == v1alpha1.DeletionTerminate:
			emit(r.Recorder, cluster, corev1.EventTypeWarning, v1alpha1.EventRetained,
				"deletionPolicy is Terminate, but this object took over RavenDB Cloud cluster %s under creationMode "+
					"TakeOverOrCreate rather than creating it, so it is left running. Terminate it in the RavenDB Cloud "+
					"portal, or use creationMode TakeOverOnly when Terminate should end a cluster this object takes over.",
				cluster.Status.ProductID)
			log.FromContext(ctx).Info("leaving a cluster this object took over running", "productID", cluster.Status.ProductID)
		case cluster.Status.ProductID != "":
			emit(r.Recorder, cluster, corev1.EventTypeNormal, v1alpha1.EventRetained,
				"deletionPolicy is Retain: leaving RavenDB Cloud cluster %s running.", cluster.Status.ProductID)
			log.FromContext(ctx).Info("leaving the RavenDB Cloud cluster running", "productID", cluster.Status.ProductID)
		}
		return r.releaseFinalizer(ctx, cluster)
	}

	productID := cluster.Status.ProductID
	if productID == "" {
		found, result, err := r.findInterruptedCreate(ctx, original, cluster, cloud)
		if found == "" {
			return result, err
		}
		productID = found
	}

	emit(r.Recorder, cluster, corev1.EventTypeNormal, v1alpha1.EventTerminating,
		"deletionPolicy is Terminate: terminating RavenDB Cloud cluster %s.", productID)
	log.FromContext(ctx).Info("terminating a RavenDB Cloud cluster", "productID", productID)

	if err := cloud.TerminateCluster(ctx, productID); err != nil {
		return r.terminateFailed(ctx, original, cluster, "terminate RavenDB Cloud cluster "+productID, err)
	}
	return r.releaseFinalizer(ctx, cluster)
}

// findInterruptedCreate finds, by name, the cluster a create made before its id
// reached status, and records the id. Releasing on an empty id is how a paid
// cluster ends up running with no object left to name it.
//
// An empty id means the pass is over, and the result says how it ended.
func (r *CloudClusterReconciler) findInterruptedCreate(
	ctx context.Context, original, cluster *v1alpha1.CloudCluster, cloud cloudapi.Client,
) (string, ctrl.Result, error) {
	name := lookupName(cluster)
	ref, err := cloud.FindClusterByName(ctx, name)
	if err == nil {
		var elsewhere bool
		elsewhere, err = r.belongsElsewhere(ctx, cluster, cloud, ref)
		if err == nil && elsewhere {
			log.FromContext(ctx).Info("the cluster that name finds is not this object's, so there is nothing to terminate",
				"displayName", name, "productID", ref.ID)
			result, err := r.releaseFinalizer(ctx, cluster)
			return "", result, err
		}
	}
	switch {
	case err == nil:
		cluster.Status.ProductID = ref.ID
		return ref.ID, ctrl.Result{}, nil
	case cloudapi.IsNotFound(err):
		log.FromContext(ctx).Info("the interrupted create never landed, so there is nothing to terminate",
			"displayName", name)
		result, err := r.releaseFinalizer(ctx, cluster)
		return "", result, err
	case cloudapi.IsAmbiguous(err):
		// Terminating an arbitrary one of two is unrecoverable, and so is
		// releasing. The advice is to hide, never to terminate: terminating this
		// object's cluster by hand leaves the stranger's as the only match, and
		// the next pass would terminate that.
		r.deletionStuck(ctx, cluster, sentence("Not terminating: "+cloudSaid(err))+
			" Hide the one that is not this object's in the RavenDB Cloud portal, and the next pass terminates the other.")
		result, err := r.finish(ctx, original, cluster, ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}, nil)
		return "", result, err
	}
	result, err := r.terminateFailed(ctx, original, cluster,
		fmt.Sprintf("find the cluster the interrupted create for %q made", name), err)
	return "", result, err
}

// belongsElsewhere reports whether the cluster a name found is plainly not one
// this object created: another CloudCluster holds it, or its tier, provider or
// region is not what this object asks for.
func (r *CloudClusterReconciler) belongsElsewhere(
	ctx context.Context, cluster *v1alpha1.CloudCluster, cloud cloudapi.Client, ref *cloudapi.ClusterRef,
) (bool, error) {
	holder, err := r.holderOf(ctx, cluster, ref.ID)
	if err != nil || holder != "" {
		return holder != "", err
	}
	observed, err := cloud.GetCluster(ctx, ref.ID)
	if err != nil {
		return false, err
	}
	return len(identityDifferences(&cluster.Spec, observed)) > 0, nil
}

// deleteWithoutAccount handles a deletion while the CloudAccount is unusable.
// It releases on the same predicate reconcileDelete does, so a broken account
// never makes deletion stricter than a healthy one.
func (r *CloudClusterReconciler) deleteWithoutAccount(
	ctx context.Context, original, cluster *v1alpha1.CloudCluster, result ctrl.Result, err error,
) (ctrl.Result, error) {
	if nothingToTerminate(cluster) {
		return r.releaseFinalizer(ctx, cluster)
	}
	r.deletionStuck(ctx, cluster, deletionBlockedMessage(cluster))
	return r.finish(ctx, original, cluster, result, err)
}

// terminateFailed keeps the object, and its finalizer, until the terminate goes
// through.
func (r *CloudClusterReconciler) terminateFailed(
	ctx context.Context, original, cluster *v1alpha1.CloudCluster, doing string, err error,
) (ctrl.Result, error) {
	message := sentence(fmt.Sprintf("Could not %s: %s", doing, cloudSaid(err))) +
		" The object stays until this succeeds."
	if cloudapi.IsRetryable(err) {
		r.deletionStuck(ctx, cluster, message+" Retrying.")
		return r.finish(ctx, original, cluster, ctrl.Result{}, err)
	}
	r.deletionStuck(ctx, cluster, message)
	return r.finish(ctx, original, cluster, ctrl.Result{RequeueAfter: cluster.EffectivePollInterval()}, nil)
}

// deletionStuck says what a deletion is waiting for, and warns once per thing
// it is stuck on: Deleting itself does not warn, because most deletions finish
// at once.
func (r *CloudClusterReconciler) deletionStuck(ctx context.Context, cluster *v1alpha1.CloudCluster, message string) {
	if !saysAlready(cluster.Status.Conditions, v1alpha1.ConditionReady, v1alpha1.ReasonDeleting, message) {
		emit(r.Recorder, cluster, corev1.EventTypeWarning, v1alpha1.ReasonDeleting, "%s", message)
	}
	r.ready(ctx, cluster, v1alpha1.ReasonDeleting, message)
}

// nothingToTerminate reports whether deleting this object provably leaves
// nothing for it to terminate in RavenDB Cloud: Retain, a cluster already
// deleted there, one this object may not terminate, or an object that holds no
// productID and never issued a create.
func nothingToTerminate(cluster *v1alpha1.CloudCluster) bool {
	if cluster.EffectiveDeletionPolicy() == v1alpha1.DeletionRetain || goneFromCloud(cluster) || keepsWhatItTookOver(cluster) {
		return true
	}
	return cluster.Status.ProductID == "" && cluster.Status.ProvisionAttemptedAt == nil
}

// keepsWhatItTookOver reports whether Terminate must leave the cluster running:
// this object found it rather than created it, and taking it over was never
// asked for, because under TakeOverOrCreate a name can match a cluster nobody
// meant this object to own.
func keepsWhatItTookOver(cluster *v1alpha1.CloudCluster) bool {
	return cluster.Status.Adopted && cluster.EffectiveCreationMode() == v1alpha1.CreationTakeOverOrCreate
}

// goneFromCloud reports whether the cluster was already deleted in RavenDB Cloud
// when this object last looked. cloudStatus outlives a Ready overwritten by
// Deleting; a 404 records no cloudStatus and leaves only Ready.
func goneFromCloud(cluster *v1alpha1.CloudCluster) bool {
	switch cloudapi.ClusterStatus(cluster.Status.CloudStatus) {
	case cloudapi.StatusTerminating, cloudapi.StatusTerminated:
		return true
	}
	return v1alpha1.ConditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady) == v1alpha1.ReasonDeletedInCloud
}

// deletionBlockedMessage explains why deletion cannot finish while the
// CloudAccount is unusable. It is rebuilt from status rather than appended to
// the condition, so polling does not stack the same sentence up.
func deletionBlockedMessage(cluster *v1alpha1.CloudCluster) string {
	if cluster.Status.ProductID != "" {
		return fmt.Sprintf(
			"deletionPolicy is Terminate, so RavenDB Cloud cluster %s cannot be terminated until CloudAccount %q and the "+
				"API key Secret it names are usable again. Restore whichever is missing, or set spec.deletionPolicy to "+
				"Retain to let this object go and leave the cluster running.",
			cluster.Status.ProductID, cluster.Spec.AccountRef.Name)
	}
	return fmt.Sprintf(
		"deletionPolicy is Terminate and a create was already issued, so deletion waits for CloudAccount %q and the "+
			"API key Secret it names to be usable before it can find out what to terminate. Restore whichever is "+
			"missing, or set spec.deletionPolicy to Retain to let this object go.",
		cluster.Spec.AccountRef.Name)
}

func (r *CloudClusterReconciler) releaseFinalizer(ctx context.Context, cluster *v1alpha1.CloudCluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		return ctrl.Result{}, nil
	}
	patch := lockedPatch(cluster)
	controllerutil.RemoveFinalizer(cluster, clusterFinalizer)
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// addFinalizer writes the finalizer and nothing else.
//
// A merge patch, not an Update: a spec that has been through Go and back is not
// the spec the user applied (metav1.Duration re-renders 5m as 5m0s, and an
// omitempty struct serialises as {}), so an Update bumps generation and the
// object stops matching anyone's git repository.
func (r *CloudClusterReconciler) addFinalizer(ctx context.Context, cluster *v1alpha1.CloudCluster) error {
	if controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		return nil
	}
	patch := lockedPatch(cluster)
	controllerutil.AddFinalizer(cluster, clusterFinalizer)
	return r.Patch(ctx, cluster, patch)
}
