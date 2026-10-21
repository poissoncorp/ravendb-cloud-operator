package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
)

// drain returns every event recorded so far.
func drain(recorder *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestAProblemWarnsOnceWhenItStarts pins what counts as news, which is what
// decides whether a Warning is recorded: a different reason, or a different
// message after the spec changed. The same problem on the next poll is not
// news, and neither is a message that changes on its own, such as a
// countdown RavenDB Cloud writes.
func TestAProblemWarnsOnceWhenItStarts(t *testing.T) {
	recorder := events.NewFakeRecorder(20)
	r := &CloudClusterReconciler{Recorder: recorder}
	cluster := &v1alpha1.CloudCluster{}
	cluster.Generation = 1
	ctx := context.Background()

	steps := []struct {
		what       string
		generation int64
		reason     string
		message    string
		warns      bool
	}{
		{"a problem starts", 1, v1alpha1.ReasonInvalidSpec, "region is wrong", true},
		{"the next poll says the same", 1, v1alpha1.ReasonInvalidSpec, "region is wrong", false},
		{"the wording moves on its own", 1, v1alpha1.ReasonInvalidSpec, "region is still wrong", false},
		{"a spec edit meets a new problem", 2, v1alpha1.ReasonInvalidSpec, "instance type is wrong", true},
		{"it is fixed", 2, v1alpha1.ReasonCreating, "creating", false},
		{"another problem starts", 2, v1alpha1.ReasonCloudUnreachable, "no answer", true},
	}
	for _, step := range steps {
		cluster.Generation = step.generation
		r.ready(ctx, cluster, step.reason, step.message)
		events := drain(recorder)
		if step.warns && (len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+step.reason)) {
			t.Errorf("%s: events = %q, want one Warning %s", step.what, events, step.reason)
		}
		if !step.warns && len(events) != 0 {
			t.Errorf("%s: events = %q, want none", step.what, events)
		}
	}
}

// TestATransitionIsLoggedFromWhereItWas: the log line is the operator's record
// of what changed, so it has to name the reason the condition had before. It
// once read the old reason through a pointer into the conditions slice after
// the new one had been written over it, and every line said "Running ->
// Running".
func TestATransitionIsLoggedFromWhereItWas(t *testing.T) {
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := log.IntoContext(context.Background(), logger)
	r := &CloudClusterReconciler{}
	cluster := &v1alpha1.CloudCluster{}

	r.ready(ctx, cluster, v1alpha1.ReasonCreating, "creating")
	r.ready(ctx, cluster, v1alpha1.ReasonRunning, "running")

	want := []string{
		`"condition"="Ready" "from"="none" "to"="Creating"`,
		`"condition"="Ready" "from"="Creating" "to"="Running"`,
	}
	if len(lines) != len(want) {
		t.Fatalf("logged %d lines, want %d: %q", len(lines), len(want), lines)
	}
	for i, line := range lines {
		if !strings.Contains(line, want[i]) {
			t.Errorf("line %d = %s, want it to say %q", i, line, want[i])
		}
	}
}

// TestAReasonCarriesItsStatus: the table, not the caller, decides whether a
// reason is True, False or Unknown, so one word always means one status.
func TestAReasonCarriesItsStatus(t *testing.T) {
	r := &CloudClusterReconciler{}
	for reason, want := range map[string]metav1.ConditionStatus{
		v1alpha1.ReasonRunning:            metav1.ConditionTrue,
		v1alpha1.ReasonCreating:           metav1.ConditionFalse,
		v1alpha1.ReasonDeletedInCloud:     metav1.ConditionFalse,
		v1alpha1.ReasonCloudUnreachable:   metav1.ConditionUnknown,
		v1alpha1.ReasonUnexpectedResponse: metav1.ConditionUnknown,
		// Not in the table: a bug here, and Unknown is the honest status.
		"SomethingNobodyAdded": metav1.ConditionUnknown,
	} {
		cluster := &v1alpha1.CloudCluster{}
		r.ready(context.Background(), cluster, reason, "m")
		if got := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady).Status; got != want {
			t.Errorf("%s: Ready = %s, want %s", reason, got, want)
		}
	}
}

// TestInUseKeepsTheStatusItFound is the one reason with no status of its own:
// a CloudCluster decides whether it may use an account from Ready's status,
// and the clusters holding a deleted account open need to.
func TestInUseKeepsTheStatusItFound(t *testing.T) {
	r := &CloudAccountReconciler{}
	for _, before := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse} {
		account := &v1alpha1.CloudAccount{}
		meta.SetStatusCondition(&account.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionReady,
			Status:             before,
			Reason:             v1alpha1.ReasonVerified,
			Message:            "m",
			ObservedGeneration: 0,
		})
		r.ready(context.Background(), account, v1alpha1.ReasonInUse, "held")
		if got := meta.FindStatusCondition(account.Status.Conditions, v1alpha1.ConditionReady).Status; got != before {
			t.Errorf("InUse over %s gave %s", before, got)
		}
	}
}

// TestEveryTableIsClosed: each condition's reasons are a closed list, and a
// reason spelt in two tables has to mean the same kind of thing in both.
func TestEveryTableIsClosed(t *testing.T) {
	for name, table := range map[string]reasons{
		"CloudCluster Ready": clusterReady, "CloudCluster Synced": clusterSynced,
		"CloudAccount Ready": accountReady, "CloudClusterRestart Complete": restartComplete,
	} {
		trues := 0
		for reason, m := range table {
			if reason == "" {
				t.Errorf("%s has an empty reason", name)
			}
			if m.status == metav1.ConditionTrue {
				trues++
			}
		}
		if trues == 0 {
			t.Errorf("%s has no True reason, so it can never be satisfied", name)
		}
	}
	// Words both kinds use for the same problem.
	for _, reason := range []string{v1alpha1.ReasonKeyRejected, v1alpha1.ReasonCloudUnreachable, v1alpha1.ReasonUnexpectedResponse} {
		if clusterReady[reason].event != corev1Warning || accountReady[reason].event != corev1Warning {
			t.Errorf("%s warns on one kind and not the other", reason)
		}
	}
}

const corev1Warning = "Warning"
