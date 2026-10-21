package cloudapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/time/rate"
)

// newTestClient builds a REST client against a test server with the rate limit
// and backoff turned down, so tests exercise the logic rather than the clock.
func newTestClient(t *testing.T, handler http.Handler) (Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewRESTClient(Options{
		Endpoint:       server.URL,
		APIKey:         "test-key",
		RateLimit:      rate.Limit(10000),
		RetryBaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}
	return client, server
}

func TestNewRESTClientRequiresAPIKey(t *testing.T) {
	if _, err := NewRESTClient(Options{}); err == nil {
		t.Fatal("expected an error when the API key is empty")
	}
}

func TestGetAccountInfoSendsAPIKeyHeader(t *testing.T) {
	var gotKey, gotPath, gotAgent string
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotPath = r.URL.Path
		gotAgent = r.Header.Get("User-Agent")
		_ = json.NewEncoder(w).Encode(wireAccountInfo{DomainName: "acme.ravendb.cloud"})
	}))

	info, err := client.GetAccountInfo(context.Background())
	if err != nil {
		t.Fatalf("GetAccountInfo: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("X-Api-Key = %q, want %q", gotKey, "test-key")
	}
	if gotPath != "/api/v1/account/info" {
		t.Errorf("path = %q, want /api/v1/account/info", gotPath)
	}
	if gotAgent == "" {
		t.Error("expected a User-Agent identifying the operator")
	}
	if info.DomainName != "acme.ravendb.cloud" {
		t.Errorf("DomainName = %q", info.DomainName)
	}
}

func TestGetClusterMapsProviderAndStatus(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/products/details/abc" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(wireProductDetails{
			ID:            "abc",
			DisplayName:   "orders",
			CloudProvider: "Aws",
			Status:        "Active",
			Tier:          "Production",
			Region:        "eu-central-1",
			InstanceType:  "PB40",
			// Bare hostname, no scheme, which is what RavenDB Cloud actually
			// sends. A scheme-prefixed value here could never catch the
			// operator publishing unusable URLs.
			DNS:      []string{"a.orders.acme.ravendb.cloud"},
			NodeTags: []string{"A", "B", "C"},
			Security: wireSecurity{AllowedIPs: []string{"10.0.0.0/8"}},
			HardwareInfo: wireHardwareInfo{
				CPUs:    4,
				RAM:     16,
				Storage: wireStorage{Size: 256, Type: "SsdPremium"},
			},
		})
	}))

	cluster, err := client.GetCluster(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	// The wire spelling "Aws" must surface as the CRD spelling "AWS".
	if cluster.Provider != ProviderAWS {
		t.Errorf("Provider = %q, want %q", cluster.Provider, ProviderAWS)
	}
	if !cluster.Status.IsServing() {
		t.Errorf("expected Active to be serving, got %q", cluster.Status)
	}
	if cluster.NodeCount() != 3 {
		t.Errorf("NodeCount = %d, want 3", cluster.NodeCount())
	}
	if cluster.Hardware.Storage.SizeGiB != 256 {
		t.Errorf("storage size = %d, want 256", cluster.Hardware.Storage.SizeGiB)
	}
	// The bare hostname must arrive as something a client can dial.
	if len(cluster.URLs) != 1 || cluster.URLs[0] != "https://a.orders.acme.ravendb.cloud" {
		t.Errorf("URLs = %v, want the dns entry with an https scheme", cluster.URLs)
	}
}

// TestNodeURLsAddsTheSchemeTheAPIOmits pins the normalization on its own, since
// the end-to-end value is identical whether both sides are right or both sides
// are wrong. That is precisely how this defect survived: the simulator emitted
// "https://..." and the operator passed it through, so every test agreed while
// the real API would have produced URLs no client could use.
func TestNodeURLsAddsTheSchemeTheAPIOmits(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "the shape RavenDB Cloud actually sends",
			in:   []string{"a.orders.acme.ravendb.cloud", "b.orders.acme.ravendb.cloud"},
			want: []string{"https://a.orders.acme.ravendb.cloud", "https://b.orders.acme.ravendb.cloud"},
		},
		{
			// So that an API which starts sending real URLs does not get two schemes.
			name: "already a URL, left alone",
			in:   []string{"https://a.orders.acme.ravendb.cloud"},
			want: []string{"https://a.orders.acme.ravendb.cloud"},
		},
		{
			name: "blank entries are dropped rather than published as a bare scheme",
			in:   []string{"", "   ", "a.orders.acme.ravendb.cloud"},
			want: []string{"https://a.orders.acme.ravendb.cloud"},
		},
		{name: "nothing in, nothing out", in: nil, want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeURLs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("nodeURLs(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("nodeURLs(%v)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestGetClusterNotFound(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	_, err := client.GetCluster(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if IsRetryable(err) {
		t.Error("a 404 must not be retryable")
	}
}

func TestUnauthorizedIsNotRetryable(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))

	_, err := client.GetAccountInfo(context.Background())
	if !IsUnauthorized(err) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d calls; a rejected key must not be retried", calls)
	}
}

func TestRateLimitedRequestIsRetried(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(wireAccountInfo{DomainName: "acme.ravendb.cloud"})
	}))

	info, err := client.GetAccountInfo(context.Background())
	if err != nil {
		t.Fatalf("GetAccountInfo: %v", err)
	}
	if calls != 3 {
		t.Errorf("made %d calls, want 3 (two 429s then success)", calls)
	}
	if info.DomainName == "" {
		t.Error("expected the retry to return the payload")
	}
}

// A change whose answer was lost may have happened, and RavenDB Cloud takes no
// idempotency key, so sending it again would restart the node twice, or refuse a
// landed create for its own name.
func TestAChangeThatMayHaveTakenEffectIsNotSentAgain(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))

	err := client.RestartNode(context.Background(), "p-1", "A")
	if !MayHaveApplied(err) {
		t.Fatalf("a 502 on a change is %v; want it classed as possibly applied", err)
	}
	if calls != 1 {
		t.Errorf("sent the restart %d times, want once", calls)
	}
}

func TestARateLimitedChangeIsSentAgain(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))

	if err := client.RestartNode(context.Background(), "p-1", "A"); err != nil {
		t.Fatalf("RestartNode: %v", err)
	}
	if calls != 2 {
		t.Errorf("sent the restart %d times, want twice (a 429, then the restart)", calls)
	}
}

func TestAChangeThatNeverConnectedHasNotTakenEffect(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL
	server.Close()
	client, err := NewRESTClient(Options{Endpoint: endpoint, APIKey: "test-key", RateLimit: rate.Inf, RetryBaseDelay: time.Millisecond})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}

	err = client.RestartNode(context.Background(), "p-1", "A")
	if err == nil || MayHaveApplied(err) {
		t.Errorf("a refused connection is %v; want a failure classed as not applied", err)
	}
}

// TestAChangeGivenUpBeforeItWasSentHasNotTakenEffect: a restart still waiting
// for its turn when the pass runs out of time never reached RavenDB Cloud, so
// the next pass may send it.
func TestAChangeGivenUpBeforeItWasSentHasNotTakenEffect(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	t.Cleanup(server.Close)
	busy := rate.NewLimiter(rate.Every(time.Hour), 1)
	busy.Allow()
	client, err := NewRESTClient(Options{Endpoint: server.URL, APIKey: "test-key", Limiter: busy})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err = client.RestartNode(ctx, "p-1", "A")
	if err == nil || MayHaveApplied(err) {
		t.Errorf("a restart given up while waiting is %v; want a failure classed as not applied", err)
	}
	if calls != 0 {
		t.Errorf("RavenDB Cloud saw %d call(s), want none", calls)
	}
}

// TestACallThatGetsNoAnswerEndsWithThePass: the pass deadline reaches the
// request in flight, and nothing is retried after it.
func TestACallThatGetsNoAnswerEndsWithThePass(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls++
		<-r.Context().Done()
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.GetCluster(ctx, "p-1")
	if err == nil {
		t.Fatal("a call nobody answered succeeded")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the call outlived its deadline by %s", took)
	}
	if calls != 1 {
		t.Errorf("sent %d attempt(s), want one: retrying after the deadline cannot succeed", calls)
	}
}

// TestEveryAttemptIsCounted: the metrics are how somebody finds out why a poll
// took twenty minutes, so a retry and its cause have to show up in them.
func TestEveryAttemptIsCounted(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"domainName":"acme.ravendb.cloud"}`))
	}))
	refused := testutil.ToFloat64(attempts.WithLabelValues(http.MethodGet, "503"))
	answered := testutil.ToFloat64(attempts.WithLabelValues(http.MethodGet, "200"))
	retried := testutil.ToFloat64(retries.WithLabelValues(http.MethodGet))

	if _, err := client.GetAccountInfo(context.Background()); err != nil {
		t.Fatalf("GetAccountInfo: %v", err)
	}

	if got := testutil.ToFloat64(attempts.WithLabelValues(http.MethodGet, "503")) - refused; got != 1 {
		t.Errorf("counted %v attempts answered 503, want 1", got)
	}
	if got := testutil.ToFloat64(attempts.WithLabelValues(http.MethodGet, "200")) - answered; got != 1 {
		t.Errorf("counted %v attempts answered 200, want 1", got)
	}
	if got := testutil.ToFloat64(retries.WithLabelValues(http.MethodGet)) - retried; got != 1 {
		t.Errorf("counted %v retries, want 1", got)
	}
}

// TestTheMetricsAreWellFormed runs Prometheus's own linter over them, which is
// what catches a unit missing from a name or a counter not ending in _total.
func TestTheMetricsAreWellFormed(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(Collectors()...)
	attempts.WithLabelValues(http.MethodGet, "200").Add(0)
	attemptSeconds.WithLabelValues(http.MethodGet, "200").Observe(0)
	retries.WithLabelValues(http.MethodGet).Add(0)

	problems, err := testutil.GatherAndLint(registry)
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	for _, p := range problems {
		t.Errorf("%s: %s", p.Metric, p.Text)
	}
}

func TestCreateClusterSendsMappedRequestAndReadsID(t *testing.T) {
	var body wireProductCreateRequest
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/products/create" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		// The real API answers 202 Accepted, not 200.
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(wireProductCreated{ProductID: "7-A"})
	}))

	id, err := client.CreateCluster(context.Background(), CreateClusterRequest{
		DisplayName:    "orders",
		Provider:       ProviderAWS,
		Region:         "eu-central-1",
		Tier:           TierProduction,
		InstanceType:   "PB40",
		ReleaseChannel: "Stable72",
		SubdomainName:  "orders",
		Storage:        Storage{SizeGiB: 50, Type: StorageSsdPremium},
		AllowedIPs:     []string{"0.0.0.0/0"},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if id != "7-A" {
		t.Errorf("id = %q", id)
	}
	if body.CloudProvider != "Aws" {
		t.Errorf("cloudProvider on the wire = %q, want %q", body.CloudProvider, "Aws")
	}
	if body.DiskSize != 50 {
		t.Errorf("diskSize = %d, want 50", body.DiskSize)
	}
	if body.StorageTypeName != "SsdPremium" {
		t.Errorf("storageTypeName = %q", body.StorageTypeName)
	}
}

func TestCreateClusterRejectsEmptyAllowedIPs(t *testing.T) {
	var called bool
	client, _ := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	_, err := client.CreateCluster(context.Background(), CreateClusterRequest{
		DisplayName: "orders", Provider: ProviderAWS, Region: "eu-central-1",
		Tier: TierProduction, InstanceType: "PB40", ReleaseChannel: "Stable72",
		SubdomainName: "orders", Storage: Storage{SizeGiB: 50},
	})
	if err == nil {
		t.Fatal("expected validation to reject an empty allowed IP list")
	}
	if called {
		t.Error("the request must not reach the API when validation fails")
	}
}

func TestTerminateTreatsGoneAsSuccess(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 410 means somebody already terminated it, which is the end state we
		// wanted anyway.
		w.WriteHeader(http.StatusGone)
	}))

	if err := client.TerminateCluster(context.Background(), "abc"); err != nil {
		t.Fatalf("expected 410 Gone to be treated as success, got %v", err)
	}
}

func TestGetClientCertificateKeepsBytesAndMetadata(t *testing.T) {
	payload := []byte("PK\x03\x04 pretend zip")
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/products/security/certificate/abc" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="orders.acme.client.certificate.zip"`)
		_, _ = w.Write(payload)
	}))

	cert, err := client.GetClientCertificate(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if string(cert.Data) != string(payload) {
		t.Error("certificate bytes were modified in transit")
	}
}

func TestFindClusterByNameAmbiguous(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wireProductList{Items: []wireProductListItem{
			{ID: "a", Name: "orders"},
			{ID: "b", Name: "orders"},
		}})
	}))

	_, err := client.FindClusterByName(context.Background(), "orders")
	if !IsAmbiguous(err) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
}

// RavenDB Cloud refuses "orders" when "Orders" exists, because its uniqueness
// check ignores case, so the lookup has to find "Orders" when asked for
// "orders": otherwise the operator creates, and is refused for a name this said
// was free.
func TestFindClusterByNameIgnoresCase(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wireProductList{Items: []wireProductListItem{{ID: "a", Name: "Orders"}}})
	}))

	ref, err := client.FindClusterByName(context.Background(), "orders")
	if err != nil || ref.ID != "a" {
		t.Fatalf("FindClusterByName(orders) = %+v, %v; want the cluster named Orders", ref, err)
	}
}

func TestFindClusterByNameNotFound(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wireProductList{Items: []wireProductListItem{{ID: "a", Name: "other"}}})
	}))

	if _, err := client.FindClusterByName(context.Background(), "orders"); !IsNotFound(err) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRateLimiterThrottlesToConfiguredRate(t *testing.T) {
	// Asserts the limiter is actually in the request path, using a fast rate so
	// the test stays quick.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wireAccountInfo{DomainName: "acme"})
	}))
	defer server.Close()

	client, err := NewRESTClient(Options{
		Endpoint:  server.URL,
		APIKey:    "k",
		RateLimit: rate.Limit(20), // 50ms apart
	})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}

	start := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := client.GetAccountInfo(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// Burst of 1 means 4 calls cost at least 3 intervals.
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("4 calls took %v; the rate limiter does not appear to be applied", elapsed)
	}
}

// TestAnEmptyEndpointMeansProduction: whoever builds a client without an
// endpoint gets the public API, not an error and not a guess.
func TestAnEmptyEndpointMeansProduction(t *testing.T) {
	client, err := NewRESTClient(Options{APIKey: "test-key", Endpoint: "  "})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}
	if got := client.(*restClient).endpoint; got != DefaultEndpoint {
		t.Errorf("endpoint = %q, want %q", got, DefaultEndpoint)
	}
}

func TestDefaultPacingStaysAboveOneSecondPerCall(t *testing.T) {
	gap := time.Duration(float64(time.Second) / float64(DefaultRateLimit))
	if gap <= time.Second {
		t.Errorf("default pacing is one call per %v; it must stay above one second", gap)
	}
	if gap > 2*time.Second {
		t.Errorf("default pacing is one call per %v, which is needlessly slow", gap)
	}
}

// And the limiter is actually applied, rather than merely configured.
func TestSequentialCallsArePaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wireAccountInfo{DomainName: "acme.ravendb.cloud"})
	}))
	t.Cleanup(server.Close)

	const perSecond = 20
	client, err := NewRESTClient(Options{
		Endpoint: server.URL, APIKey: "k",
		RateLimit: rate.Limit(perSecond), RetryBaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}

	start := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := client.GetAccountInfo(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	want := 3 * time.Second / perSecond
	if elapsed := time.Since(start); elapsed < want*8/10 {
		t.Errorf("4 calls took %v, want about %v; the limiter is not being waited on", elapsed, want)
	}
}

// TestTheAPIKeyNeverReachesALog is the one logging test that has to exist.
//
// The operator holds a RavenDB Cloud credential and logs every request it
// makes. Those two facts together are how a key ends up in a log aggregator
// that a wider group can read than can read the Secret it came from. The key
// belongs in exactly one place, the X-Api-Key header, and nowhere else.
//
// This drives real traffic through a real logger and reads back everything
// written: the success path, the retry path, and the terminal-refusal path,
// because each formats its own line.
func TestTheAPIKeyNeverReachesALog(t *testing.T) {
	const secret = "super-secret-key-do-not-log-me"

	var sink bytes.Buffer
	logger := funcr.New(func(prefix, args string) {
		sink.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10}) // verbose, so V(1) lines are captured too
	ctx := logr.NewContext(context.Background(), logger)

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case calls == 1: // retryable, so the retry path logs
			w.WriteHeader(http.StatusTooManyRequests)
		case calls == 2: // success, so the V(1) line logs
			_ = json.NewEncoder(w).Encode(wireAccountInfo{DomainName: "acme.ravendb.cloud"})
		default: // terminal, so the refusal line logs
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewRESTClient(Options{
		Endpoint: server.URL, APIKey: secret,
		RateLimit: rate.Inf, RetryBaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}

	if _, err := client.GetAccountInfo(ctx); err != nil {
		t.Fatalf("the retry-then-succeed call should have worked: %v", err)
	}
	if _, err := client.GetAccountInfo(ctx); err == nil {
		t.Fatal("expected the third response to be a terminal refusal")
	}

	logged := sink.String()
	if logged == "" {
		t.Fatal("nothing was logged, so this test proves nothing")
	}
	if strings.Contains(logged, secret) {
		t.Errorf("the API key appears in the log output:\n%s", logged)
	}
	// The key is a header, so guard the header name too: logging headers
	// wholesale is the likeliest way this regresses.
	if strings.Contains(strings.ToLower(logged), "x-api-key") {
		t.Errorf("the credential header name appears in the log, which suggests headers are being dumped:\n%s", logged)
	}
	// And confirm the lines that should exist do, or the assertions above
	// would pass on an empty-ish log.
	for _, want := range []string{"cloud call", "retrying", "will not be retried"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected a %q line; got:\n%s", want, logged)
		}
	}
}

// TestErrorBodiesAreSafeToPrint covers what happens to a response body on its
// way to a Ready condition and a Kubernetes event, which is where somebody
// reads it out of `kubectl describe` in a terminal.
//
// The body is not necessarily from RavenDB Cloud. spec.endpoint accepts any
// http or https URL, so this is attacker-controllable by anyone who can create
// a CloudAccount.
func TestErrorBodiesAreSafeToPrint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"an ordinary refusal survives intact",
			`{"error":"the selected subdomain name is taken"}`,
			`{"error":"the selected subdomain name is taken"}`},
		{"ANSI escapes are dropped, message kept",
			"\x1b[2J\x1b[1;31mdenied\x1b[0m",
			"[2J[1;31mdenied[0m"},
		{"newlines and tabs become spaces so the message stays one line",
			"line one\nline two\ttabbed",
			"line one line two tabbed"},
		{"a NUL is dropped", "before\x00after", "beforeafter"},
		{"non-ASCII text is preserved", "niepoprawna nazwa", "niepoprawna nazwa"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forDisplay(tc.in, 512); got != tc.want {
				t.Errorf("forDisplay(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("truncation counts runes and never splits one", func(t *testing.T) {
		// Ten 3-byte runes. A byte-wise cut at 512 is irrelevant here; the
		// point is that cutting at 5 leaves five whole runes, not 15 bytes.
		got := forDisplay(strings.Repeat("ł", 10), 5)
		if want := strings.Repeat("ł", 5) + "..."; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%q is not valid UTF-8", got)
		}
	})

	t.Run("an invalid byte cannot reach the output", func(t *testing.T) {
		got := forDisplay("ok\xff\xfebad", 512)
		if !utf8.ValidString(got) {
			t.Errorf("forDisplay let invalid UTF-8 through: %q", got)
		}
	})

	t.Run("the bound holds for a body far over it", func(t *testing.T) {
		got := forDisplay(strings.Repeat("A", 100_000), 512)
		if len([]rune(got)) != 515 {
			t.Errorf("got %d runes, want 512 plus the ellipsis", len([]rune(got)))
		}
	})
}
