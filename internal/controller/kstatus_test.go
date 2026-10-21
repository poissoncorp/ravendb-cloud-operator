package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
)

// What kstatus, and through it Flux, Argo CD and kubectl wait, reads from an
// object: Reconciling while it works, Stalled while it waits for a person,
// neither once settled.
func TestReconcilingAndStalledFollowTheReasons(t *testing.T) {
	present := func(cluster *v1alpha1.CloudCluster, condType string) bool {
		return meta.IsStatusConditionTrue(cluster.Status.Conditions, condType)
	}

	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("refused"), provisionCluster("orders"))
	h.cloud.FailNext("CreateCluster", http.StatusBadRequest)
	h.reconcileCluster("refused")
	refused := h.getCluster("refused")
	if !present(refused, v1alpha1.ConditionStalled) || present(refused, v1alpha1.ConditionReconciling) {
		t.Errorf("a refused create: Stalled %v, Reconciling %v; want Stalled only",
			present(refused, v1alpha1.ConditionStalled), present(refused, v1alpha1.ConditionReconciling))
	}

	settled := h.reconcileClusterUntilSettled("orders", 6)
	if present(settled, v1alpha1.ConditionStalled) || present(settled, v1alpha1.ConditionReconciling) {
		t.Errorf("a Running, Applied cluster still says Stalled or Reconciling: %+v", settled.Status.Conditions)
	}
}

// As on a Job, so kubectl wait --for=condition=Complete never reports a failed
// restart as done.
func TestAFailedRestartIsFailedNotComplete(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	readyCluster(t, h, "orders")
	h.k8sCreate(restartOf("orders", "Z"))
	h.reconcileRestart("restart-orders-Z")

	restart := h.getRestart("restart-orders-Z")
	if meta.IsStatusConditionTrue(restart.Status.Conditions, v1alpha1.ConditionComplete) ||
		!meta.IsStatusConditionTrue(restart.Status.Conditions, v1alpha1.ConditionFailed) {
		t.Errorf("a restart of a node that does not exist: %+v; want Failed True and Complete not True", restart.Status.Conditions)
	}
	if !restart.IsFinished() {
		t.Error("a failed restart is not finished, so it would be tried again")
	}
}

// A wait that usually ends by itself warns once it has lasted: a misspelled
// account name never resolves.
func TestALongWaitForTheAccountWarnsOnce(t *testing.T) {
	h := newHarness(t, credentialSecret(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.AccountRef.Name = "misspelled"
	}))
	h.reconcileCluster("orders")
	if events := strings.Join(h.events(), "\n"); strings.Contains(events, "Warning") {
		t.Fatalf("warned at once, before the account had a chance to arrive:\n%s", events)
	}

	cluster := h.getCluster("orders")
	waiting := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady)
	waiting.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * longWait))
	if err := h.k8s.Status().Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	h.reconcileCluster("orders")
	h.reconcileCluster("orders")

	warnings := 0
	for _, e := range h.events() {
		if strings.HasPrefix(e, "Warning "+v1alpha1.ReasonWaitingForAccount) {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("%d WaitingForAccount warnings after a long wait, want exactly one", warnings)
	}
}
