package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"time"

	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// maxRetryBackoff is the longest a failing object waits for its next try.
// controller-runtime's own ceiling is 1000 seconds; five minutes is the default
// poll of a settled cluster, so a failing object is never looked at less often
// than a healthy one.
const maxRetryBackoff = 5 * time.Minute

// concurrentReconciles is how many CloudClusters, and how many CloudAccounts,
// the operator works on at once. With controller-runtime's default of one, a
// call that gets no answer holds up every other object of its kind. Requests do
// not multiply: every worker waits on the same operator-wide rate limiter.
const concurrentReconciles = 4

// passTimeout bounds one reconcile. Without it a control plane that accepts
// connections and never answers holds every worker, and every object waits.
const passTimeout = 2 * time.Minute

// statusWriteTimeout is how long the status write ending a pass may take once
// the pass itself is out of time.
const statusWriteTimeout = 10 * time.Second

// controllerOptions is how the controllers run.
func controllerOptions() controller.Options {
	return controller.Options{
		RateLimiter:             retryBackoff(),
		MaxConcurrentReconciles: concurrentReconciles,
		ReconciliationTimeout:   passTimeout,
	}
}

// retryBackoff is controller-runtime's per-object exponential backoff with the
// ceiling lowered to maxRetryBackoff, plus the overall bucket of ten a second
// its default carried before the priority queue, so a burst of failures
// cannot spin.
func retryBackoff() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, maxRetryBackoff),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// ignoreStatusOnlyUpdates keeps the operator from reconciling its own status
// writes, which would be a self-inflicted rate limit.
// Spec edits, labels, annotations and deletion requests still come through.
func ignoreStatusOnlyUpdates() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
		predicate.LabelChangedPredicate{},
		predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				if e.ObjectOld == nil || e.ObjectNew == nil {
					return true
				}
				// By value: the two objects come from separate decodes, so a
				// pointer comparison would admit every status write on a
				// terminating object.
				return !e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp())
			},
		},
	)
}

// statusChanged reports whether a reconcile moved status. Every write bumps
// resourceVersion and wakes every watcher, so a pass that changed nothing
// writes nothing. A CloudCluster that read RavenDB Cloud always changed
// lastSyncTime, so it writes once per poll and no more often.
func statusChanged[T any](before, after *T) bool {
	return !reflect.DeepEqual(before, after)
}

// writeStatus ends a pass: it writes status when the pass changed it, and folds
// a failed write into the result the pass chose. A requeue on a timer is a
// routine look, so it waits behind changes people make.
func writeStatus[S any](
	ctx context.Context, c client.Client, obj client.Object, before, after *S, result ctrl.Result, err error,
) (ctrl.Result, error) {
	if result.RequeueAfter > 0 && result.Priority == nil {
		result.Priority = new(handler.LowPriority)
	}
	if !statusChanged(before, after) {
		return result, err
	}
	// Not the pass's context: a pass cut off by passTimeout still records
	// what it found, why it stopped included.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusWriteTimeout)
	defer cancel()
	updateErr := c.Status().Update(ctx, obj)
	switch {
	case updateErr == nil:
		return result, err
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("%w (status update also failed: %v)", err, updateErr)
	}
	return statusWriteFailed(ctx, result, updateErr)
}

// statusWriteFailed decides what a failed status write means for the pass that
// computed it.
//
// A conflict means the object changed under this pass, so its status is stale
// anyway. Returning the error would redo the whole pass, RavenDB Cloud read
// included; keeping the requeue this pass chose revisits the object
// regardless. NotFound is the object being gone.
func statusWriteFailed(ctx context.Context, result ctrl.Result, err error) (ctrl.Result, error) {
	switch {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil
	case apierrors.IsConflict(err):
		log.FromContext(ctx).V(1).Info("dropped a status write: the object changed under this pass")
		return result, nil
	default:
		return ctrl.Result{}, err
	}
}

// arrivalsAndDepartures passes creates and deletes and drops updates, which is
// the shape of a watch kept to answer "who is there now". Creates matter too: a
// cluster created while its account waits to be deleted is one more holding it.
func arrivalsAndDepartures() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
	}
}

// deletionsOnly passes delete events and nothing else.
func deletionsOnly() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
	}
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// emit records an event when there is a recorder; unit tests often run without
// one. events.k8s.io requires an action, and the reason doubles as one.
func emit(recorder events.EventRecorder, obj runtime.Object, eventType, reason, format string, args ...any) {
	if recorder == nil {
		return
	}
	recorder.Eventf(obj, nil, eventType, reason, reason, format, args...)
}

// lockedPatch is a merge patch that fails, rather than overwrites, when the
// object changed after it was read. A merge patch sends whole lists, so without
// the lock a stale copy would drop a finalizer or an owner reference another
// controller added in the meantime.
func lockedPatch(obj client.Object) client.Patch {
	return client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
}
