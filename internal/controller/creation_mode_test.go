package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
)

func TestTakeOverOnlyNeverCreates(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.CreationMode = v1alpha1.CreationTakeOverOnly
	}))
	h.reconcileCluster("orders")

	if n := h.cloud.CallCount("CreateCluster"); n != 0 {
		t.Errorf("sent %d creates under TakeOverOnly", n)
	}
	if got := conditionReason(h.getCluster("orders").Status.Conditions, v1alpha1.ConditionReady); got != v1alpha1.ReasonClusterNotFound {
		t.Errorf("Ready = %q, want ClusterNotFound", got)
	}
}

func TestCreateOnlyNeverTakesOver(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.CreationMode = v1alpha1.CreationCreateOnly
	}))
	h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})
	h.reconcileCluster("orders")

	cluster := h.getCluster("orders")
	if cluster.Status.ProductID != "" {
		t.Errorf("bound %s under CreateOnly", cluster.Status.ProductID)
	}
	if got := conditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady); got != v1alpha1.ReasonNameTaken {
		t.Errorf("Ready = %q, want NameTaken", got)
	}
}

// deleteTakenOver takes over a seeded cluster under mode, with Terminate, then
// deletes the object.
func deleteTakenOver(t *testing.T, mode v1alpha1.CreationMode) (*harness, *cloudsim.Cluster) {
	t.Helper()
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.CreationMode = mode
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	}))
	seeded := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})
	h.reconcileClusterUntilSettled("orders", 6)
	h.events()
	if err := h.k8s.Delete(context.Background(), h.getCluster("orders")); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	h.reconcileCluster("orders")
	return h, seeded
}

// Terminate ends the cluster an object holds however it came to hold it. It
// used to leave one found under TakeOverOrCreate running, and since only
// status says how a cluster was found, a cluster this object created and then
// found again after a restore from git was left running and billed.
func TestTerminateEndsAClusterTakenOverInEitherMode(t *testing.T) {
	for _, mode := range []v1alpha1.CreationMode{v1alpha1.CreationTakeOverOrCreate, v1alpha1.CreationTakeOverOnly} {
		t.Run(string(mode), func(t *testing.T) {
			h, seeded := deleteTakenOver(t, mode)

			if n := h.cloud.CallCount("TerminateCluster"); n != 1 {
				t.Errorf("TerminateCluster called %d times, want once", n)
			}
			if got, _ := h.cloudCluster(seeded.ID); got.Status == "Active" {
				t.Error("the cluster is still Active")
			}
		})
	}
}

// The usual way out of a takeover nobody meant is deleting the object, and
// under Terminate that ends the cluster, so the warning says to switch to
// Retain first.
func TestAnUnaskedTakeoverUnderTerminateWarnsBeforeTheDelete(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	}))
	h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})
	h.reconcileCluster("orders")

	events := strings.Join(h.events(), "\n")
	if !strings.Contains(events, "set spec.deletionPolicy to Retain before deleting this object") {
		t.Errorf("the takeover warning does not say deleting would terminate; events:\n%s", events)
	}
}

// A takeover nobody asked for warns; one the manifest asked for is routine.
func TestATakeoverWarnsUnlessTheManifestAskedForIt(t *testing.T) {
	for mode, want := range map[v1alpha1.CreationMode]string{
		v1alpha1.CreationTakeOverOrCreate: "Warning " + v1alpha1.EventAdopted,
		v1alpha1.CreationTakeOverOnly:     "Normal " + v1alpha1.EventAdopted,
	} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders", func(c *v1alpha1.CloudCluster) {
				c.Spec.CreationMode = mode
			}))
			h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})
			h.reconcileCluster("orders")

			if events := strings.Join(h.events(), "\n"); !strings.Contains(events, want) {
				t.Errorf("no %q event; events:\n%s", want, events)
			}
		})
	}
}
