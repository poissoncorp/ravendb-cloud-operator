package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// Each test here is one way things go wrong, and what a person reading
// kubectl describe is told when they do.

// readable fails when a message meant for a person carries what only a log
// should: the Go package, the API path, or a raw JSON body.
func readable(t *testing.T, what, message string) {
	t.Helper()
	if message == "" {
		t.Errorf("%s is empty", what)
	}
	for _, wire := range []string{"cloudapi:", "/api/v1/", `{"`} {
		if strings.Contains(message, wire) {
			t.Errorf("%s carries %q, which a user cannot act on: %q", what, wire, message)
		}
	}
}

// events drains everything recorded so far.
func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func countContaining(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, reason) {
			n++
		}
	}
	return n
}

// editSpec changes a cluster's spec the way kubectl apply does, which moves the
// generation on. The fake client does not do that itself.
func (h *harness) editSpec(name string, edit func(*v1alpha1.CloudCluster)) {
	h.t.Helper()
	cluster := h.getCluster(name)
	edit(cluster)
	cluster.Generation++
	if err := h.k8s.Update(context.Background(), cluster); err != nil {
		h.t.Fatalf("editing %s: %v", name, err)
	}
}

func (h *harness) reconcileClusterWithError(name string) (ctrl.Result, error) {
	h.t.Helper()
	return h.cluster.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
}

func condition(t *testing.T, cluster *v1alpha1.CloudCluster, condType string) metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(cluster.Status.Conditions, condType)
	if cond == nil {
		t.Fatalf("no %s condition: %+v", condType, cluster.Status.Conditions)
	}
	return *cond
}

// resize settles a provisioned cluster, then moves its disk once.
func resizedOnce(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	h.editSpec("orders", func(c *v1alpha1.CloudCluster) { c.Spec.Storage.SizeGB = 30 })
	h.reconcileClusterUntilSettled("orders", 6)
	if got := h.getCluster("orders").Status.Hardware.Storage.SizeGB; got != 30 {
		t.Fatalf("the first resize did not land: size %d", got)
	}
	h.events()
	return h
}

// TestASecondStorageChangeWaitsInsteadOfFailing is lab 05's finding against the
// real service. RavenDB Cloud allows one storage change every six hours on AWS.
// Treated like any other 4xx, the refusal puts the object in Error while the
// cluster serves, tells the user to edit or recreate it, never asks again when
// the window opens, and leaves Synced saying the spec matches.
func TestASecondStorageChangeWaitsInsteadOfFailing(t *testing.T) {
	h := resizedOnce(t)
	h.editSpec("orders", func(c *v1alpha1.CloudCluster) { c.Spec.Storage.SizeGB = 50 })

	result := h.reconcileCluster("orders")
	cluster := h.getCluster("orders")

	if got := h.cloud.CallCount("ChangeStorage"); got != 2 {
		t.Fatalf("ChangeStorage called %d times, want the second one sent and refused", got)
	}
	if ready := condition(t, cluster, v1alpha1.ConditionReady); ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s %s, want True", ready.Status, ready.Reason)
	}
	synced := condition(t, cluster, v1alpha1.ConditionSynced)
	if synced.Status != metav1.ConditionFalse || synced.Reason != v1alpha1.ReasonWaiting {
		t.Fatalf("Synced = %s %s %q, want False Waiting", synced.Status, synced.Reason, synced.Message)
	}
	for _, want := range []string{"resizing storage from 30 GB to 50 GB", "available in", "asks again"} {
		if !strings.Contains(synced.Message, want) {
			t.Errorf("Synced message %q does not say %q", synced.Message, want)
		}
	}
	readable(t, "Synced message", synced.Message)
	if result.RequeueAfter != cluster.EffectivePollInterval() {
		t.Errorf("RequeueAfter = %v, want the poll interval, so the change is asked for again on its own", result.RequeueAfter)
	}
	if n := countContaining(h.events(), v1alpha1.ReasonWaiting); n != 1 {
		t.Errorf("%d Waiting events, want 1 when the wait starts", n)
	}

	// Still inside the window: asked again, refused again, and not announced
	// again.
	h.cloud.State().Advance(time.Hour)
	h.reconcileCluster("orders")
	if n := countContaining(h.events(), v1alpha1.ReasonWaiting); n != 0 {
		t.Errorf("%d more Waiting events while nothing changed", n)
	}

	// The window opens, and the change goes through without anybody touching
	// the object.
	h.cloud.State().Advance(6 * time.Hour)
	h.reconcileCluster("orders")
	if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionSynced).Reason; got != v1alpha1.ReasonApplying {
		t.Errorf("after the window Synced reason = %q, want Applying", got)
	}
	settled := h.reconcileClusterUntilSettled("orders", 6)
	if got := settled.Status.Hardware.Storage.SizeGB; got != 50 {
		t.Errorf("size after the window = %d, want 50", got)
	}
}

// TestAPostponedChangeDoesNotHoldBackTheNextOne: a storage window can be twelve
// hours, and a node count has nothing to do with it.
func TestAPostponedChangeDoesNotHoldBackTheNextOne(t *testing.T) {
	h := resizedOnce(t)
	h.editSpec("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.Storage.SizeGB = 50
		nodes := int32(4)
		c.Spec.Nodes = &nodes
	})

	h.reconcileCluster("orders")

	if got := h.cloud.CallCount("AddNode"); got != 1 {
		t.Errorf("AddNode called %d times, want the node added in the same pass the storage change was postponed", got)
	}
}

// TestSyncedSaysAChangeIsBeingApplied: the pass that sends a change must update
// Synced too, or a cluster mid-resize claims its spec matches.
func TestSyncedSaysAChangeIsBeingApplied(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionSynced).Reason; got != v1alpha1.ReasonApplied {
		t.Fatalf("settled Synced reason = %q, want Applied to start from", got)
	}

	h.editSpec("orders", func(c *v1alpha1.CloudCluster) { c.Spec.Storage.SizeGB = 30 })
	h.reconcileCluster("orders")

	synced := condition(t, h.getCluster("orders"), v1alpha1.ConditionSynced)
	if synced.Status != metav1.ConditionFalse || synced.Reason != v1alpha1.ReasonApplying {
		t.Errorf("Synced = %s %s %q, want False Applying", synced.Status, synced.Reason, synced.Message)
	}
	if !strings.Contains(synced.Message, "20 GB to 30 GB") {
		t.Errorf("Synced message %q does not say what is being applied", synced.Message)
	}
}

// TestARefusedChangeLeavesTheClusterReady: a change RavenDB Cloud refuses says
// nothing about whether the cluster serves, so it must not put the object in
// Error with Ready False, nor stop polling it and let the rest of status go
// stale.
func TestARefusedChangeLeavesTheClusterReady(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	h.events()

	h.editSpec("orders", func(c *v1alpha1.CloudCluster) { c.Spec.Storage.SizeGB = 30 })
	h.cloud.FailNext("ChangeStorage", http.StatusBadRequest)
	result := h.reconcileCluster("orders")
	cluster := h.getCluster("orders")

	if ready := condition(t, cluster, v1alpha1.ConditionReady); ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s %s, want True", ready.Status, ready.Reason)
	}
	synced := condition(t, cluster, v1alpha1.ConditionSynced)
	if synced.Reason != v1alpha1.ReasonRefused || synced.Status != metav1.ConditionFalse {
		t.Fatalf("Synced = %s %s, want False Refused", synced.Status, synced.Reason)
	}
	readable(t, "Synced message", synced.Message)
	if !strings.Contains(synced.Message, "until the spec changes") {
		t.Errorf("Synced message %q does not say what makes the operator try again", synced.Message)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue: the cluster would stop being observed until somebody edits it")
	}
	if countContaining(h.events(), v1alpha1.ReasonRefused) != 1 {
		t.Error("expected one Refused warning")
	}

	// The poll comes round. The cluster is read, the refused change is not
	// sent again, and the refusal still stands.
	h.reconcileCluster("orders")
	if got := h.cloud.CallCount("ChangeStorage"); got != 1 {
		t.Errorf("ChangeStorage called %d times: a refusal that will repeat was asked again", got)
	}
	if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionSynced).Reason; got != v1alpha1.ReasonRefused {
		t.Errorf("Synced reason after the next poll = %q, want the refusal to stand", got)
	}

	// A spec edit is what lets it try again.
	h.editSpec("orders", func(c *v1alpha1.CloudCluster) { c.Spec.Storage.SizeGB = 50 })
	h.reconcileCluster("orders")
	if got := h.cloud.CallCount("ChangeStorage"); got != 2 {
		t.Errorf("ChangeStorage called %d times after the spec changed, want 2", got)
	}
}

// TestARefusalStopsStandingOnceTheClusterMatches: the change was made another
// way, in the portal, and Synced should stop saying it was refused.
func TestARefusalStopsStandingOnceTheClusterMatches(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	h.editSpec("orders", func(c *v1alpha1.CloudCluster) { c.Spec.Storage.SizeGB = 30 })
	h.cloud.FailNext("ChangeStorage", http.StatusBadRequest)
	h.reconcileCluster("orders")
	if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionSynced).Reason; got != v1alpha1.ReasonRefused {
		t.Fatalf("Synced reason = %q, want Refused to start from", got)
	}

	// Somebody resizes it in the portal.
	if !h.cloud.State().Patch(h.getCluster("orders").Status.ProductID, "", 30, nil, "") {
		t.Fatal("could not resize the simulated cluster")
	}
	h.reconcileCluster("orders")

	if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionSynced).Reason; got != v1alpha1.ReasonApplied {
		t.Errorf("Synced reason = %q once the cluster matches, want Applied", got)
	}
}

// TestABlipDoesNotTurnAServingClusterToError: a RavenDB Cloud outage must not
// put every CloudCluster in the namespace into Error with Ready False for as
// long as it lasts, while every database keeps serving.
func TestABlipDoesNotTurnAServingClusterToError(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	h.cloud.FailNext("GetCluster", http.StatusServiceUnavailable)
	if _, err := h.reconcileClusterWithError("orders"); err == nil {
		t.Fatal("a 503 should go back to controller-runtime to be retried with backoff")
	}

	ready := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionUnknown || ready.Reason != v1alpha1.ReasonCloudUnreachable {
		t.Errorf("Ready = %s %s, want Unknown CloudUnreachable", ready.Status, ready.Reason)
	}
	readable(t, "Ready message", ready.Message)
	if !strings.Contains(ready.Message, "503 Service Unavailable") {
		t.Errorf("Ready message %q does not say what came back", ready.Message)
	}
}

// TestARejectedKeyIsNotRetriedTightly: a key revoked while the account is
// Ready must not send every cluster into controller-runtime's backoff, retrying
// a 401 that only a human can fix.
func TestARejectedKeyIsNotRetriedTightly(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	h.cloud.FailNext("GetCluster", http.StatusUnauthorized)
	result, err := h.reconcileClusterWithError("orders")
	if err != nil {
		t.Fatalf("a 401 was handed back to be retried with backoff: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue at all: the cluster would stop being observed")
	}
	ready := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionUnknown || ready.Reason != v1alpha1.ReasonKeyRejected {
		t.Errorf("Ready = %s %s, want Unknown KeyRejected", ready.Status, ready.Reason)
	}
	if !strings.Contains(ready.Message, testAccountName) {
		t.Errorf("Ready message %q does not say which account's key to fix", ready.Message)
	}
}

// TestARejectedKeySaysWhatItCanMeanAndHowToReplaceIt: RavenDB Cloud's 401 has
// no body, so why a key is refused, and where a new one comes from, has to come
// from the operator.
func TestARejectedKeySaysWhatItCanMeanAndHowToReplaceIt(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())
	h.cloud.FailNext("GetAccountInfo", http.StatusUnauthorized)
	h.reconcileAccount(testAccountName)

	ready := meta.FindStatusCondition(h.getAccount(testAccountName).Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Reason != v1alpha1.ReasonKeyRejected {
		t.Fatalf("Ready = %+v, want KeyRejected", ready)
	}
	readable(t, "Ready message", ready.Message)
	for _, want := range []string{
		"401 Unauthorized",
		"Secret apps/ravendb-cloud-api-key",
		`key "apiKey"`,
		"deactivated or past its expiry date",
		"Create a key in the RavenDB Cloud portal and put it in that Secret",
		"as soon as the Secret changes",
	} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("Ready message does not say %q:\n%s", want, ready.Message)
		}
	}
}

// TestAnOutageWhileCreatingIsNotAnInvalidSpec: the one live read a create makes
// was reported as the user's mistake, with a Warning, and retried only at the
// five minute poll.
func TestAnOutageWhileCreatingIsNotAnInvalidSpec(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.cloud.FailNext("ListInstanceTypes", http.StatusServiceUnavailable)

	if _, err := h.reconcileClusterWithError("orders"); err == nil {
		t.Fatal("a 503 should be retried with backoff")
	}

	ready := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady)
	if ready.Reason == v1alpha1.ReasonInvalidSpec {
		t.Fatalf("a RavenDB Cloud outage was reported as an invalid spec: %q", ready.Message)
	}
	if ready.Reason != v1alpha1.ReasonCloudUnreachable {
		t.Errorf("Ready reason = %q, want CloudUnreachable", ready.Reason)
	}
	readable(t, "Ready message", ready.Message)
	if countContaining(h.events(), v1alpha1.ReasonInvalidSpec) != 0 {
		t.Error("a Warning told the user their spec was wrong")
	}
	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("a create went out without the instance types it is validated against")
	}
}

// TestABoundClusterIsUnknownWhileItsKeyIsRejected: False on a cluster that has
// been serving for a month reads as broken, when all that is known is that the
// operator cannot look. It uses the account's word for it, so both objects
// name the same problem the same way.
func TestABoundClusterIsUnknownWhileItsKeyIsRejected(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

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

	h.reconcileCluster("orders")
	ready := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionUnknown || ready.Reason != v1alpha1.ReasonKeyRejected {
		t.Errorf("Ready = %s %s, want Unknown KeyRejected", ready.Status, ready.Reason)
	}
	if !strings.Contains(ready.Message, "the key was rejected") {
		t.Errorf("Ready message %q does not carry the account's own reason", ready.Message)
	}
}

// TestDeletingAnAccountTogetherWithItsClustersFinishes is kubectl delete -f on
// a file that holds both, which is the most ordinary way to tear down.
//
// The account's finalizer held it open, correctly, while a cluster still named
// it, and marked it Ready False while it waited. A cluster with deletionPolicy
// Terminate reads that Ready to decide whether it may use the credential to
// terminate, so it waited for the account, and the account waited for it.
// Neither ever finished.
func TestDeletingAnAccountTogetherWithItsClustersFinishes(t *testing.T) {
	ctx := context.Background()
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)
	h.reconcileAccount(testAccountName)
	h.reconcileClusterUntilSettled("orders", 6)

	// Both deleted at once. The account happens to be reconciled first.
	h.deleteAccount(testAccountName)
	if err := h.k8s.Delete(ctx, h.getCluster("orders")); err != nil {
		t.Fatalf("deleting the cluster: %v", err)
	}
	h.reconcileAccount(testAccountName)

	held := condition(t, &v1alpha1.CloudCluster{Status: v1alpha1.CloudClusterStatus{
		Conditions: h.getAccount(testAccountName).Status.Conditions}}, v1alpha1.ConditionReady)
	if held.Reason != v1alpha1.ReasonInUse {
		t.Fatalf("account Ready reason = %q, want InUse while the cluster holds it", held.Reason)
	}

	h.reconcileCluster("orders")
	if got := h.cloud.CallCount("TerminateCluster"); got != 1 {
		t.Fatalf("TerminateCluster called %d times: the cluster is waiting for an account that is waiting for it", got)
	}
	if err := h.k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "orders"}, &v1alpha1.CloudCluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the cluster is still there after terminating: %v", err)
	}

	h.reconcileAccount(testAccountName)
	var account v1alpha1.CloudAccount
	err := h.k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testAccountName}, &account)
	if err == nil && hasFinalizer(&account) {
		t.Error("the account is still held after its last cluster went")
	}
}

// TestAFailedCatalogueRefreshIsRetriedSoon: with nothing to fall back on, a
// failed refresh left the account with no default release channel for an
// hour, and no cluster that omits spec.releaseChannel could be
// created in that time.
func TestAFailedCatalogueRefreshIsRetriedSoon(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())
	h.cloud.FailNext("ListReleaseChannels", http.StatusServiceUnavailable)

	result := h.reconcileAccount(testAccountName)
	if result.RequeueAfter != catalogueRetryRequeue {
		t.Errorf("RequeueAfter = %v, want %v with no catalogue to fall back on", result.RequeueAfter, catalogueRetryRequeue)
	}

	result = h.reconcileAccount(testAccountName)
	account := h.getAccount(testAccountName)
	if account.Status.DefaultReleaseChannel == "" {
		t.Fatal("the retry did not fetch the catalogue")
	}
	if result.RequeueAfter != account.EffectiveRefreshInterval() {
		t.Errorf("RequeueAfter = %v once refreshed, want the refresh interval", result.RequeueAfter)
	}
}

// TestAWarningMarksWhenAProblemStarts: a Warning on every poll for as long as
// a problem lasts is an API write per object every five minutes, and buries
// the one line saying when it began. The condition carries it from then on.
func TestAWarningMarksWhenAProblemStarts(t *testing.T) {
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) { c.Spec.Region = "mars-north-1" })
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)

	for range 4 {
		h.reconcileCluster("orders")
	}
	if n := countContaining(h.events(), v1alpha1.ReasonInvalidSpec); n != 1 {
		t.Errorf("%d InvalidSpec warnings over three polls of the same problem, want 1", n)
	}
}

// TestRetainSaysRetained: the event a Retain delete leaves behind was recorded
// under reason Terminated, beside a message saying the cluster was left
// running, and lab 06 had to explain that away.
func TestRetainSaysRetained(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	h.events()

	if err := h.k8s.Delete(context.Background(), h.getCluster("orders")); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	h.reconcileCluster("orders")

	events := h.events()
	if countContaining(events, v1alpha1.EventRetained) != 1 {
		t.Errorf("no Retained event among %q", events)
	}
	if countContaining(events, v1alpha1.EventTerminating) != 0 {
		t.Errorf("an event says Terminating about a cluster left running: %q", events)
	}
}

// TestAnActiveClusterWithNoAddressesPublishesNothing: a renamed dns field
// upstream reads as an Active cluster with no nodes to reach, and publishing
// that gave applications an empty url on a cluster that said it was ready.
func TestAnActiveClusterWithNoAddressesPublishesNothing(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	cluster := h.getCluster("orders")
	before := h.getSecret(cluster.ConnectionSecretName()).Data[secretKeyURL]

	observed := &cloudapi.Cluster{ID: cluster.Status.ProductID, Status: cloudapi.StatusActive}
	_, err := h.cluster.publishConnectionSecret(context.Background(), cluster, observed, nil)
	var noAddresses *noEndpointsError
	if !errors.As(err, &noAddresses) {
		t.Fatalf("err = %v, want a noEndpointsError", err)
	}
	if _, err := h.cluster.reportSecretFailure(context.Background(), cluster, err); err != nil {
		t.Errorf("the same answer comes back next time, so it must not be handed back to retry: %v", err)
	}
	if ready := condition(t, cluster, v1alpha1.ConditionReady); ready.Reason != v1alpha1.ReasonUnexpectedResponse {
		t.Errorf("Ready = %s %s, want UnexpectedResponse", ready.Status, ready.Reason)
	}
	if after := h.getSecret(cluster.ConnectionSecretName()).Data[secretKeyURL]; string(after) != string(before) || len(after) == 0 {
		t.Errorf("url went from %q to %q: the last good address should be kept", before, after)
	}
}

// TestARefusalNamesTheSpecFieldToChange: RavenDB Cloud names its own request
// fields, and the manifest has different ones.
func TestARefusalNamesTheSpecFieldToChange(t *testing.T) {
	refusal := &cloudapi.APIError{StatusCode: http.StatusBadRequest, Problems: []cloudapi.Problem{
		{Field: "subdomainName", Message: "Domain name is too long."},
		{Field: "allowedIps[0]", Message: "Invalid IP address."},
		{Field: "somethingNew", Message: "Kept as sent."},
	}}
	got := cloudSaid(refusal)
	want := "spec.subdomain: Domain name is too long. " +
		"spec.networking.allowedIPs: Invalid IP address. " +
		"somethingNew: Kept as sent."
	if got != want {
		t.Errorf("cloudSaid = %q\nwant       %q", got, want)
	}
}

// TestAFailingObjectIsNeverLeftLongerThanAPoll: controller-runtime's own
// ceiling is 1000 seconds, so after a long outage recovery could lag the
// control plane coming back by almost seventeen minutes.
func TestAFailingObjectIsNeverLeftLongerThanAPoll(t *testing.T) {
	limiter := retryBackoff()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "orders"}}

	var wait time.Duration
	for range 40 {
		wait = limiter.When(req)
	}
	if wait != maxRetryBackoff {
		t.Errorf("after 40 failures the wait is %v, want the ceiling %v", wait, maxRetryBackoff)
	}
	if poll := (&v1alpha1.CloudCluster{}).EffectivePollInterval(); maxRetryBackoff > poll {
		t.Errorf("a failing object waits up to %v, longer than the %v a healthy one is polled at", maxRetryBackoff, poll)
	}

	limiter.Forget(req)
	if first := limiter.When(req); first > time.Second {
		t.Errorf("after a success the first retry waits %v; the backoff did not reset", first)
	}
}

// TestClustersAreWorkedOnInParallel: with one worker, one call that got no
// answer held up every other cluster for up to four minutes.
func TestClustersAreWorkedOnInParallel(t *testing.T) {
	opts := controllerOptions()
	if opts.MaxConcurrentReconciles < 2 {
		t.Errorf("MaxConcurrentReconciles = %d: one stuck call would hold up every other cluster", opts.MaxConcurrentReconciles)
	}
	if opts.RateLimiter == nil {
		t.Error("no RateLimiter: the retry ceiling is gone with it")
	}
}

// TestParallelReconcilesDoNotRace runs several clusters through the reconciler
// at once, which is what the workers now do, under -race. The shared client
// cache, its rate limiter and the reconciler itself must be safe for it, and
// no cluster may be created twice.
func TestParallelReconcilesDoNotRace(t *testing.T) {
	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	objects := []client.Object{credentialSecret(), readyAccount()}
	for _, name := range names {
		objects = append(objects, provisionCluster(name))
	}
	h := newHarness(t, objects...)

	for range 5 {
		var wg sync.WaitGroup
		errs := make(chan error, len(names))
		for _, name := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := h.reconcileClusterWithError(name); err != nil {
					errs <- fmt.Errorf("%s: %w", name, err)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	}

	seen := map[string]string{}
	for _, name := range names {
		cluster := h.getCluster(name)
		if !cluster.IsRunning() {
			t.Errorf("%s: %q, want Running", name, conditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady))
		}
		if other, taken := seen[cluster.Status.ProductID]; taken {
			t.Errorf("%s and %s are bound to the same cluster %s", name, other, cluster.Status.ProductID)
		}
		seen[cluster.Status.ProductID] = name
	}
	if got := h.cloud.CallCount("CreateCluster"); got != len(names) {
		t.Errorf("CreateCluster called %d times for %d clusters", got, len(names))
	}
}

// TestOwnSecretWritesDoNotStartAnotherPass: the Secret's creates and updates
// are the controller's own writes, and reacting to them started a second pass
// the moment the first published, against a cache that had not caught up: it
// bound again, announced it again, and lost its status write to a conflict.
// A deleted Secret still has to wake the cluster, so it can be put back.
func TestOwnSecretWritesDoNotStartAnotherPass(t *testing.T) {
	p := deletionsOnly()
	secret := &corev1.Secret{}
	if p.Create(event.CreateEvent{Object: secret}) || p.Update(event.UpdateEvent{ObjectOld: secret, ObjectNew: secret}) {
		t.Error("a Secret create or update starts a pass")
	}
	if !p.Delete(event.DeleteEvent{Object: secret}) {
		t.Error("a deleted Secret does not wake its cluster")
	}
}

// TestADeletionStuckOnItsAccountLogsOnce: saying what the cluster waits for in
// its account and then what the deletion waits for would be two transitions
// and two log lines on every pass it stays stuck.
func TestADeletionStuckOnItsAccountLogsOnce(t *testing.T) {
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)
	h.reconcileClusterUntilSettled("orders", 6)

	// The key is gone and so is the client built from it, as after a restart.
	if err := h.k8s.Delete(context.Background(), credentialSecret()); err != nil {
		t.Fatalf("deleting the Secret: %v", err)
	}
	h.cluster.Clients.Forget(testNamespace, testAccountName)
	if err := h.k8s.Delete(context.Background(), h.getCluster("orders")); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	var lines []string
	ctx := log.IntoContext(context.Background(),
		funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "orders"}}
	for range 3 {
		if _, err := h.cluster.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady).Reason; got != v1alpha1.ReasonDeleting {
		t.Errorf("Ready = %q, want Deleting", got)
	}
	transitions := 0
	for _, line := range lines {
		if strings.Contains(line, `"condition"="Ready"`) {
			transitions++
		}
	}
	if transitions != 1 {
		t.Errorf("%d Ready transitions logged over three stuck passes, want 1: %q", transitions, lines)
	}
}

// A merge patch replaces the whole finalizer list, so a stale copy written
// without a lock would drop a finalizer another controller added since.
func TestAddingTheFinalizerNeverDropsAnotherControllers(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	stale := h.getCluster("orders")

	fresh := h.getCluster("orders")
	fresh.Finalizers = append(fresh.Finalizers, "example.com/backup")
	if err := h.k8s.Update(context.Background(), fresh); err != nil {
		t.Fatalf("adding the other finalizer: %v", err)
	}

	err := h.cluster.addFinalizer(context.Background(), stale)
	if !apierrors.IsConflict(err) {
		t.Errorf("adding the finalizer from a stale copy returned %v, want a conflict", err)
	}
	if got := h.getCluster("orders").Finalizers; !slices.Contains(got, "example.com/backup") {
		t.Errorf("finalizers %v; the other controller's is gone", got)
	}
}

// kubectl delete -f on a file holding the key Secret, the account and a
// Terminate cluster: the Secret has no finalizer and goes first. The cluster
// must still be terminated, with the key the operator already holds.
func TestTerminateSurvivesTheKeySecretGoingFirst(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	}))
	h.reconcileClusterUntilSettled("orders", 6)

	if err := h.k8s.Delete(ctx, credentialSecret()); err != nil {
		t.Fatalf("deleting the Secret: %v", err)
	}
	if err := h.k8s.Delete(ctx, h.getCluster("orders")); err != nil {
		t.Fatalf("deleting the cluster: %v", err)
	}
	h.reconcileCluster("orders")

	if n := h.cloud.CallCount("TerminateCluster"); n != 1 {
		t.Errorf("TerminateCluster called %d times, want once", n)
	}
	var left v1alpha1.CloudCluster
	if err := h.k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "orders"}, &left); !apierrors.IsNotFound(err) {
		t.Errorf("the object is still there (%v); its deletion hangs on a Secret that is gone", err)
	}
}

// kubectl delete namespace: the Secret, the account and the cluster all go at
// once, and the account notices the missing key before the cluster runs. The
// account then reports KeyMissing, and the cluster waited for it to be Ready
// again, which in a namespace being deleted it never is.
func TestTerminateSurvivesTheAccountNoticingTheMissingKeyFirst(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	}))
	h.reconcileAccount(testAccountName)
	h.reconcileClusterUntilSettled("orders", 6)

	if err := h.k8s.Delete(ctx, credentialSecret()); err != nil {
		t.Fatalf("deleting the Secret: %v", err)
	}
	h.reconcileAccount(testAccountName)
	if got := h.getAccount(testAccountName); meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatal("the account is still Ready without its key, so this test is not exercising the teardown order")
	}
	h.deleteAccount(testAccountName)
	if err := h.k8s.Delete(ctx, h.getCluster("orders")); err != nil {
		t.Fatalf("deleting the cluster: %v", err)
	}
	h.reconcileAccount(testAccountName)
	h.reconcileCluster("orders")

	if n := h.cloud.CallCount("TerminateCluster"); n != 1 {
		t.Errorf("TerminateCluster called %d times, want once", n)
	}
	var left v1alpha1.CloudCluster
	if err := h.k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "orders"}, &left); !apierrors.IsNotFound(err) {
		t.Errorf("the object is still there (%v); it waits for an account that is being deleted", err)
	}
}

// A key for another account in the account's Secret made every bound cluster
// read 404 and report DeletedInCloud, and deleting one with Terminate then
// released it without terminating: a cluster gone from the cloud has nothing
// to terminate. It was still running, and billed, in its own account.
func TestAKeyForAnotherAccountLeavesTheClusterAlone(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	}))
	h.reconcileClusterUntilSettled("orders", 6)
	if got := h.getCluster("orders").Status.AccountDomain; got != "example.ravendb.cloud" {
		t.Fatalf("accountDomain = %q, want the account's domain recorded at binding", got)
	}

	// What the account reports once it has verified a key for another account.
	account := h.getAccount(testAccountName)
	account.Status.DomainName = "elsewhere.ravendb.cloud"
	if err := h.k8s.Status().Update(ctx, account); err != nil {
		t.Fatalf("updating the account: %v", err)
	}
	reads := h.cloud.CallCount("GetCluster")
	h.reconcileCluster("orders")

	if n := h.cloud.CallCount("GetCluster"); n != reads {
		t.Errorf("read the cluster %d time(s) with a key for another account", n-reads)
	}
	ready := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady)
	if ready.Reason != v1alpha1.ReasonWaitingForAccount || !strings.Contains(ready.Message, "elsewhere.ravendb.cloud") {
		t.Errorf("Ready = %s %q, want WaitingForAccount naming the other account", ready.Reason, ready.Message)
	}

	if err := h.k8s.Delete(ctx, h.getCluster("orders")); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	h.reconcileCluster("orders")
	if n := h.cloud.CallCount("TerminateCluster"); n != 0 {
		t.Errorf("sent %d terminate(s) through a key for another account", n)
	}
	if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady).Reason; got != v1alpha1.ReasonDeleting {
		t.Errorf("Ready = %q, want Deleting while the keys differ", got)
	}
}

// A refused terminate was sent again on every poll, though RavenDB Cloud
// answers it the same way each time. A rejected key is different: a new key
// wakes the account, not this object, so only a timer notices it.
func TestARefusedTerminateWaitsAndARejectedKeyPolls(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   time.Duration
	}{
		{http.StatusBadRequest, 0},
		{http.StatusUnauthorized, 5 * time.Minute},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
				c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
			}))
			h.reconcileClusterUntilSettled("orders", 6)
			if err := h.k8s.Delete(context.Background(), h.getCluster("orders")); err != nil {
				t.Fatalf("deleting: %v", err)
			}
			h.cloud.FailNext("TerminateCluster", tc.status)

			result := h.reconcileCluster("orders")
			if result.RequeueAfter != tc.want {
				t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, tc.want)
			}
			if got := condition(t, h.getCluster("orders"), v1alpha1.ConditionReady).Reason; got != v1alpha1.ReasonDeleting {
				t.Errorf("Ready = %q, want Deleting", got)
			}
		})
	}
}
