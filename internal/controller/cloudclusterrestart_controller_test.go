package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
)

func (h *harness) reconcileRestart(name string) ctrl.Result {
	h.t.Helper()
	result, err := h.restart.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	if err != nil {
		h.t.Fatalf("reconcile restart %s: %v", name, err)
	}
	return result
}

func (h *harness) k8sCreate(obj client.Object) {
	h.t.Helper()
	if err := h.k8s.Create(context.Background(), obj); err != nil {
		h.t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
	}
}

func (h *harness) getRestart(name string) *v1alpha1.CloudClusterRestart {
	h.t.Helper()
	var restart v1alpha1.CloudClusterRestart
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	if err := h.k8s.Get(context.Background(), key, &restart); err != nil {
		h.t.Fatalf("getting CloudClusterRestart %s: %v", name, err)
	}
	return &restart
}

// restartStatus is the restart's STATUS column: its Complete condition's reason.
func restartStatus(restart *v1alpha1.CloudClusterRestart) string {
	return v1alpha1.ConditionReason(restart.Status.Conditions, v1alpha1.ConditionComplete)
}

func restartOf(cluster, nodeTag string, mutate ...func(*v1alpha1.CloudClusterRestart)) *v1alpha1.CloudClusterRestart {
	restart := &v1alpha1.CloudClusterRestart{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-" + cluster + "-" + nodeTag, Namespace: testNamespace},
		Spec: v1alpha1.CloudClusterRestartSpec{
			ClusterRef: v1alpha1.LocalObjectReference{Name: cluster},
			NodeTag:    nodeTag,
		},
	}
	for _, m := range mutate {
		m(restart)
	}
	return restart
}

// readyCluster brings a provisioned cluster all the way up, which is the state
// every restart below starts from.
func readyCluster(t *testing.T, h *harness, name string) *v1alpha1.CloudCluster {
	t.Helper()
	h.reconcileCluster(name) // creates
	h.reconcileCluster(name) // observes it Active
	cluster := h.getCluster(name)
	if !cluster.IsRunning() {
		t.Fatalf("cluster %s is %s, want Running before restarting a node of it",
			name, conditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady))
	}
	return cluster
}

func TestRestartRestartsTheNodeAndCompletes(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")
	tag := cluster.Status.NodeTags[0]

	h.k8sCreate(restartOf("orders", tag))
	name := "restart-orders-" + tag

	h.reconcileRestart(name)
	if got := h.cloud.CallCount("RestartNode"); got != 1 {
		t.Fatalf("RestartNode called %d times, want 1", got)
	}
	if got := restartStatus(h.getRestart(name)); got != v1alpha1.ReasonRestarting {
		t.Errorf("status = %q, want Restarting", got)
	}

	// The cluster goes through DeployingChanges and back. Its own controller is
	// what notices, which is why the restart reads the cluster rather than the
	// cloud while it waits.
	h.reconcileCluster("orders")
	h.reconcileRestart(name)

	restart := h.getRestart(name)
	if restartStatus(restart) != v1alpha1.ReasonSucceeded {
		t.Fatalf("status = %q, want Succeeded: %+v", restartStatus(restart), restart.Status.Conditions)
	}
	if !meta.IsStatusConditionTrue(restart.Status.Conditions, v1alpha1.ConditionComplete) {
		t.Error("Complete is not True on a finished restart")
	}
	if restart.Status.CompletedAt == nil {
		t.Error("no completedAt on a finished restart")
	}
}

// TestARestartSaysWhyItsAccountCannotBeUsed: the restart reads its account the
// way the cluster does, so it waits in the account's own words rather than a
// bare "not usable yet" that sends somebody looking.
func TestARestartSaysWhyItsAccountCannotBeUsed(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")

	account := h.getAccount(testAccountName)
	meta.SetStatusCondition(&account.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonKeyRejected,
		Message:            "the key was rejected",
		ObservedGeneration: account.Generation,
	})
	if err := h.k8s.Status().Update(context.Background(), account); err != nil {
		t.Fatalf("marking the account not Ready: %v", err)
	}

	restart := restartOf("orders", cluster.Status.NodeTags[0])
	h.k8sCreate(restart)
	h.reconcileRestart(restart.Name)

	complete := meta.FindStatusCondition(h.getRestart(restart.Name).Status.Conditions, v1alpha1.ConditionComplete)
	if complete == nil || complete.Reason != v1alpha1.ReasonWaiting {
		t.Fatalf("Complete = %+v, want Waiting", complete)
	}
	if !strings.Contains(complete.Message, "the key was rejected") {
		t.Errorf("message %q does not carry the account's own reason", complete.Message)
	}
	if got := h.cloud.CallCount("RestartNode"); got != 0 {
		t.Errorf("RestartNode called %d times while the account was unusable", got)
	}
}

// The whole reason this is an object rather than a field: it must happen once.
// A finished restart that keeps being reconciled, by a resync or by its
// cluster's next status write, must not reboot the node again.
func TestRestartHappensOnlyOnce(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")
	tag := cluster.Status.NodeTags[0]

	h.k8sCreate(restartOf("orders", tag))
	name := "restart-orders-" + tag

	h.reconcileRestart(name)
	h.reconcileCluster("orders")
	h.reconcileRestart(name)
	if got := restartStatus(h.getRestart(name)); got != v1alpha1.ReasonSucceeded {
		t.Fatalf("status = %q, want Succeeded before testing that it stays put", got)
	}

	for range 5 {
		h.reconcileRestart(name)
	}
	if got := h.cloud.CallCount("RestartNode"); got != 1 {
		t.Errorf("RestartNode called %d times across repeated reconciles, want exactly 1", got)
	}
}

// A crash between the call going out and the result being recorded must not
// reboot the node a second time. issuedAt is the marker that says so, exactly
// as provisionAttemptedAt does for a create.
func TestRestartIsNotReissuedAfterACrash(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")
	tag := cluster.Status.NodeTags[0]

	// Seeded as an operator that died right after the call: the marker is set,
	// nothing else is.
	issued := metav1.Now()
	h.k8sCreate(restartOf("orders", tag, func(r *v1alpha1.CloudClusterRestart) {
		r.Status.IssuedAt = &issued
	}))
	name := "restart-orders-" + tag

	h.reconcileRestart(name)

	if got := h.cloud.CallCount("RestartNode"); got != 0 {
		t.Errorf("RestartNode called %d times after recovery, want 0: the node was already rebooting", got)
	}
	if got := restartStatus(h.getRestart(name)); got != v1alpha1.ReasonSucceeded {
		t.Errorf("status = %q, want the recovery to conclude the restart finished", got)
	}
}

// Both safety valves on the cluster apply, because a restart changes it. They
// have different fixes, so the message has to name the right one, and the
// restart has to run once the fix is made rather than fail for good while
// promising that the fix would make it run.
func TestRestartWaitsOnTheClusterSafetyValves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fence func(*v1alpha1.CloudCluster)
		lift  func(*v1alpha1.CloudCluster)
		says  string
	}{
		{
			"observe",
			func(c *v1alpha1.CloudCluster) { c.Spec.ManagementPolicy = v1alpha1.ManagementObserve },
			func(c *v1alpha1.CloudCluster) { c.Spec.ManagementPolicy = v1alpha1.ManagementFull },
			"managementPolicy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Found rather than created, so that the fence does not stop the
			// object binding: Observe never creates.
			// Full is explicit because a found cluster would default to Observe.
			cluster := takeOverCluster("orders", func(c *v1alpha1.CloudCluster) {
				c.Spec.ManagementPolicy = v1alpha1.ManagementFull
				tc.fence(c)
			})
			h := newHarness(t, credentialSecret(), readyAccount(), cluster)
			h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})
			h.reconcileCluster("orders")
			h.reconcileCluster("orders")

			h.k8sCreate(restartOf("orders", "A"))
			h.reconcileRestart("restart-orders-A")

			restart := h.getRestart("restart-orders-A")
			if restartStatus(restart) != v1alpha1.ReasonWaiting {
				t.Fatalf("status = %q, want Waiting", restartStatus(restart))
			}
			if got := h.cloud.CallCount("RestartNode"); got != 0 {
				t.Errorf("RestartNode called %d times on a fenced-off cluster, want 0", got)
			}
			cond := meta.FindStatusCondition(restart.Status.Conditions, v1alpha1.ConditionComplete)
			if cond == nil || !strings.Contains(cond.Message, tc.says) {
				t.Errorf("message %q does not name %s, so it points at the wrong fix", cond.Message, tc.says)
			}

			fenced := h.getCluster("orders")
			tc.lift(fenced)
			if err := h.k8s.Update(context.Background(), fenced); err != nil {
				t.Fatalf("lifting the fence: %v", err)
			}
			h.reconcileCluster("orders")
			h.reconcileRestart("restart-orders-A")

			if got := h.cloud.CallCount("RestartNode"); got != 1 {
				t.Errorf("RestartNode called %d times once the fence was lifted, want 1", got)
			}
		})
	}
}

// The tag has to exist, and that is the check CEL cannot make: the answer is
// in the cluster's status, not in this object.
func TestRestartRefusesANodeTheClusterDoesNotHave(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")

	absent := "Z"
	for _, tag := range cluster.Status.NodeTags {
		if tag == absent {
			t.Fatalf("the fixture grew a node %s; pick another tag for this test", absent)
		}
	}

	h.k8sCreate(restartOf("orders", absent))
	h.reconcileRestart("restart-orders-" + absent)

	restart := h.getRestart("restart-orders-" + absent)
	if restartStatus(restart) != v1alpha1.ReasonFailed {
		t.Fatalf("status = %q, want Failed", restartStatus(restart))
	}
	if got := h.cloud.CallCount("RestartNode"); got != 0 {
		t.Errorf("RestartNode called %d times for a node that does not exist, want 0", got)
	}
	// Failed is final, since the object cannot be edited, so the message has to
	// say what to do instead.
	cond := meta.FindStatusCondition(restart.Status.Conditions, v1alpha1.ConditionComplete)
	if cond == nil || !strings.Contains(cond.Message, "create a new one") {
		t.Errorf("message %+v does not say to create a new restart", cond)
	}
}

// A restart applied in the same file as its cluster can be reconciled before
// the cluster is in the cache. That must not fail it for good: it waits, and
// runs once the cluster is there.
func TestRestartWaitsForItsClusterToExist(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount())

	h.k8sCreate(restartOf("orders", "A"))
	result := h.reconcileRestart("restart-orders-A")

	restart := h.getRestart("restart-orders-A")
	if restartStatus(restart) != v1alpha1.ReasonWaiting {
		t.Fatalf("status = %q, want Waiting", restartStatus(restart))
	}
	cond := meta.FindStatusCondition(restart.Status.Conditions, v1alpha1.ConditionComplete)
	if cond == nil || !strings.Contains(cond.Message, "orders") {
		t.Errorf("message %+v does not name the cluster it is waiting for", cond)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue, so the restart would wait for an unrelated edit")
	}

	h.k8sCreate(provisionCluster("orders"))
	readyCluster(t, h, "orders")
	h.reconcileRestart("restart-orders-A")
	if got := h.cloud.CallCount("RestartNode"); got != 1 {
		t.Errorf("RestartNode called %d times once the cluster existed, want 1", got)
	}
}

// A cluster still being created is not the user's mistake, so this waits
// rather than failing, and spends no call learning what the cluster's own
// status already says.
func TestRestartWaitsForAClusterThatIsNotReady(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.cloud.State().SetProvisionDuration(time.Hour)
	h.reconcileCluster("orders")
	h.reconcileCluster("orders")

	h.k8sCreate(restartOf("orders", "A"))
	result := h.reconcileRestart("restart-orders-A")

	restart := h.getRestart("restart-orders-A")
	if restartStatus(restart) != v1alpha1.ReasonWaiting {
		t.Fatalf("status = %q, want Waiting", restartStatus(restart))
	}
	if got := h.cloud.CallCount("RestartNode"); got != 0 {
		t.Errorf("RestartNode called %d times against a cluster that is not Active, want 0", got)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue, so the restart would wait for an unrelated edit")
	}
}

// Deleting the cluster should collect its restart records, which is what the
// owner reference is for.
func TestRestartIsOwnedByItsCluster(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")
	tag := cluster.Status.NodeTags[0]

	h.k8sCreate(restartOf("orders", tag))
	h.reconcileRestart("restart-orders-" + tag)

	restart := h.getRestart("restart-orders-" + tag)
	if !metav1.IsControlledBy(restart, cluster) {
		t.Errorf("no controller reference to the cluster: %+v", restart.OwnerReferences)
	}
}

// A finished restart is a record, and records pile up. Its cluster writes
// status on every poll and wakes each of them, so a finished one has to cost
// nothing: no work, and above all no status write, which would wake the next
// reconcile in turn.
func TestFinishedRestartIsFreeToReconcile(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")
	tag := cluster.Status.NodeTags[0]

	h.k8sCreate(restartOf("orders", tag))
	name := "restart-orders-" + tag
	h.reconcileRestart(name)
	h.reconcileCluster("orders")
	h.reconcileRestart(name)

	settled := h.getRestart(name)
	if restartStatus(settled) != v1alpha1.ReasonSucceeded {
		t.Fatalf("status = %q, want Succeeded before measuring what a further pass costs", restartStatus(settled))
	}

	before := settled.ResourceVersion
	for range 3 {
		h.reconcileRestart(name)
	}
	if after := h.getRestart(name).ResourceVersion; after != before {
		t.Errorf("a finished restart was written again: resourceVersion %s -> %s", before, after)
	}
}

// awaitCompletion asks RavenDB Cloud rather than reading the CloudCluster's
// status, which is only as fresh as that controller's last poll:
// spec.pollInterval, five minutes by default. Concluding from status means
// concluding from an observation made before the request went out, and against
// the real service that reported Succeeded thirty seconds in, completedAt and
// all, while the control plane still said DeployingChanges and the node was
// still rebooting.
//
// The cluster controller is deliberately never driven after the restart is
// issued here, which is what leaves the cached status stale and reproduces it.
func TestRestartDoesNotConcludeFromAStaleClusterStatus(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")
	tag := cluster.Status.NodeTags[0]

	// Long enough that the node is still coming back on the next pass, the way
	// a real one is.
	h.cloud.State().SetChangeDuration(time.Hour)

	h.k8sCreate(restartOf("orders", tag))
	name := "restart-orders-" + tag

	h.reconcileRestart(name)
	if got := restartStatus(h.getRestart(name)); got != v1alpha1.ReasonRestarting {
		t.Fatalf("status = %q, want Restarting once the request has gone out", got)
	}

	// The cached status still says Running, because nothing re-read the cluster.
	if cached := h.getCluster("orders"); !cached.IsRunning() {
		t.Fatalf("the cached cluster is %q; this test needs it stale at Running",
			conditionReason(cached.Status.Conditions, v1alpha1.ConditionReady))
	}

	h.reconcileRestart(name)

	restart := h.getRestart(name)
	if restartStatus(restart) == v1alpha1.ReasonSucceeded {
		t.Error("reported Succeeded from a cluster status written before the restart was issued")
	}
	if restart.Status.CompletedAt != nil {
		t.Errorf("stamped completedAt %v while the node is still restarting", restart.Status.CompletedAt)
	}
	if restartStatus(restart) != v1alpha1.ReasonRestarting {
		t.Errorf("status = %q, want Restarting while the control plane still reports a change in flight", restartStatus(restart))
	}
}
