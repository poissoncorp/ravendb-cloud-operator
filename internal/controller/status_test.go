package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// Every RavenDB Cloud status the operator can see, and what each one costs the
// object. Two things are being pinned here and they are easy to get backwards:
// which statuses are worth polling fast for, and which reason each one reads
// as in the STATUS column.
//
// A status that is about to change on its own polls at the transitional
// interval. One that only a human can change polls at the ordinary one, because
// asking again sooner makes calls to be told the same thing.
func TestReportUnlessServing(t *testing.T) {
	const poll = 4 * time.Minute
	// Read through the accessor rather than restated: what this pins is which
	// of the two intervals each status gets, and poll above is far enough from
	// it that picking the wrong one cannot pass.
	transitional := (&v1alpha1.CloudCluster{}).EffectiveTransitionalInterval()

	for _, tc := range []struct {
		status  cloudapi.ClusterStatus
		serving bool
		reason  string
		ready   metav1.ConditionStatus
		requeue time.Duration
	}{
		{cloudapi.StatusActive, true, "", "", 0},
		{cloudapi.StatusCreating, false, v1alpha1.ReasonCreating, metav1.ConditionFalse, transitional},
		{cloudapi.StatusDeployingChanges, false, v1alpha1.ReasonUpdating, metav1.ConditionFalse, transitional},
		// The operator's own terminate takes the deletion path, so a
		// terminating cluster here is somebody else doing it.
		{cloudapi.StatusTerminating, false, v1alpha1.ReasonDeletedInCloud, metav1.ConditionFalse, poll},
		{cloudapi.StatusTerminated, false, v1alpha1.ReasonDeletedInCloud, metav1.ConditionFalse, poll},
		{cloudapi.StatusAwaitingPayment, false, v1alpha1.ReasonAwaitingPayment, metav1.ConditionFalse, poll},
		{cloudapi.StatusError, false, v1alpha1.ReasonCreationFailed, metav1.ConditionFalse, poll},
		// Not a status the API documents. It must still land somewhere
		// describable rather than being read as Active, and Unknown is honest:
		// the operator cannot tell what it means.
		{cloudapi.ClusterStatus("SomethingNew"), false, v1alpha1.ReasonUnexpectedResponse, metav1.ConditionUnknown, poll},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			cluster := &v1alpha1.CloudCluster{
				Spec: v1alpha1.CloudClusterSpec{PollInterval: &metav1.Duration{Duration: poll}},
			}
			r := &CloudClusterReconciler{}

			serving, result := r.reportUnlessServing(context.Background(), cluster, &cloudapi.Cluster{ID: "1-A", Status: tc.status})

			if serving != tc.serving {
				t.Fatalf("serving = %v, want %v", serving, tc.serving)
			}
			if serving {
				if result.RequeueAfter != 0 {
					t.Errorf("a serving cluster asked for a requeue of %s", result.RequeueAfter)
				}
				if len(cluster.Status.Conditions) != 0 {
					t.Errorf("a serving cluster was given %+v by a function that should leave it alone", cluster.Status.Conditions)
				}
				return
			}

			if result.RequeueAfter != tc.requeue {
				t.Errorf("requeueAfter = %s, want %s", result.RequeueAfter, tc.requeue)
			}

			ready := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady)
			if ready == nil {
				t.Fatal("no Ready condition")
			}
			if ready.Status != tc.ready {
				t.Errorf("Ready = %q, want %q", ready.Status, tc.ready)
			}
			if ready.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", ready.Reason, tc.reason)
			}
		})
	}
}

// A status this operator does not know has to be named in the message, since
// its own word for it says nothing more than "unexpected".
func TestAnUnknownCloudStatusIsNamed(t *testing.T) {
	cluster := &v1alpha1.CloudCluster{}
	r := &CloudClusterReconciler{}

	r.reportUnlessServing(context.Background(), cluster, &cloudapi.Cluster{Status: "SomethingNew"})

	if cond := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady); !strings.Contains(cond.Message, "SomethingNew") {
		t.Errorf("message %q does not name the cloud status", cond.Message)
	}
}

// A short spec.pollInterval is a request to be told sooner, and it wins over
// the transitional default rather than being floored by it.
func TestReportUnlessServingHonoursAShortPollInterval(t *testing.T) {
	cluster := &v1alpha1.CloudCluster{
		Spec: v1alpha1.CloudClusterSpec{PollInterval: &metav1.Duration{Duration: 2 * time.Second}},
	}
	r := &CloudClusterReconciler{}

	_, result := r.reportUnlessServing(context.Background(), cluster, &cloudapi.Cluster{Status: cloudapi.StatusCreating})

	if result.RequeueAfter != 2*time.Second {
		t.Errorf("requeueAfter = %s, want the 2s the spec asked for", result.RequeueAfter)
	}
}

// The operator must never write a user's spec back, and the write that did it
// was the finalizer going out as an Update.
//
// Update sends the object whole, and a spec that has been through Go and back
// is not the one that was applied: metav1.Duration re-renders 5m as 5m0s, and a
// struct field carrying omitempty serialises as {} because omitempty does not
// apply to structs. The API server sees a changed spec, bumps generation, and
// the object stops matching what is in anyone's git repository, so a GitOps
// tool reports drift it can never resolve.
//
// Asserted as "no Update reaches the resource" rather than "the spec came back
// equal", because the fake client stores the Go object rather than serialising
// it and so cannot reproduce the round trip. Going back to Update passes a
// comparison and fails this.
func TestFinalizersAreWrittenAsAPatchNotAnUpdate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects []client.Object
		run     func(*harness)
	}{
		{
			"cluster", []client.Object{credentialSecret(), readyAccount(), provisionCluster("gitops")},
			func(h *harness) { h.reconcileCluster("gitops") },
		},
		{
			"account", []client.Object{credentialSecret(), unverifiedAccount()},
			func(h *harness) { h.reconcileAccount(testAccountName) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var updates []string
			h := newHarness(t, tc.objects...)
			h.interceptUpdates(func(obj client.Object) {
				updates = append(updates, fmt.Sprintf("%T %s", obj, obj.GetName()))
			})

			tc.run(h)

			if len(updates) > 0 {
				t.Errorf("the reconcile sent %d Update(s) to the resource itself: %v", len(updates), updates)
			}
		})
	}
}

// observedGeneration has to survive the finalizer write that happens in the
// same pass. It did not: the response to that write replaces the object in
// hand with the server's copy, which carries no status this pass has written,
// so a status computed before it was silently rolled back and the field came
// out empty. Only a real API server showed it, because the fake client's
// Update happened to be harmless.
func TestFirstReconcileRecordsTheGenerationItActedOn(t *testing.T) {
	// One account, because readyAccount and unverifiedAccount are the same
	// object under two names. It is Ready so the cluster can bind, and it still
	// has no finalizer, so this is the pass that adds one.
	account := readyAccount()
	account.Generation = 7
	cluster := provisionCluster("observed")
	cluster.Generation = 9

	h := newHarness(t, credentialSecret(), account, cluster)

	h.reconcileAccount(testAccountName)
	if got := h.getAccount(testAccountName).Status.ObservedGeneration; got != 7 {
		t.Errorf("account observedGeneration = %d, want 7", got)
	}

	// The finalizer goes on in the same pass that acts, and its patch answer
	// must not wipe the generation that pass records.
	h.reconcileCluster("observed")
	if !slices.Contains(h.getCluster("observed").Finalizers, clusterFinalizer) {
		t.Error("the first pass did not add the finalizer")
	}
	if got := h.getCluster("observed").Status.ObservedGeneration; got != 9 {
		t.Errorf("cluster observedGeneration = %d, want 9", got)
	}
}

// TestALosingStatusWriteIsNotAnError is about what the operator says out loud
// when it loses a harmless race.
//
// Status().Update sends the whole object with the resourceVersion the pass read.
// Edit the object while a reconcile is in flight and that write is refused, which
// is optimistic concurrency working. Handing the refusal back to
// controller-runtime logs it at error level with a stack trace and replays the
// whole pass, RavenDB Cloud read included; one lab run produced five of those.
//
// A spec edit is the usual cause, and it queues a reconcile of its own, so the
// object was always going to be revisited. Dropping the stale status is the
// whole correction.
func TestALosingStatusWriteIsNotAnError(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	conflicts := 0
	h.interceptStatusUpdates(func(obj client.Object) error {
		conflicts++
		return apierrors.NewConflict(
			schema.GroupResource{Group: "cloud.ravendb.net", Resource: "cloudclusters"},
			obj.GetName(),
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	})

	// A spec change, so the pass has a status worth writing and therefore
	// something to lose.
	cluster := h.getCluster("orders")
	faster := metav1.Duration{Duration: 90 * time.Second}
	cluster.Spec.PollInterval = &faster
	if err := h.k8s.Update(context.Background(), cluster); err != nil {
		t.Fatalf("editing the spec: %v", err)
	}

	result, err := h.cluster.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cluster),
	})
	if conflicts == 0 {
		t.Fatal("no status write was attempted, so this test staged nothing")
	}
	if err != nil {
		t.Errorf("a refused status write came back as a reconcile error: %v", err)
	}
	// The requeue the pass chose survives, so the object is revisited even when
	// whatever moved it is a change the watch predicate drops, such as another
	// controller adding a finalizer.
	if result.RequeueAfter == 0 {
		t.Error("the pass dropped its requeue as well as its status write")
	}
}

// TestStatusWriteFailedKeepsRealFailures is the other half: only the two
// outcomes that mean "this pass is moot" are swallowed. Anything else is a
// write that should have worked, and hiding it would leave status silently
// behind the object forever.
func TestStatusWriteFailedKeepsRealFailures(t *testing.T) {
	resource := schema.GroupResource{Group: "cloud.ravendb.net", Resource: "cloudclusters"}
	pass := ctrl.Result{RequeueAfter: 5 * time.Minute}

	for _, tc := range []struct {
		name       string
		err        error
		wantResult ctrl.Result
		wantErr    bool
	}{
		{"conflict keeps the requeue", apierrors.NewConflict(resource, "orders", errors.New("modified")), pass, false},
		{"gone needs no requeue", apierrors.NewNotFound(resource, "orders"), ctrl.Result{}, false},
		{"forbidden is a real problem", apierrors.NewForbidden(resource, "orders", errors.New("rbac")), ctrl.Result{}, true},
		{"unavailable is a real problem", apierrors.NewServiceUnavailable("etcd"), ctrl.Result{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := statusWriteFailed(context.Background(), pass, tc.err)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if result != tc.wantResult {
				t.Errorf("result = %+v, want %+v", result, tc.wantResult)
			}
		})
	}
}
