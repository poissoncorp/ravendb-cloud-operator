package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
)

func (h *harness) reconcileAccount(name string) ctrl.Result {
	h.t.Helper()
	result, err := h.account.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	if err != nil {
		h.t.Fatalf("reconcile account %s: %v", name, err)
	}
	return result
}

func (h *harness) getAccount(name string) *v1alpha1.CloudAccount {
	h.t.Helper()
	var account v1alpha1.CloudAccount
	if err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &account); err != nil {
		h.t.Fatalf("getting CloudAccount %s: %v", name, err)
	}
	return &account
}

// unverifiedAccount is a CloudAccount as the user just applied it: no status.
func unverifiedAccount() *v1alpha1.CloudAccount {
	return &v1alpha1.CloudAccount{
		ObjectMeta: metav1.ObjectMeta{Name: testAccountName, Namespace: testNamespace},
		Spec: v1alpha1.CloudAccountSpec{
			APIKeySecretRef: v1alpha1.SecretKeySelector{Name: testSecretName, Key: "apiKey"},
		},
	}
}

func TestAccountBecomesReadyAndCachesRegions(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())

	h.reconcileAccount(testAccountName)
	account := h.getAccount(testAccountName)

	if !meta.IsStatusConditionTrue(account.Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatalf("account is not Ready: %+v", account.Status.Conditions)
	}
	if account.Status.DomainName != "example.ravendb.cloud" {
		t.Errorf("domainName = %q", account.Status.DomainName)
	}
	if account.Status.DefaultReleaseChannel != "Stable72" {
		t.Errorf("defaultReleaseChannel = %q, want Stable72", account.Status.DefaultReleaseChannel)
	}
	// The mirrored lists are what turn "describe" into an answer to "what can I ask
	// for", so it must be populated for all three providers.
	if len(account.Status.Providers) != 3 {
		t.Fatalf("providers has %d providers, want 3", len(account.Status.Providers))
	}
	if account.Status.RefreshedAt == nil {
		t.Error("refreshedAt was not stamped")
	}
}

func TestAccountListsAreNotRefetchedOnEveryReconcile(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())

	h.reconcileAccount(testAccountName)
	afterFirst := h.cloud.CallCount("ListRegions")
	h.reconcileAccount(testAccountName)
	afterSecond := h.cloud.CallCount("ListRegions")

	if afterFirst == 0 {
		t.Fatal("the first reconcile should have fetched regions")
	}
	// Refetching hourly data on every pass would take turns that cluster
	// reconciles also need.
	if afterSecond != afterFirst {
		t.Errorf("ListRegions calls went from %d to %d; the lists should be cached", afterFirst, afterSecond)
	}
}

func TestAccountNotReadyWhenSecretIsMissing(t *testing.T) {
	h := newHarness(t, unverifiedAccount()) // no credential Secret

	result := h.reconcileAccount(testAccountName)
	account := h.getAccount(testAccountName)

	if meta.IsStatusConditionTrue(account.Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatal("account should not be Ready without a credential")
	}
	cond := meta.FindStatusCondition(account.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonKeyMissing {
		t.Fatalf("Ready condition = %+v, want reason KeyMissing", cond)
	}
	if !contains(cond.Message, testSecretName) {
		t.Errorf("message %q should name the missing Secret", cond.Message)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected a requeue so the account recovers once the Secret appears")
	}
}

func TestAccountNotReadyWhenSecretKeyIsWrong(t *testing.T) {
	secret := credentialSecret()
	secret.Data = map[string][]byte{"token": []byte("oops-wrong-key-name")}
	h := newHarness(t, secret, unverifiedAccount())

	h.reconcileAccount(testAccountName)
	account := h.getAccount(testAccountName)

	cond := meta.FindStatusCondition(account.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", cond)
	}
	if !contains(cond.Message, "apiKey") {
		t.Errorf("message %q should name the key it looked for", cond.Message)
	}
}

func TestAccountWithRejectedKeyIsNotRetriedTightly(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())
	// A 401 from the control plane, not a substituted Go error: whether the
	// operator recognises a rejected key depends on how the client classifies
	// that status, and this way both halves are under test.
	h.cloud.FailNext("GetAccountInfo", http.StatusUnauthorized)

	result := h.reconcileAccount(testAccountName)
	account := h.getAccount(testAccountName)

	cond := meta.FindStatusCondition(account.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonKeyRejected {
		t.Fatalf("Ready condition = %+v, want reason KeyRejected", cond)
	}
	// A rejected key needs a human, so the controller backs off rather than
	// returning an error and hammering the API through the retry queue.
	if result.RequeueAfter < 60_000_000_000 { // 1 minute in nanoseconds
		t.Errorf("RequeueAfter = %v, want a slow backoff for an invalid credential", result.RequeueAfter)
	}
}

func TestAccountSurfacesTransportErrors(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())
	// 500 with retries turned off. A real transport failure would do as well,
	// but this is the same branch: an error the operator cannot attribute to
	// the credential, so it has to be returned for controller-runtime to back
	// off on rather than parked on the object.
	h.cloud.FailNext("GetAccountInfo", http.StatusInternalServerError)

	_, err := h.account.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testAccountName},
	})
	if err == nil {
		t.Fatal("a transport failure should be returned so controller-runtime backs off and retries")
	}

	account := h.getAccount(testAccountName)
	cond := meta.FindStatusCondition(account.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonCloudUnreachable {
		t.Fatalf("Ready condition = %+v, want reason CloudUnreachable", cond)
	}
}

func TestAccountsUsingSecretMapsBackToAccounts(t *testing.T) {
	// Rotating an API key should wake the accounts that use it, rather than
	// waiting out the hourly resync. The two decoys pin the other half of that
	// sentence: an account on a different Secret, and a same-named account in
	// another namespace, must both be left asleep.
	usesAnotherSecret := &v1alpha1.CloudAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "other-account", Namespace: testNamespace},
		Spec: v1alpha1.CloudAccountSpec{
			APIKeySecretRef: v1alpha1.SecretKeySelector{Name: "another-api-key", Key: "apiKey"},
		},
	}
	inAnotherNamespace := &v1alpha1.CloudAccount{
		ObjectMeta: metav1.ObjectMeta{Name: testAccountName, Namespace: "elsewhere"},
		Spec: v1alpha1.CloudAccountSpec{
			APIKeySecretRef: v1alpha1.SecretKeySelector{Name: testSecretName, Key: "apiKey"},
		},
	}
	h := newHarness(t, credentialSecret(), unverifiedAccount(), usesAnotherSecret, inAnotherNamespace)

	requests := h.account.accountsUsingSecret(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace},
	})

	if len(requests) != 1 {
		t.Fatalf("got %d reconcile requests, want exactly 1: %v", len(requests), requests)
	}
	want := types.NamespacedName{Namespace: testNamespace, Name: testAccountName}
	if requests[0].NamespacedName != want {
		t.Errorf("reconcile request = %v, want %v", requests[0].NamespacedName, want)
	}
}

// Deleting a CloudAccount that clusters still name is the failure this
// finalizer exists to prevent: without the credential they cannot reconcile,
// and one with deletionPolicy Terminate cannot even finish deleting, because
// terminating needs the key that just went away.
func TestAccountWaitsForTheClustersUsingIt(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount(),
		provisionCluster("orders"), provisionCluster("billing"))

	h.reconcileAccount(testAccountName)
	if !hasFinalizer(h.getAccount(testAccountName)) {
		t.Fatal("no finalizer after the first reconcile, so a delete would go straight through")
	}

	h.deleteAccount(testAccountName)
	h.reconcileAccount(testAccountName)

	account := h.getAccount(testAccountName)
	if !hasFinalizer(account) {
		t.Fatal("the finalizer was released while two clusters still referenced the account")
	}
	ready := meta.FindStatusCondition(account.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Reason != v1alpha1.ReasonInUse {
		t.Fatalf("Ready condition = %+v, want reason %s", ready, v1alpha1.ReasonInUse)
	}
	// Named, and in a stable order, because the message is how a user finds out
	// what to delete first.
	for _, want := range []string{"billing", "orders"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("message %q does not name %s", ready.Message, want)
		}
	}
	if i, j := strings.Index(ready.Message, "billing"), strings.Index(ready.Message, "orders"); i > j {
		t.Errorf("names are not sorted, so the message reshuffles between reconciles: %q", ready.Message)
	}
}

// And it lets go once they are gone, rather than needing the operator restarted
// or the finalizer removed by hand.
func TestAccountIsReleasedOnceNothingUsesIt(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount(), provisionCluster("orders"))

	h.reconcileAccount(testAccountName)
	h.deleteAccount(testAccountName)
	h.reconcileAccount(testAccountName)
	if !hasFinalizer(h.getAccount(testAccountName)) {
		t.Fatal("released while a cluster still referenced it")
	}

	var cluster v1alpha1.CloudCluster
	key := types.NamespacedName{Namespace: testNamespace, Name: "orders"}
	if err := h.k8s.Get(context.Background(), key, &cluster); err != nil {
		t.Fatalf("getting the cluster: %v", err)
	}
	if err := h.k8s.Delete(context.Background(), &cluster); err != nil {
		t.Fatalf("deleting the cluster: %v", err)
	}

	h.reconcileAccount(testAccountName)

	var account v1alpha1.CloudAccount
	err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testAccountName}, &account)
	if err == nil && hasFinalizer(&account) {
		t.Fatal("the finalizer survived the last cluster going away")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("getting the account: %v", err)
	}
}

// An account nothing ever used must not need a cluster deleted to go away.
func TestAccountWithNoClustersDeletesImmediately(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())

	h.reconcileAccount(testAccountName)
	h.deleteAccount(testAccountName)
	h.reconcileAccount(testAccountName)

	var account v1alpha1.CloudAccount
	err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testAccountName}, &account)
	if err == nil && hasFinalizer(&account) {
		t.Fatal("an unused account still holds its finalizer")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("getting the account: %v", err)
	}
}

func hasFinalizer(account *v1alpha1.CloudAccount) bool {
	return controllerutil.ContainsFinalizer(account, accountFinalizer)
}

func (h *harness) deleteAccount(name string) {
	h.t.Helper()
	account := h.getAccount(name)
	if err := h.k8s.Delete(context.Background(), account); err != nil {
		h.t.Fatalf("deleting CloudAccount %s: %v", name, err)
	}
}

// TestAHeldAccountCountsAClusterThatArrivedAfterTheDelete is the predicate, not
// the counting.
//
// clustersUsing has always listed every CloudCluster naming the account, so the
// count was never wrong when it was taken. What was wrong was when it was taken:
// the watch that wakes a terminating account passed deletions only, so a cluster
// created while the account waited did not wake anything and the message kept
// naming the set from the last deletion.
//
// The user-visible result was an InUse message saying one cluster held the
// account, naming it, while two did. Lab 07 caught it on its first run.
func TestAHeldAccountCountsAClusterThatArrivedAfterTheDelete(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, credentialSecret(), unverifiedAccount(), provisionCluster("first"))

	// Hold the account open, then ask for it to go.
	h.reconcileAccount(testAccountName)
	h.deleteAccount(testAccountName)
	h.reconcileAccount(testAccountName)

	held := meta.FindStatusCondition(h.getAccount(testAccountName).Status.Conditions, v1alpha1.ConditionReady)
	if held == nil || held.Reason != v1alpha1.ReasonInUse {
		t.Fatalf("Ready = %+v, want reason InUse", held)
	}
	if !contains(held.Message, "1 CloudCluster") {
		t.Fatalf("message %q should start out naming one holder", held.Message)
	}

	// A second cluster arrives while the account is waiting. The predicate is
	// what decides whether the account ever hears about it.
	second := provisionCluster("second")
	if err := h.k8s.Create(ctx, second); err != nil {
		t.Fatalf("creating the second cluster: %v", err)
	}

	if !arrivalsAndDepartures().Create(event.CreateEvent{Object: second}) {
		t.Fatal("a cluster arriving does not reach the account's watch, so the message cannot update")
	}
	// And the wake is aimed at the right object.
	requests := h.account.accountAwaitingCluster(ctx, second)
	if len(requests) != 1 || requests[0].Name != testAccountName {
		t.Fatalf("the arrival enqueued %+v, want the account holding it open", requests)
	}

	h.reconcileAccount(testAccountName)
	after := meta.FindStatusCondition(h.getAccount(testAccountName).Status.Conditions, v1alpha1.ConditionReady)
	if after == nil || !contains(after.Message, "2 CloudCluster") {
		t.Errorf("message %q does not count the cluster that arrived after the delete", after.Message)
	}
	if after != nil && !contains(after.Message, "second") {
		t.Errorf("message %q does not name it either", after.Message)
	}
}

// TestAPollingClusterDoesNotWakeAHeldAccount is the other half of the same
// predicate. A cluster writes status every poll, and none of that changes who
// references the account, so passing updates would wake every terminating
// account on a timer for nothing.
func TestAPollingClusterDoesNotWakeAHeldAccount(t *testing.T) {
	cluster := provisionCluster("busy")
	if arrivalsAndDepartures().Update(event.UpdateEvent{ObjectOld: cluster, ObjectNew: cluster}) {
		t.Error("a status write reaches the account's watch, which it has no reason to")
	}
}

// TestACatalogueFromAnotherAccountIsNotKept is the bug a lab found by pointing
// one CloudAccount at two control planes in turn.
//
// The credential is verified on every pass and the catalogue only on its
// interval. Switch the account over and it re-verified at once, reporting the
// new domain, while keeping the previous environment's default release channel
// and regions. A real cluster was then created on Stable72, the stand-in's
// default, on an environment whose own default is Stable62.
//
// The domain changing is the signal that the credential now belongs to a
// different account, whatever the spec says.
func TestACatalogueFromAnotherAccountIsNotKept(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, credentialSecret(), unverifiedAccount())
	h.reconcileAccount(testAccountName)
	fresh := h.getAccount(testAccountName)
	if fresh.Status.RefreshedAt == nil || fresh.Status.DefaultReleaseChannel == "" {
		t.Fatalf("the first pass did not mirror a catalogue: %+v", fresh.Status)
	}
	want := fresh.Status.DefaultReleaseChannel

	// What the object looks like after its key was swapped for another
	// account's: a recent catalogue, recorded against a different domain.
	stale := fresh.DeepCopy()
	stale.Status.DomainName = "elsewhere.example"
	stale.Status.DefaultReleaseChannel = "Stale99"
	stale.Status.ReleaseChannels = []v1alpha1.ReleaseChannelStatus{{Name: "Stale99"}}
	if err := h.k8s.Status().Update(ctx, stale); err != nil {
		t.Fatalf("staging a catalogue from another account: %v", err)
	}
	before := h.cloud.CallCount("ListReleaseChannels")

	h.reconcileAccount(testAccountName)

	got := h.getAccount(testAccountName)
	if h.cloud.CallCount("ListReleaseChannels") == before {
		t.Error("the catalogue was not fetched again although the account it came from changed")
	}
	if got.Status.DefaultReleaseChannel != want {
		t.Errorf("default channel = %q, want %q: the other account's default survived",
			got.Status.DefaultReleaseChannel, want)
	}
}

// TestASpecChangeRefetchesTheCatalogue covers the other way an object moves: a
// new spec.endpoint or a different Secret. Only the spec knows that happened,
// so a spec this catalogue was not taken for is reason enough to take it again.
func TestASpecChangeRefetchesTheCatalogue(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, credentialSecret(), unverifiedAccount())
	h.reconcileAccount(testAccountName)

	stale := h.getAccount(testAccountName).DeepCopy()
	stale.Status.DefaultReleaseChannel = "Stale99"
	stale.Status.ReleaseChannels = []v1alpha1.ReleaseChannelStatus{{Name: "Stale99"}}
	stale.Status.ObservedGeneration = stale.Generation + 1
	if err := h.k8s.Status().Update(ctx, stale); err != nil {
		t.Fatalf("staging a catalogue taken for another spec: %v", err)
	}
	before := h.cloud.CallCount("ListReleaseChannels")

	h.reconcileAccount(testAccountName)

	if h.cloud.CallCount("ListReleaseChannels") == before {
		t.Error("a catalogue taken for a different spec was kept")
	}
	if got := h.getAccount(testAccountName).Status.DefaultReleaseChannel; got == "Stale99" {
		t.Errorf("default channel still %q after the spec moved", got)
	}
}

// TestASettledAccountDoesNotRefetchItsCatalogue is the half that keeps the fix
// honest: the same account, the same spec, a recent catalogue, and no reason to
// spend four calls re-reading it.
func TestASettledAccountDoesNotRefetchItsCatalogue(t *testing.T) {
	h := newHarness(t, credentialSecret(), unverifiedAccount())
	h.reconcileAccount(testAccountName)
	before := h.cloud.CallCount("ListReleaseChannels")

	h.reconcileAccount(testAccountName)

	if n := h.cloud.CallCount("ListReleaseChannels"); n != before {
		t.Errorf("a settled account re-read its catalogue: %d calls, want %d", n, before)
	}
}
