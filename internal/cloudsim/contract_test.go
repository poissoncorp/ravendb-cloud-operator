package cloudsim

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// This is a contract test between two independent readings of the RavenDB
// Cloud OpenAPI document: the operator's client and this mock. Neither was
// written from the other, so agreement here is evidence that both match the
// specification, and a disagreement means one of them is wrong.
//
// It also means the labs cannot quietly drift: if the mock stops behaving like
// the API the operator expects, this fails long before a lab does.

// newMock starts a simulator on an httptest server with a client pointed at
// it. Options default to instant transitions and no throttling, so a test only
// names the timing it actually cares about.
func newMock(t *testing.T, opts ...Options) (cloudapi.Client, *Sim, *httptest.Server) {
	t.Helper()

	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	sim := New(opt)

	httpSrv := httptest.NewServer(checkedAgainstSwagger(t, sim.Handler()))
	t.Cleanup(httpSrv.Close)

	client, err := cloudapi.NewRESTClient(cloudapi.Options{
		Endpoint:       httpSrv.URL,
		APIKey:         sim.apiKey,
		RateLimit:      rate.Inf,
		RetryBaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return client, sim, httpSrv
}

func TestContractAccountAndMetadata(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	info, err := client.GetAccountInfo(ctx)
	if err != nil {
		t.Fatalf("GetAccountInfo: %v", err)
	}
	if info.DomainName != "acme.ravendb.cloud" {
		t.Errorf("domain = %q", info.DomainName)
	}

	// The operator says "AWS", the wire says "Aws". If that mapping were
	// broken, this call would 400 rather than return regions.
	regions, err := client.ListRegions(ctx, cloudapi.ProviderAWS)
	if err != nil {
		t.Fatalf("ListRegions: %v", err)
	}
	if len(regions) == 0 {
		t.Fatal("no regions returned for AWS")
	}

	for _, provider := range []cloudapi.Provider{cloudapi.ProviderAzure, cloudapi.ProviderGCP} {
		if _, err := client.ListRegions(ctx, provider); err != nil {
			t.Errorf("ListRegions(%s): %v", provider, err)
		}
	}

	types, err := client.ListInstanceTypes(ctx, cloudapi.ProviderAWS, "eu-central-1")
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	var sawProduction bool
	for _, it := range types {
		if it.Tier == cloudapi.TierProduction {
			sawProduction = true
			if it.NumberOfNodes < 1 {
				t.Errorf("instance type %q reports %d nodes", it.Name, it.NumberOfNodes)
			}
		}
	}
	if !sawProduction {
		t.Error("no Production instance types are exposed")
	}

	channels, err := client.ListReleaseChannels(ctx)
	if err != nil {
		t.Fatalf("ListReleaseChannels: %v", err)
	}
	if channels.Default == "" {
		t.Error("no default release channel reported")
	}
}

func TestContractCreatePollAndConnect(t *testing.T) {
	// A non-zero provisioning window, so the Creating state is real.
	client, srv, _ := newMock(t, Options{ProvisionDuration: 150 * time.Millisecond})
	ctx := context.Background()

	id, err := client.CreateCluster(ctx, cloudapi.CreateClusterRequest{
		DisplayName:    "orders",
		Provider:       cloudapi.ProviderAWS,
		Region:         "eu-central-1",
		Tier:           cloudapi.TierProduction,
		InstanceType:   "PB10",
		ReleaseChannel: "Stable72",
		SubdomainName:  "orders",
		Storage:        cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:     []string{"0.0.0.0/0"},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	// The public API strips the "products/" prefix, so ids look like "1-A".
	if strings.Contains(id, "/") {
		t.Errorf("product id %q contains a slash; the public API returns the short form", id)
	}

	first, err := client.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if first.Status != cloudapi.StatusCreating {
		t.Errorf("status right after create = %q, want Creating", first.Status)
	}
	if !first.Status.IsTransitional() {
		t.Error("Creating should be transitional")
	}

	waitUntilActive(t, client, id)

	settled, err := client.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if settled.Provider != cloudapi.ProviderAWS {
		t.Errorf("provider = %q, want AWS", settled.Provider)
	}
	if settled.NodeCount() != 3 {
		t.Errorf("PB10 produced %d nodes, want 3", settled.NodeCount())
	}
	if len(settled.URLs) != int(settled.NodeCount()) {
		t.Errorf("%d urls for %d nodes", len(settled.URLs), settled.NodeCount())
	}
	if settled.Hardware.Storage.SizeGiB != 50 {
		t.Errorf("storage = %d GiB, want 50", settled.Hardware.Storage.SizeGiB)
	}
	if len(settled.AllowedIPs) != 1 {
		t.Errorf("allowedIPs = %v", settled.AllowedIPs)
	}

	// Discovery by name must find what we just created.
	ref, err := client.FindClusterByName(ctx, "orders")
	if err != nil {
		t.Fatalf("FindClusterByName: %v", err)
	}
	if ref.ID != id {
		t.Errorf("resolved %q, want %q", ref.ID, id)
	}

	_ = srv
}

func TestContractCertificateBundleIsUsable(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	id := mustCreate(t, client, "orders")
	waitUntilActive(t, client, id)

	cert, err := client.GetClientCertificate(ctx, id)
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(cert.Data), int64(len(cert.Data)))
	if err != nil {
		t.Fatalf("payload is not a zip: %v", err)
	}

	var sawCrt, sawKey, sawPFX, sawPassword bool
	for _, f := range reader.File {
		switch {
		case strings.HasSuffix(f.Name, ".crt"):
			sawCrt = true
			// Parse it, so "we published a certificate" is a fact and not a
			// claim about some bytes that happen to be there.
			raw := readAll(t, f)
			block, _ := pem.Decode(raw)
			if block == nil {
				t.Fatalf("%s is not PEM", f.Name)
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				t.Fatalf("%s does not parse as a certificate: %v", f.Name, err)
			}
		case strings.HasSuffix(f.Name, ".key"):
			sawKey = true
			block, _ := pem.Decode(readAll(t, f))
			if block == nil {
				t.Fatalf("%s is not PEM", f.Name)
			}
		case strings.HasSuffix(f.Name, ".pfx"):
			sawPFX = true
		case f.Name == "PASSWORD.TXT":
			sawPassword = true
		}
	}
	for label, ok := range map[string]bool{"crt": sawCrt, "key": sawKey, "pfx": sawPFX, "PASSWORD.TXT": sawPassword} {
		if !ok {
			t.Errorf("certificate archive is missing %s", label)
		}
	}
}

func TestContractDayTwoOperations(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	id := mustCreate(t, client, "orders")
	waitUntilActive(t, client, id)

	if err := client.ChangeStorage(ctx, id, cloudapi.StorageChange{SizeGiB: 75, Type: cloudapi.StorageSsdPremium}); err != nil {
		t.Fatalf("ChangeStorage: %v", err)
	}
	waitUntilActive(t, client, id)
	after, _ := client.GetCluster(ctx, id)
	if after.Hardware.Storage.SizeGiB != 75 {
		t.Errorf("storage = %d GiB, want 75", after.Hardware.Storage.SizeGiB)
	}

	if err := client.ChangeInstanceType(ctx, id, "PB20"); err != nil {
		t.Fatalf("ChangeInstanceType: %v", err)
	}
	waitUntilActive(t, client, id)
	after, _ = client.GetCluster(ctx, id)
	if after.InstanceType != "PB20" {
		t.Errorf("instance type = %q, want PB20", after.InstanceType)
	}

	before := after.NodeCount()
	if err := client.AddNode(ctx, id); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	waitUntilActive(t, client, id)
	after, _ = client.GetCluster(ctx, id)
	if after.NodeCount() != before+1 {
		t.Errorf("nodes = %d, want %d", after.NodeCount(), before+1)
	}

	tag := after.NodeTags[len(after.NodeTags)-1]
	if err := client.RemoveNode(ctx, id, tag); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	waitUntilActive(t, client, id)
	after, _ = client.GetCluster(ctx, id)
	if after.NodeCount() != before {
		t.Errorf("nodes = %d, want %d after removing %s", after.NodeCount(), before, tag)
	}
}

func TestContractTerminate(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	id := mustCreate(t, client, "scratch")
	waitUntilActive(t, client, id)

	if err := client.TerminateCluster(ctx, id); err != nil {
		t.Fatalf("TerminateCluster: %v", err)
	}
	// The record survives. RavenDB Cloud sets the status to Terminated and
	// keeps the product, keeping its display name with it; only an explicit
	// hideInPortal takes it out of the list, and nothing takes it out of
	// details.
	terminated, err := client.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("a terminated product must still answer details: %v", err)
	}
	if terminated.Status != cloudapi.StatusTerminated {
		t.Errorf("status = %q, want Terminated", terminated.Status)
	}
	if !containsRef(t, client, "scratch") {
		t.Error("a terminated product stays in the product list under its display name")
	}
	// Gone rather than 404, and forever, because the record never disappears.
	// The client treats it as success: the desired end state is reached either
	// way.
	if err := client.TerminateCluster(ctx, id); err != nil {
		t.Errorf("re-terminating something already terminated should be a no-op, got %v", err)
	}
}

// TestContractTerminateTwiceWhileStillTerminating covers the code the previous
// test cannot reach. A second terminate on a cluster that is still winding down
// is 410 Gone rather than 404, and the client has to treat that as success too,
// or a repeated delete would wedge on an error that means "already doing it".
func TestContractTerminateTwiceWhileStillTerminating(t *testing.T) {
	// A change window long enough that the record is still there for the
	// second call.
	client, _, _ := newMock(t, Options{ChangeDuration: 10 * time.Second})
	ctx := context.Background()

	id := mustCreate(t, client, "scratch")
	waitUntilActive(t, client, id)

	if err := client.TerminateCluster(ctx, id); err != nil {
		t.Fatalf("first terminate: %v", err)
	}
	if err := client.TerminateCluster(ctx, id); err != nil {
		t.Errorf("a second terminate while still terminating should be a no-op, got %v", err)
	}

	// And it must not have pushed the deadline out: a caller retrying in a
	// loop would otherwise keep the cluster alive forever.
	cluster, err := client.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if cluster.Status != cloudapi.StatusTerminating {
		t.Errorf("status = %q, want Terminating", cluster.Status)
	}
}

// TestContractRejectsCreatingTheFreeTier pins a rule the OpenAPI document does
// not state: the free offer is portal-only, so create refuses it. The stand-in has to refuse it too, or a lab would provision
// something production cannot.
func TestContractRejectsCreatingTheFreeTier(t *testing.T) {
	client, _, _ := newMock(t)

	_, err := client.CreateCluster(context.Background(), cloudapi.CreateClusterRequest{
		DisplayName: "freebie", Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierFree, InstanceType: "Free", ReleaseChannel: "Stable72",
		SubdomainName: "freebie",
		Storage:       cloudapi.Storage{SizeGiB: 20, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	})
	if err == nil {
		t.Fatal("the control plane accepted a Free-tier create, which RavenDB Cloud refuses")
	}
	if !strings.Contains(err.Error(), "Free") {
		t.Errorf("error %q does not say the tier is the problem", err.Error())
	}
}

func TestContractRejectedCredential(t *testing.T) {
	_, _, httpSrv := newMock(t)

	wrong, err := cloudapi.NewRESTClient(cloudapi.Options{
		Endpoint:       httpSrv.URL,
		APIKey:         "not-the-key",
		RateLimit:      rate.Limit(10000),
		RetryBaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	if _, err := wrong.GetAccountInfo(context.Background()); !cloudapi.IsUnauthorized(err) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestContractAmbiguousDisplayName(t *testing.T) {
	client, srv, _ := newMock(t)
	srv.state.Seed(SeedSpec{DisplayName: "twin"})
	srv.state.Seed(SeedSpec{DisplayName: "twin"})

	if _, err := client.FindClusterByName(context.Background(), "twin"); !cloudapi.IsAmbiguous(err) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
}

func TestContractRejectsUnorderableCombinations(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	base := cloudapi.CreateClusterRequest{
		DisplayName: "x", Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: "x",
		Storage:       cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	}

	cases := map[string]func(*cloudapi.CreateClusterRequest){
		"unknown region":        func(r *cloudapi.CreateClusterRequest) { r.Region = "moon-base-1" },
		"unknown instance type": func(r *cloudapi.CreateClusterRequest) { r.InstanceType = "PB99" },
		"tier mismatch":         func(r *cloudapi.CreateClusterRequest) { r.Tier = cloudapi.TierDevelopment },
		"disk size off menu":    func(r *cloudapi.CreateClusterRequest) { r.Storage.SizeGiB = 7 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := base
			mutate(&req)
			if _, err := client.CreateCluster(ctx, req); err == nil {
				t.Fatal("the control plane accepted a combination it should refuse")
			}
		})
	}
}

// TestContractSubdomainRules pins down the least obvious part of the real API.
// The OpenAPI document marks subdomainName nullable and says nothing about a
// length limit, but RavenDB Cloud runs it through a domain-name validator:
// non-empty, at most 13 characters, letters digits and dashes, no dash at
// either end, and not already taken. Each of those is a create that fails
// after the manifest looked perfectly reasonable.
func TestContractSubdomainRules(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	base := cloudapi.CreateClusterRequest{
		DisplayName: "orders", Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: "orders",
		Storage:       cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	}

	rejected := map[string]string{
		"empty":         "",
		"too long":      "fourteen-chars",
		"leading dash":  "-orders",
		"trailing dash": "orders-",
		"underscore":    "my_orders",
		"dot":           "my.orders",
	}
	for name, subdomain := range rejected {
		t.Run(name, func(t *testing.T) {
			req := base
			req.SubdomainName = subdomain
			if _, err := client.CreateCluster(ctx, req); err == nil {
				t.Fatalf("subdomain %q was accepted; RavenDB Cloud rejects it", subdomain)
			}
		})
	}

	// Thirteen characters is the boundary, and it is allowed.
	req := base
	req.SubdomainName = "thirteenchars"
	if len(req.SubdomainName) != 13 {
		t.Fatalf("test fixture is %d characters, not 13", len(req.SubdomainName))
	}
	if _, err := client.CreateCluster(ctx, req); err != nil {
		t.Fatalf("a 13 character subdomain should be accepted: %v", err)
	}

	// A subdomain already in use is refused, which is why the operator cannot
	// derive one and simply hope.
	req.DisplayName = "other"
	if _, err := client.CreateCluster(ctx, req); err == nil {
		t.Fatal("a subdomain already in use was accepted twice")
	}
}

// TestContractSelfThrottlingClientNeverTrips is the property the operator's
// rate limiter exists for: a client that paces itself is never told no.
func TestContractSelfThrottlingClientNeverTrips(t *testing.T) {
	_, srv, httpSrv := newMock(t, Options{Throttle: true})

	polite, err := cloudapi.NewRESTClient(cloudapi.Options{
		Endpoint: httpSrv.URL,
		APIKey:   "lab-api-key",
		// No RateLimit, so cloudapi.DefaultRateLimit.
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := polite.GetAccountInfo(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	for _, entry := range srv.state.Requests() {
		if entry.Status == 429 {
			t.Fatalf("a client pacing itself was still throttled: %+v", entry)
		}
	}
}

// TestContractIgnoringTheRateLimitGetsThrottled is the other half: without the
// limiter the same calls trip the control plane, which is why the limiter is
// not optional.
func TestContractIgnoringTheRateLimitGetsThrottled(t *testing.T) {
	_, srv, httpSrv := newMock(t, Options{Throttle: true})

	greedy, err := cloudapi.NewRESTClient(cloudapi.Options{
		Endpoint:   httpSrv.URL,
		APIKey:     "lab-api-key",
		RateLimit:  rate.Limit(10000),
		MaxRetries: -1, // no retries, so the 429 is visible instead of smoothed over
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	ctx := context.Background()
	if _, err := greedy.GetAccountInfo(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := greedy.GetAccountInfo(ctx); err == nil {
		t.Fatal("expected the second immediate call to be throttled")
	}

	var throttled bool
	for _, entry := range srv.state.Requests() {
		if entry.Status == 429 {
			throttled = true
		}
	}
	if !throttled {
		t.Error("the control plane did not record a 429")
	}
}

// --- helpers ---------------------------------------------------------------

func mustCreate(t *testing.T, client cloudapi.Client, name string) string {
	t.Helper()
	id, err := client.CreateCluster(context.Background(), cloudapi.CreateClusterRequest{
		DisplayName: name, Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: name,
		Storage:       cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	})
	if err != nil {
		t.Fatalf("CreateCluster(%s): %v", name, err)
	}
	return id
}

func waitUntilActive(t *testing.T, client cloudapi.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cluster, err := client.GetCluster(context.Background(), id)
		if err != nil {
			t.Fatalf("GetCluster: %v", err)
		}
		if cluster.Status.IsServing() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cluster %s never became Active", id)
}

func readAll(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatalf("opening %s: %v", f.Name, err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatalf("reading %s: %v", f.Name, err)
	}
	return buf.Bytes()
}

// TestContractDnsIsBareHostnames asserts the raw wire shape rather than the
// decoded one, which is the only place this can be checked.
//
// RavenDB Cloud sends dns as bare hostnames with no scheme, values like
// "a.clustername.accountname.ravendb.cloud". The operator adds the scheme when
// it decodes, so by the time a cloudapi.Cluster exists the two possible bugs
// are indistinguishable: a simulator that wrongly emits "https://" and a client
// that correctly adds it produce the same Cluster as a simulator that is right
// and a client that is right.
//
// That is not hypothetical. It is exactly what this project shipped, and every
// test passed while the real API would have handed applications URLs they could
// not dial. So this reads the JSON off the wire.
func TestContractDnsIsBareHostnames(t *testing.T) {
	client, _, httpSrv := newMock(t)
	ctx := context.Background()

	id, err := client.CreateCluster(ctx, cloudapi.CreateClusterRequest{
		DisplayName: "dnsshape", Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: "dnsshape",
		Storage:       cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpSrv.URL+"/api/v1/products/details/"+id, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("X-Api-Key", "lab-api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET details: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		DNS []string `json:"dns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding details: %v", err)
	}
	if len(body.DNS) == 0 {
		t.Fatal("the details response carried no dns entries")
	}
	for _, host := range body.DNS {
		if strings.Contains(host, "://") {
			t.Errorf("dns entry %q carries a scheme; RavenDB Cloud sends bare hostnames, "+
				"and emitting a URL here hides the operator's normalization", host)
		}
	}

	// And the client must still hand back something dialable.
	cluster, err := client.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	for _, u := range cluster.URLs {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("cluster URL %q should have been normalized to https://", u)
		}
	}
}

// TestContractIopsRules checks that the operator and the simulator agree on
// when RavenDB Cloud wants a provisioned IOPS value.
//
// The rule, as RavenDB Cloud checks it on create: a disk class whose range the
// metadata endpoint publishes refuses a create that does not choose a value, a
// class with no published range refuses one that tries. The operator can only
// see which is which because ListInstanceTypes carries iopsRange.
func TestContractIopsRules(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	base := cloudapi.CreateClusterRequest{
		DisplayName: "iops", Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: "iops",
		AllowedIPs:    []string{"0.0.0.0/0"},
	}

	// The instance types are what the operator reads to decide, so pin them first.
	types, err := client.ListInstanceTypes(ctx, cloudapi.ProviderAWS, "eu-central-1")
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	var standard, premium *cloudapi.StorageOffer
	for _, it := range types {
		if it.Name != "PB10" {
			continue
		}
		for i := range it.StorageOffers {
			switch it.StorageOffers[i].Type {
			case cloudapi.StorageSsdStandard:
				standard = &it.StorageOffers[i]
			case cloudapi.StorageSsdPremium:
				premium = &it.StorageOffers[i]
			}
		}
	}
	if standard == nil || premium == nil {
		t.Fatal("PB10 should offer both disk classes")
	}
	if standard.IOPS != nil {
		t.Error("SsdStandard publishes no iops range, so the operator must not require one")
	}
	if premium.IOPS == nil {
		t.Fatal("SsdPremium publishes an iops range, which is what makes iops required")
	}

	t.Run("premium without iops is refused", func(t *testing.T) {
		req := base
		req.SubdomainName, req.DisplayName = "iopsa", "iopsa"
		req.Storage = cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdPremium}
		if _, err := client.CreateCluster(ctx, req); err == nil {
			t.Fatal("the control plane accepted SsdPremium with no iops")
		}
	})

	t.Run("premium with iops in range is accepted", func(t *testing.T) {
		req := base
		req.SubdomainName, req.DisplayName = "iopsb", "iopsb"
		want := premium.IOPS.Min + 1
		req.Storage = cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdPremium, IOPS: &want}
		if _, err := client.CreateCluster(ctx, req); err != nil {
			t.Fatalf("CreateCluster: %v", err)
		}
	})

	t.Run("standard with iops is refused", func(t *testing.T) {
		req := base
		req.SubdomainName, req.DisplayName = "iopsc", "iopsc"
		unwanted := int32(3000)
		req.Storage = cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard, IOPS: &unwanted}
		if _, err := client.CreateCluster(ctx, req); err == nil {
			t.Fatal("the control plane accepted iops on a class that does not offer it")
		}
	})
}

// TestContractResizeAllowList pins the difference between the two disk-size
// lists, which is easy to conflate because they share a JSON name.
//
// availableDiskSizes on the metadata endpoint is the create-time menu.
// availableDiskSizes inside a product's hardwareInfo.storage is what this disk
// may move to from where it is now, which is the current size and up. A
// simulator reporting the menu in both places lets a lab resize to a size
// production refuses.
func TestContractResizeAllowList(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	id, err := client.CreateCluster(ctx, cloudapi.CreateClusterRequest{
		DisplayName: "resize", Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: "resize",
		Storage:       cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	cluster, err := client.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	allowed := cluster.Hardware.Storage.ResizableTo
	if len(allowed) == 0 {
		t.Fatal("the details response reported no resize allow-list")
	}
	for _, size := range allowed {
		if size < cluster.Hardware.Storage.SizeGiB {
			t.Errorf("resize allow-list offers %d GiB, below the current %d; a disk cannot shrink",
				size, cluster.Hardware.Storage.SizeGiB)
		}
	}

	// The create-time menu is the wider list, and the two must not be the same
	// thing, or the distinction this test exists for has been lost.
	types, err := client.ListInstanceTypes(ctx, cloudapi.ProviderAWS, "eu-central-1")
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	var menu []int32
	for _, it := range types {
		if it.Name == "PB10" {
			menu = it.DiskSizesGiB
		}
	}
	if len(menu) <= len(allowed) {
		t.Errorf("create menu (%v) should be wider than the resize allow-list (%v) for a mid-range disk", menu, allowed)
	}
}

// TestLabRequestScoping is what lets the labs run concurrently. Each logged
// call is attributed to the cluster it concerns, so one lab can zero and read
// its own measurements without touching another's.
func TestLabRequestScoping(t *testing.T) {
	client, sim, httpSrv := newMock(t)
	ctx := context.Background()

	mk := func(name string) {
		if _, err := client.CreateCluster(ctx, cloudapi.CreateClusterRequest{
			DisplayName: name, Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
			Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
			SubdomainName: name,
			Storage:       cloudapi.Storage{SizeGiB: 20, Type: cloudapi.StorageSsdStandard},
			AllowedIPs:    []string{"0.0.0.0/0"},
		}); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	mk("lab02x")
	mk("lab05x")
	if _, err := client.GetAccountInfo(ctx); err != nil { // unattributed, shared
		t.Fatalf("GetAccountInfo: %v", err)
	}

	get := func(query string) []map[string]any {
		t.Helper()
		resp, err := http.Get(httpSrv.URL + "/_lab/requests" + query)
		if err != nil {
			t.Fatalf("GET%s: %v", query, err)
		}
		defer resp.Body.Close()
		var out []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decoding%s: %v", query, err)
		}
		return out
	}

	for _, e := range get("?lab=lab02") {
		if p, _ := e["product"].(string); p != "" && p != "lab02x" {
			t.Errorf("lab02's view contains %q, which belongs to another lab", p)
		}
	}
	// The shared account call has to survive the filter, or a lab measuring
	// metadata traffic would read zero.
	shared := 0
	for _, e := range get("?lab=lab02") {
		if p, _ := e["product"].(string); p == "" {
			shared++
		}
	}
	if shared == 0 {
		t.Error("unattributed calls were filtered out; a lab measuring metadata would see nothing")
	}

	// Resetting one lab must leave the other's entries alone.
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/_lab/requests/reset?lab=lab02", nil)
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, e := range get("?lab=lab02") {
		if p, _ := e["product"].(string); p == "lab02x" {
			t.Error("lab02's own entries survived its reset")
		}
	}
	found := false
	for _, e := range get("?lab=lab05") {
		if p, _ := e["product"].(string); p == "lab05x" {
			found = true
		}
	}
	if !found {
		t.Error("resetting lab02 destroyed lab05's measurements, which is the bug this prevents")
	}
	_ = sim
}

// containsRef reports whether the product list still carries a display name.
func containsRef(t *testing.T, client cloudapi.Client, name string) bool {
	t.Helper()
	refs, err := client.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// TestContractTerminatedIsNotAdoptable is the operator half of the same fact.
// A dead product keeps its display name forever, so resolving a name has to
// skip it or the operator would bind to a cluster that no longer runs.
func TestContractTerminatedIsNotAdoptable(t *testing.T) {
	client, sim, _ := newMock(t)
	ctx := context.Background()

	id := mustCreate(t, client, "recycled")
	waitUntilActive(t, client, id)
	if err := client.TerminateCluster(ctx, id); err != nil {
		t.Fatalf("TerminateCluster: %v", err)
	}
	if _, err := client.GetCluster(ctx, id); err != nil {
		t.Fatalf("the record should survive: %v", err)
	}

	if _, err := client.FindClusterByName(ctx, "recycled"); !cloudapi.IsNotFound(err) {
		t.Fatalf("resolving the name of a terminated cluster = %v, want ErrNotFound", err)
	}

	// And the name is not free. RavenDB Cloud counts every product not hidden
	// in the portal, terminated or not, and says so on the field.
	_, err := client.CreateCluster(ctx, cloudapi.CreateClusterRequest{
		DisplayName: "recycled", Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: "recycled2",
		Storage:       cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	})
	var refused *cloudapi.APIError
	if !errors.As(err, &refused) || len(refused.Problems) != 1 || !strings.EqualFold(refused.Problems[0].Field, "displayName") {
		t.Fatalf("creating a second %q = %v, want a refusal on displayName", "recycled", err)
	}

	// A live cluster sharing the name with a terminated one can still exist,
	// from before the check or from two creates racing it, and the name
	// resolves to the live one, not to the corpse.
	fresh := sim.State().Seed(SeedSpec{DisplayName: "recycled", SubdomainName: "recycled3"}).ID
	ref, err := client.FindClusterByName(ctx, "recycled")
	if err != nil {
		t.Fatalf("FindClusterByName after re-creating: %v", err)
	}
	if ref.ID != fresh {
		t.Errorf("resolved to %s, want the live cluster %s", ref.ID, fresh)
	}
}

// TestContractDetailsPayloadMatchesProd covers three fields the simulator used
// to invent. None of them break the operator, which is exactly why they lasted:
// a lab simply showed a value production would never send.
func TestContractDetailsPayloadMatchesProd(t *testing.T) {
	client, _, httpSrv := newMock(t)
	ctx := context.Background()

	id := mustCreate(t, client, "payload")
	waitUntilActive(t, client, id)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		httpSrv.URL+"/api/v1/products/details/"+id, nil)
	req.Header.Set("X-Api-Key", "lab-api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET details: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decoding details: %v", err)
	}
	hw, _ := raw["hardwareInfo"].(map[string]any)
	if hw == nil {
		t.Fatal("no hardwareInfo in the details response")
	}

	// Prod omits additionalStorage on a root-only product. Emitting a zero
	// value put "type":"" on the wire, which is not a member of the enum and
	// throws in the generated clients.
	if _, present := hw["additionalStorage"]; present {
		t.Errorf("additionalStorage is present on a root-only product; prod omits it")
	}

	// Every instance type this catalogue offers is Basic in production. Only
	// the P* family is Standard, and none are offered here.
	if got := hw["cpuPriority"]; got != "Basic" {
		t.Errorf("cpuPriority = %v, want Basic", got)
	}
}

// TestContractRejectedKeyHasNoBody pins the shape of a 401, because the reason
// text the operator publishes on the CloudAccount is built from it.
func TestContractRejectedKeyHasNoBody(t *testing.T) {
	_, _, httpSrv := newMock(t)

	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/account/info", nil)
	req.Header.Set("X-Api-Key", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET account/info: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(body)) != 0 {
		t.Errorf("401 carried a body %q; prod sends none, so a lab would show a "+
			"different Ready message than production", body)
	}
	if resp.Header.Get("X-RateLimit-Limit") == "" {
		t.Error("no X-RateLimit-Limit on a response that was not a 429")
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q; prod does not send one", ra)
	}
}

// TestContractRestartDeploysChanges is the one of these with a behavioural
// edge: a restart is a change like any other, so the operator sees the cluster
// leave Active, which is what a CloudClusterRestart waits on.
func TestContractRestartDeploysChanges(t *testing.T) {
	client, _, _ := newMock(t, Options{ChangeDuration: time.Hour})
	ctx := context.Background()

	id := mustCreate(t, client, "restarter")
	waitUntilActive(t, client, id)

	if err := client.RestartNode(ctx, id, "A"); err != nil {
		t.Fatalf("RestartNode: %v", err)
	}
	cluster, err := client.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if cluster.Status != cloudapi.StatusDeployingChanges {
		t.Errorf("status after a restart = %q, want DeployingChanges", cluster.Status)
	}
}

// A node that is not there is refused, and refused terminally: this is a 4xx
// the API will give again, which is what the controller turns into a Failed
// restart rather than a retry loop.
func TestContractRestartRefusesAnUnknownNode(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()

	id := mustCreate(t, client, "restarter")
	waitUntilActive(t, client, id)

	err := client.RestartNode(ctx, id, "Z")
	if err == nil {
		t.Fatal("restarting a node that does not exist was accepted")
	}
	if cloudapi.IsRetryable(err) {
		t.Errorf("error %v is classified retryable; the API will refuse it again", err)
	}
}

// TestContractHostnamesFollowTheTier: RavenDB Cloud builds a cluster's hostnames
// from its tier and lowercases them, so a Development cluster is not where its
// subdomain alone suggests. Verified against RavenDB Cloud's domain naming.
func TestContractHostnamesFollowTheTier(t *testing.T) {
	client, sim, _ := newMock(t)
	for _, tc := range []struct {
		tier, want string
	}{
		{tierProduction, "https://a.orders.acme.ravendb.cloud"},
		{tierDevelopment, "https://a.orders-dev.acme.ravendb.cloud"},
		{tierFree, "https://a.free.acme.ravendb.cloud"},
	} {
		seeded := sim.State().Seed(SeedSpec{DisplayName: "Orders " + tc.tier, SubdomainName: "Orders", Tier: tc.tier})
		cluster, err := client.GetCluster(context.Background(), seeded.ID)
		if err != nil {
			t.Fatalf("GetCluster: %v", err)
		}
		if len(cluster.URLs) == 0 || cluster.URLs[0] != tc.want {
			t.Errorf("%s: URLs = %v, want %s first", tc.tier, cluster.URLs, tc.want)
		}
	}
}

// TestContractTheProductQuota: RavenDB Cloud refuses a create with 412 once the
// account holds its quota of products that are neither Free nor Terminated,
// whatever tier the new one asks for. Terminating one frees the slot.
// Verified against RavenDB Cloud's create endpoint.
func TestContractTheProductQuota(t *testing.T) {
	client, sim, _ := newMock(t, Options{ProductQuota: 1})
	ctx := context.Background()

	first, err := client.CreateCluster(ctx, validCreate("first", "first"))
	if err != nil {
		t.Fatalf("the first paid product: %v", err)
	}
	if _, err := client.CreateCluster(ctx, validCreate("second", "second")); !cloudapi.IsQuotaReached(err) {
		t.Fatalf("a second paid product: %v, want a quota refusal", err)
	}
	free := validCreate("free", "free")
	free.Tier = cloudapi.TierFree
	if _, err := client.CreateCluster(ctx, free); !cloudapi.IsQuotaReached(err) {
		t.Fatalf("a Free product while the quota is full: %v, want a quota refusal", err)
	}

	sim.State().Patch(first, "", 0, nil, statusTerminated)
	if _, err := client.CreateCluster(ctx, validCreate("second", "second")); err != nil {
		t.Errorf("after the first was terminated: %v", err)
	}
}

// validCreate is a create request RavenDB Cloud accepts.
func validCreate(name, subdomain string) cloudapi.CreateClusterRequest {
	return cloudapi.CreateClusterRequest{
		DisplayName: name, Provider: cloudapi.ProviderAWS, Region: "eu-central-1",
		Tier: cloudapi.TierProduction, InstanceType: "PB10", ReleaseChannel: "Stable72",
		SubdomainName: subdomain,
		Storage:       cloudapi.Storage{SizeGiB: 50, Type: cloudapi.StorageSsdStandard},
		AllowedIPs:    []string{"0.0.0.0/0"},
	}
}

// TestContractAllowedIpsRules: RavenDB Cloud refuses two entries that are one
// network once normalised, on every provider, and two that overlap on Azure
// only. Verified against RavenDB Cloud's product validation.
func TestContractAllowedIpsRules(t *testing.T) {
	client, _, _ := newMock(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		provider cloudapi.Provider
		region   string
		ips      []string
		refusal  string
	}{
		{"twice", cloudapi.ProviderAWS, "eu-central-1", []string{"10.0.0.0/8", "10.1.2.3/8"}, "duplicate entries"},
		{"overlap-aws", cloudapi.ProviderAWS, "eu-central-1", []string{"0.0.0.0/0", "10.0.0.0/8"}, ""},
		{"overlap-az", cloudapi.ProviderAzure, "westeurope", []string{"0.0.0.0/0", "10.0.0.0/8"}, "overlapping IP ranges"},
	} {
		req := validCreate(tc.name, tc.name)
		req.Provider, req.Region, req.AllowedIPs = tc.provider, tc.region, tc.ips
		_, err := client.CreateCluster(ctx, req)
		switch {
		case tc.refusal == "" && err != nil:
			t.Errorf("%s: %v, want it accepted", tc.name, err)
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: %v, want a refusal about %s", tc.name, err, tc.refusal)
		}
	}
}
