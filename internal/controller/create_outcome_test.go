package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
)

// These guard one rule: a create whose outcome is known leaves nothing behind
// that could later make a stranger's cluster look like this object's own.

// loseFirstCreateAnswer puts a proxy in front of the simulator that lets the
// first create land and answers it with a 502, and gives the harness the
// production retry count, which the rest of the suite turns off.
func loseFirstCreateAnswer(t *testing.T, h *harness) {
	t.Helper()
	sim := h.cloud.Handler()
	var once sync.Once
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lost := false
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/products/create" {
			once.Do(func() { lost = true })
		}
		if lost {
			sim.ServeHTTP(httptest.NewRecorder(), r)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		sim.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	h.cluster.Clients = &cloudclients.Cache{
		Kube:     h.k8s,
		Endpoint: proxy.URL,
		Pacing:   cloudclients.Pacing{RateLimit: rate.Inf, RetryBaseDelay: time.Millisecond},
	}
}

func TestACreateWhoseAnswerIsLostIsFoundRatherThanRefused(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	loseFirstCreateAnswer(t, h)

	if _, err := h.cluster.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "orders"},
	}); err == nil {
		t.Fatal("a create answered with a 502 reported success")
	}
	if n := h.cloud.CallCount("CreateCluster"); n != 1 {
		t.Fatalf("sent the create %d times, want once", n)
	}

	h.reconcileCluster("orders")
	cluster := h.getCluster("orders")
	if cluster.Status.ProductID == "" || cluster.Status.Adopted {
		t.Errorf("productID %q, adopted %v; want the landed cluster, bound as created", cluster.Status.ProductID, cluster.Status.Adopted)
	}
	if got := conditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady); got == v1alpha1.ReasonRefused {
		t.Errorf("Ready = %s for a create that landed", got)
	}
}

func TestARefusedCreateLeavesNoClaimOnTheName(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	}))
	h.cloud.FailNext("CreateCluster", http.StatusBadRequest)
	h.reconcileCluster("orders")
	if got := h.getCluster("orders").Status.ProvisionAttemptedAt; got != nil {
		t.Fatalf("provisionAttemptedAt = %v after a refused create; want it cleared", got)
	}

	// Somebody else then makes a cluster with that name.
	stranger := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders", Tier: "Development"})
	h.reconcileCluster("orders")
	cluster := h.getCluster("orders")
	if cluster.Status.ProductID != "" {
		t.Errorf("bound the stranger's cluster %s", cluster.Status.ProductID)
	}

	if err := h.k8s.Delete(context.Background(), cluster); err != nil {
		t.Fatalf("delete: %v", err)
	}
	h.reconcileCluster("orders")
	if got, _ := h.cloudCluster(stranger.ID); got.Status == "Terminated" || h.cloud.CallCount("TerminateCluster") != 0 {
		t.Error("deleting the object terminated a cluster it never created")
	}
}

func TestDeletingNeverTerminatesAClusterAnotherObjectHolds(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	holder := h.reconcileClusterUntilSettled("orders", 6)

	// An object whose create may have landed, under the name the holder's
	// cluster has.
	copyOf := provisionCluster("copy", func(c *v1alpha1.CloudCluster) {
		c.Spec.DisplayName = "orders"
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
		c.Finalizers = []string{clusterFinalizer}
	})
	h.k8sCreate(copyOf)
	attempted := metav1.Now()
	copyOf.Status.ProvisionAttemptedAt = &attempted
	copyOf.Status.ProvisionAttemptedName = "orders"
	if err := h.k8s.Status().Update(context.Background(), copyOf); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := h.k8s.Delete(context.Background(), copyOf); err != nil {
		t.Fatalf("delete: %v", err)
	}

	h.reconcileCluster("copy")
	if n := h.cloud.CallCount("TerminateCluster"); n != 0 {
		t.Errorf("terminated %d clusters; %s belongs to CloudCluster orders", n, holder.Status.ProductID)
	}
	if err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "copy"}, copyOf); !apierrors.IsNotFound(err) {
		t.Errorf("the object is still there (%v); with nothing of its own to terminate it should be released", err)
	}
}
