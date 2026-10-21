package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// These guard one rule: a cluster that is busy with a change or a restart is
// waited for, never given up on, and an action whose answer was lost is looked
// for, never sent again.

// The rolling restart: one object per node, applied together.
func TestASecondRestartOfABusyClusterWaitsAndThenRuns(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	readyCluster(t, h, "orders")
	h.cloud.State().SetChangeDuration(3 * time.Minute)
	h.k8sCreate(restartOf("orders", "A"))
	h.k8sCreate(restartOf("orders", "B"))

	h.reconcileRestart("restart-orders-A")
	h.reconcileRestart("restart-orders-B")
	if got := restartStatus(h.getRestart("restart-orders-B")); got != v1alpha1.ReasonWaiting {
		t.Fatalf("the second restart is %s while the first runs, want Waiting", got)
	}

	h.cloud.State().Advance(5 * time.Minute)
	h.reconcileRestart("restart-orders-A") // sees node A back
	h.reconcileRestart("restart-orders-B") // now issued
	if n := h.cloud.CallCount("RestartNode"); n != 2 {
		t.Errorf("RestartNode called %d times, want once per node", n)
	}
}

func TestARefusalBecauseTheClusterIsBusyIsClassedAsBusy(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	cluster := readyCluster(t, h, "orders")
	h.cloud.State().SetChangeDuration(3 * time.Minute)
	cloud, err := h.cluster.Clients.ClientFor(context.Background(), readyAccount())
	if err != nil {
		t.Fatal(err)
	}
	if err := cloud.RestartNode(context.Background(), cluster.Status.ProductID, "A"); err != nil {
		t.Fatalf("first restart: %v", err)
	}

	for name, call := range map[string]func() error{
		"restart":       func() error { return cloud.RestartNode(context.Background(), cluster.Status.ProductID, "B") },
		"instance type": func() error { return cloud.ChangeInstanceType(context.Background(), cluster.Status.ProductID, "PB20") },
	} {
		if err := call(); !cloudapi.IsBusy(err) {
			t.Errorf("%s on a cluster that is restarting: %v, want it classed as busy", name, err)
		}
	}
}

// A change RavenDB Cloud refuses must not hold back the others.
func TestARefusedChangeDoesNotHoldBackTheNext(t *testing.T) {
	five := int32(5)
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	cluster := h.getCluster("orders")
	cluster.Spec.InstanceType = "PB20"
	cluster.Spec.Nodes = &five
	if err := h.k8s.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	h.cloud.FailNext("ChangeInstanceType", http.StatusBadRequest)
	h.reconcileCluster("orders")

	if n := h.cloud.CallCount("AddNode"); n != 1 {
		t.Errorf("AddNode called %d times after the instance type was refused, want 1", n)
	}
}

// A restart whose answer was lost may have happened; a second one would reboot
// the node again.
func TestARestartWhoseAnswerWasLostIsNotSentAgain(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	readyCluster(t, h, "orders")
	h.k8sCreate(restartOf("orders", "A"))
	h.cloud.FailNext("RestartNode", http.StatusBadGateway)

	h.reconcileRestart("restart-orders-A")
	h.reconcileRestart("restart-orders-A")

	if n := h.cloud.CallCount("RestartNode"); n != 1 {
		t.Errorf("RestartNode called %d times, want once", n)
	}
}
