package cloudclients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8sfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
)

// TestAccountsShareOneRateLimiter is a behavioural test because there is no
// other kind available: whether two clients share a limiter is invisible from
// the outside except in the timing.
//
// A limiter per client would let a pod running three CloudAccounts call three
// times as fast as one.
func TestAccountsShareOneRateLimiter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"domainName":"acme.ravendb.cloud"}`))
	}))
	t.Cleanup(server.Close)

	const perSecond = 20
	cache := newTestCache(t, server.URL, rate.Limit(perSecond))

	first, err := cache.ClientFor(context.Background(), testAccount("one"))
	if err != nil {
		t.Fatalf("ClientFor(one): %v", err)
	}
	second, err := cache.ClientFor(context.Background(), testAccount("two"))
	if err != nil {
		t.Fatalf("ClientFor(two): %v", err)
	}
	if first == second {
		t.Fatal("two accounts must get two clients; they only share the limiter")
	}

	// Six calls alternating between the accounts. Sharing one bucket at 20/s
	// means five waits of 50ms after the first token, so about 250ms. A limiter
	// each would halve that.
	const calls = 6
	start := time.Now()
	for i := 0; i < calls; i++ {
		target := first
		if i%2 == 1 {
			target = second
		}
		if _, err := target.GetAccountInfo(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	shared := time.Duration(calls-1) * time.Second / perSecond
	if elapsed < shared*8/10 {
		t.Errorf("%d calls across two accounts took %v; a shared pace of %d/s needs about %v, "+
			"so the two clients are pacing independently", calls, elapsed, perSecond, shared)
	}
}

func newTestCache(t *testing.T, endpoint string, limit rate.Limit) *Cache {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("operator scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: "apps"},
		Data:       map[string][]byte{"apiKey": []byte("test-key")},
	}
	return &Cache{
		Kube:      k8sfake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		UserAgent: "test",
		Endpoint:  endpoint,
		Pacing:    Pacing{RateLimit: limit, RetryBaseDelay: time.Millisecond},
	}
}

func testAccount(name string) *v1alpha1.CloudAccount {
	return &v1alpha1.CloudAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec: v1alpha1.CloudAccountSpec{
			APIKeySecretRef: v1alpha1.SecretKeySelector{Name: "key", Key: "apiKey"},
		},
	}
}
