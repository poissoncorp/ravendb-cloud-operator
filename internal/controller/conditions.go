package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
)

// meaning is what one condition reason stands for: the condition status it
// carries, the event, if any, that entering it records, and how far the object
// is from where its spec says.
type meaning struct {
	// status is the condition status. Empty keeps whatever status the
	// condition already had, which only InUse on a CloudAccount wants.
	status metav1.ConditionStatus
	// event is corev1.EventTypeNormal, corev1.EventTypeWarning, or empty for
	// none. A Warning is a problem starting that a person may have to act on.
	event string
	// warnAfter turns a wait that usually ends by itself into a Warning once
	// it has lasted this long, once per wait.
	warnAfter time.Duration
	progress  progress
}

// progress is what kstatus reads from an object: still working towards the
// spec, stuck until a person acts, or settled, the zero value.
type progress int

const (
	settled progress = iota
	reconciling
	stalled
)

// reasons is the closed list of reasons one condition may give.
type reasons map[string]meaning

var (
	isTrue    = meaning{status: metav1.ConditionTrue}
	isFalse   = meaning{status: metav1.ConditionFalse}
	isUnknown = meaning{status: metav1.ConditionUnknown}
)

func (m meaning) warns() meaning                     { m.event = corev1.EventTypeWarning; return m }
func (m meaning) notifies() meaning                  { m.event = corev1.EventTypeNormal; return m }
func (m meaning) warnsAfter(d time.Duration) meaning { m.warnAfter = d; return m }
func (m meaning) inProgress() meaning                { m.progress = reconciling; return m }
func (m meaning) stalls() meaning                    { m.progress = stalled; return m }

// longWait is how long a wait that usually ends by itself may last before it
// warns: a Secret or an account applied together with its users arrives within
// seconds, and a misspelled name never does.
const longWait = time.Minute

// clusterReady is a CloudCluster's Ready condition, the STATUS column.
//
// WaitingForAccount warns only once it lasts: every object applied together
// with its account passes through it. Deleting does not warn: a stuck deletion
// warns on its own.
var clusterReady = reasons{
	v1alpha1.ReasonRunning:               isTrue,
	v1alpha1.ReasonCreating:              isFalse.inProgress(),
	v1alpha1.ReasonUpdating:              isFalse.inProgress(),
	v1alpha1.ReasonWaitingForCertificate: isFalse.inProgress(),
	v1alpha1.ReasonWaitingForAccount:     isFalse.inProgress().warnsAfter(longWait),
	v1alpha1.ReasonDeleting:              isFalse.inProgress(),
	v1alpha1.ReasonInvalidSpec:           isFalse.warns().stalls(),
	v1alpha1.ReasonRefused:               isFalse.warns().stalls(),
	v1alpha1.ReasonQuotaReached:          isFalse.warns().stalls(),
	v1alpha1.ReasonNameTaken:             isFalse.warns().stalls(),
	v1alpha1.ReasonClusterNotFound:       isFalse.warns().stalls(),
	v1alpha1.ReasonDeletedInCloud:        isFalse.warns().stalls(),
	v1alpha1.ReasonAwaitingPayment:       isFalse.warns().stalls(),
	v1alpha1.ReasonCreationFailed:        isFalse.warns().stalls(),
	v1alpha1.ReasonSecretNotWritten:      isFalse.warns().inProgress(),
	v1alpha1.ReasonCloudUnreachable:      isUnknown.warns().inProgress(),
	v1alpha1.ReasonKeyRejected:           isUnknown.warns().stalls(),
	v1alpha1.ReasonUnexpectedResponse:    isUnknown.warns().stalls(),
}

// clusterSynced is a CloudCluster's Synced condition, the CHANGES column.
// NotApplied is a policy the user chose or a field the API cannot change, so it
// records nothing and is settled. Waiting is news but not a problem.
var clusterSynced = reasons{
	v1alpha1.ReasonApplied:    isTrue,
	v1alpha1.ReasonApplying:   isFalse.inProgress(),
	v1alpha1.ReasonWaiting:    isFalse.notifies().inProgress(),
	v1alpha1.ReasonRefused:    isFalse.warns().stalls(),
	v1alpha1.ReasonNotApplied: isFalse,
}

// accountReady is a CloudAccount's Ready condition, its STATUS column.
// KeyMissing warns only once it lasts: the Secret is usually applied a moment
// after the account.
var accountReady = reasons{
	v1alpha1.ReasonVerified:           isTrue,
	v1alpha1.ReasonKeyMissing:         isFalse.inProgress().warnsAfter(longWait),
	v1alpha1.ReasonKeyRejected:        isFalse.warns().stalls(),
	v1alpha1.ReasonCloudUnreachable:   isUnknown.warns().inProgress(),
	v1alpha1.ReasonUnexpectedResponse: isUnknown.warns().stalls(),
	v1alpha1.ReasonInUse:              {progress: reconciling},
}

// restartComplete is a CloudClusterRestart's Complete condition, its STATUS
// column. Restarting and Succeeded have events of their own. Failed also sets
// the Failed condition; see complete.
var restartComplete = reasons{
	v1alpha1.ReasonWaiting:    isFalse.inProgress(),
	v1alpha1.ReasonRestarting: isFalse.inProgress(),
	v1alpha1.ReasonSucceeded:  isTrue,
	v1alpha1.ReasonFailed:     isFalse.warns().stalls(),
}

// The tables of each kind by condition type, which Reconciling and Stalled are
// derived from.
var (
	clusterTables = map[string]reasons{v1alpha1.ConditionReady: clusterReady, v1alpha1.ConditionSynced: clusterSynced}
	accountTables = map[string]reasons{v1alpha1.ConditionReady: accountReady}
	restartTables = map[string]reasons{v1alpha1.ConditionComplete: restartComplete}
)

// conditionWriter sets conditions on one object and says so. Every condition
// change goes through it, so the condition, the event, the log line and the
// kstatus conditions agree.
type conditionWriter struct {
	recorder events.EventRecorder
	object   client.Object
	tables   map[string]reasons
	// warned remembers which long waits already warned, so each warns once.
	warned *sync.Map
}

// set writes condType with the status its reason carries, then reports the
// change: one log line when there is news, and the event the reason asks for.
//
// News is a different reason, or a different message after the spec changed. A
// message that changes on its own, such as a countdown RavenDB Cloud writes, is
// not news, which keeps a problem to one Warning rather than one per poll.
func (w conditionWriter) set(
	ctx context.Context, conditions *[]metav1.Condition, generation int64,
	condType, reason, message string,
) {
	defer w.summarize(conditions, generation)
	m, known := w.tables[condType][reason]
	// A copy: SetCondition overwrites the element GetCondition points at, and
	// the log line has to say what the condition was before.
	var previous *metav1.Condition
	if found := meta.FindStatusCondition(*conditions, condType); found != nil {
		before := *found
		previous = &before
	}
	status := m.status
	switch {
	case !known:
		// A bug here, not a state the user is in.
		log.FromContext(ctx).Error(nil, "a condition reason is missing from its table",
			"condition", condType, "reason", reason)
		status = metav1.ConditionUnknown
	case status == "" && previous != nil:
		status = previous.Status
	case status == "":
		status = metav1.ConditionFalse
	}

	news := previous == nil || previous.Reason != reason ||
		(previous.Message != message && previous.ObservedGeneration != generation)
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
	if !news {
		w.warnIfLong(m, previous, condType, message)
		return
	}

	from := "none"
	if previous != nil {
		from = previous.Reason
	}
	log.FromContext(ctx).Info("condition changed",
		"condition", condType, "from", from, "to", reason, "message", message)
	if m.event != "" {
		emit(w.recorder, w.object, m.event, reason, "%s", message)
	}
}

// warnIfLong records the Warning a long wait earns, once per wait: a wait is
// one stretch in one reason, which its transition time names.
func (w conditionWriter) warnIfLong(m meaning, current *metav1.Condition, condType, message string) {
	if m.warnAfter == 0 || current == nil || w.recorder == nil || w.warned == nil ||
		time.Since(current.LastTransitionTime.Time) < m.warnAfter {
		return
	}
	key := fmt.Sprintf("%s/%s/%s/%d", w.object.GetUID(), condType, current.Reason, current.LastTransitionTime.Unix())
	if _, already := w.warned.LoadOrStore(key, true); already {
		return
	}
	emit(w.recorder, w.object, corev1.EventTypeWarning, current.Reason, "%s", message)
}

// summarize sets Reconciling and Stalled from the reasons the object's own
// conditions give: Stalled while one of them is stuck until a person acts,
// otherwise Reconciling while one is still working, and neither once the
// object has settled.
func (w conditionWriter) summarize(conditions *[]metav1.Condition, generation int64) {
	var stuck, working *metav1.Condition
	for _, c := range *conditions {
		switch w.tables[c.Type][c.Reason].progress {
		case stalled:
			if stuck == nil {
				stuck = &c
			}
		case reconciling:
			if working == nil {
				working = &c
			}
		}
	}
	if stuck != nil {
		working = nil
	}
	summary := []struct {
		condType string
		source   *metav1.Condition
	}{{v1alpha1.ConditionStalled, stuck}, {v1alpha1.ConditionReconciling, working}}
	for _, s := range summary {
		if s.source == nil {
			meta.RemoveStatusCondition(conditions, s.condType)
			continue
		}
		meta.SetStatusCondition(conditions, metav1.Condition{
			Type:               s.condType,
			Status:             metav1.ConditionTrue,
			Reason:             s.source.Reason,
			Message:            s.source.Message,
			ObservedGeneration: generation,
		})
	}
}

// ready sets a CloudCluster's Ready condition.
func (r *CloudClusterReconciler) ready(ctx context.Context, cluster *v1alpha1.CloudCluster, reason, message string) {
	conditionWriter{r.Recorder, cluster, clusterTables, &r.warned}.set(ctx, &cluster.Status.Conditions,
		cluster.Generation, v1alpha1.ConditionReady, reason, message)
}

// synced sets a CloudCluster's Synced condition.
func (r *CloudClusterReconciler) synced(ctx context.Context, cluster *v1alpha1.CloudCluster, reason, message string) {
	conditionWriter{r.Recorder, cluster, clusterTables, &r.warned}.set(ctx, &cluster.Status.Conditions,
		cluster.Generation, v1alpha1.ConditionSynced, reason, message)
}

// ready sets a CloudAccount's Ready condition.
func (r *CloudAccountReconciler) ready(ctx context.Context, account *v1alpha1.CloudAccount, reason, message string) {
	conditionWriter{r.Recorder, account, accountTables, &r.warned}.set(ctx, &account.Status.Conditions,
		account.Generation, v1alpha1.ConditionReady, reason, message)
}

// complete sets a CloudClusterRestart's Complete condition, and Failed with
// it when the restart failed, as a Job does.
func (r *CloudClusterRestartReconciler) complete(
	ctx context.Context, restart *v1alpha1.CloudClusterRestart, reason, message string,
) {
	conditionWriter{r.Recorder, restart, restartTables, &r.warned}.set(ctx, &restart.Status.Conditions,
		restart.Generation, v1alpha1.ConditionComplete, reason, message)
	if reason == v1alpha1.ReasonFailed {
		meta.SetStatusCondition(&restart.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionFailed,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: restart.Generation,
		})
	}
}
