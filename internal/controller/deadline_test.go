package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
)

// TestAPassOutOfTimeStillSaysWhy: passTimeout cuts off a pass waiting on a
// RavenDB Cloud that never answers, and the status write ending the pass must
// not share that deadline, or the object goes on saying whatever it said before.
func TestAPassOutOfTimeStillSaysWhy(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders"))
	silent := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(silent.Close)
	h.cluster.Clients = &cloudclients.Cache{
		Kube:     h.k8s,
		Endpoint: silent.URL,
		Pacing:   cloudclients.Pacing{RateLimit: rate.Inf, RetryBaseDelay: time.Millisecond},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := h.cluster.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "orders"},
	}); err == nil {
		t.Fatal("a pass that got no answer reported success")
	}

	cluster := h.getCluster("orders")
	if got := conditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady); got != v1alpha1.ReasonCloudUnreachable {
		t.Errorf("Ready = %q, want %s written after the deadline", got, v1alpha1.ReasonCloudUnreachable)
	}
}

// TestARoutinePollWaitsBehindChangesPeopleMake: when more objects want a turn
// than the operator's pacing lets through, an edit someone just applied goes
// first.
func TestARoutinePollWaitsBehindChangesPeopleMake(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 10)

	result := h.reconcileCluster("orders")
	if result.RequeueAfter == 0 {
		t.Fatal("a settled cluster was not polled again")
	}
	if result.Priority == nil || *result.Priority != handler.LowPriority {
		t.Errorf("the poll has priority %v, want handler.LowPriority", result.Priority)
	}
}
