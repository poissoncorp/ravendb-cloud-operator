package export

import (
	"bytes"
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"sigs.k8s.io/yaml"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
)

const testAPIKey = "export-test-key"

// newCloud is the real REST client against an in-process simulator, so what
// is exported is what the wire actually carried.
func newCloud(t *testing.T) (cloudapi.Client, *cloudsim.Sim) {
	t.Helper()
	sim := cloudsim.New(cloudsim.Options{APIKey: testAPIKey, Domain: "example.ravendb.cloud"})
	server := httptest.NewServer(sim.Handler())
	t.Cleanup(server.Close)
	client, err := cloudapi.NewRESTClient(cloudapi.Options{
		Endpoint: server.URL, APIKey: testAPIKey, RateLimit: rate.Inf, MaxRetries: -1,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return client, sim
}

// documents splits exported YAML the way kubectl apply -f reads it.
func documents(t *testing.T, out []byte) []manifest {
	t.Helper()
	var docs []manifest
	for _, doc := range bytes.Split(out, []byte("---\n")) {
		var m manifest
		if err := yaml.Unmarshal(doc, &m); err != nil {
			t.Fatalf("the output does not parse as YAML: %v\n%s", err, out)
		}
		docs = append(docs, m)
	}
	return docs
}

func TestAnExportDescribesTheClusterAsItRuns(t *testing.T) {
	cloud, sim := newCloud(t)
	seeded := sim.State().Seed(cloudsim.SeedSpec{DisplayName: "Legacy Prod", SubdomainName: "legacy", InstanceType: "PB20"})

	out, err := Named(context.Background(), cloud, Target{Account: "ravendb-cloud", Namespace: "apps"}, []string{"Legacy Prod"})
	if err != nil {
		t.Fatalf("Named: %v", err)
	}
	docs := documents(t, out)
	if len(docs) != 1 {
		t.Fatalf("%d documents, want 1:\n%s", len(docs), out)
	}
	got := docs[0]

	if got.APIVersion != "cloud.ravendb.net/v1alpha1" || got.Kind != "CloudCluster" {
		t.Errorf("apiVersion %q kind %q, want a CloudCluster", got.APIVersion, got.Kind)
	}
	// The object name has to be a valid Kubernetes name, and the display name,
	// which is the identity, is carried in spec where it can be anything.
	if got.Metadata.Name != "legacy-prod" || got.Metadata.Namespace != "apps" {
		t.Errorf("metadata = %+v, want legacy-prod in apps", got.Metadata)
	}
	spec := got.Spec
	if spec.DisplayName != "Legacy Prod" || spec.AccountRef.Name != "ravendb-cloud" {
		t.Errorf("displayName %q accountRef %q", spec.DisplayName, spec.AccountRef.Name)
	}
	if spec.Tier != v1alpha1.TierProduction || spec.InstanceType != "PB20" ||
		spec.Provider != v1alpha1.ProviderAWS || spec.Region != "eu-central-1" || spec.ReleaseChannel != "Stable72" {
		t.Errorf("shape = %+v, want what the seeded cluster runs", spec)
	}
	if spec.Nodes == nil || *spec.Nodes != int32(len(seeded.NodeTags)) {
		t.Errorf("nodes = %v, want %d", spec.Nodes, len(seeded.NodeTags))
	}
	if spec.Storage.SizeGB != 20 || spec.Storage.Type != v1alpha1.StorageSsdStandard {
		t.Errorf("storage = %+v, want the seeded 20 GiB SsdStandard", spec.Storage)
	}
	if len(spec.Networking.AllowedIPs) != 1 || spec.Networking.AllowedIPs[0] != "0.0.0.0/0" {
		t.Errorf("allowedIPs = %v", spec.Networking.AllowedIPs)
	}
	// Full, because the manifest matches: applying it changes nothing, and
	// from then on editing it is how the cluster is changed.
	if spec.CreationMode != v1alpha1.CreationTakeOverOnly {
		t.Errorf("creationMode = %q, want TakeOverOnly: applying this again must never create a cluster", spec.CreationMode)
	}
	if spec.ManagementPolicy != v1alpha1.ManagementFull {
		t.Errorf("managementPolicy = %q, want Full", spec.ManagementPolicy)
	}
	// Only sent on a create, and this cluster exists.
	if spec.Subdomain != "" {
		t.Errorf("subdomain = %q, want it left out", spec.Subdomain)
	}
	if !strings.Contains(string(out), seeded.ID) {
		t.Error("the header comment does not say which cluster the manifest came from")
	}
	if strings.Contains(string(out), "status:") || strings.Contains(string(out), "creationTimestamp") {
		t.Errorf("the manifest carries noise nobody should commit:\n%s", out)
	}
}

func TestAnExportOfAMissingNameWritesNothing(t *testing.T) {
	cloud, sim := newCloud(t)
	sim.State().Seed(cloudsim.SeedSpec{DisplayName: "orders"})

	out, err := Named(context.Background(), cloud, Target{Account: "a"}, []string{"orders", "nope"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want one naming the missing cluster", err)
	}
	if len(out) != 0 {
		t.Errorf("wrote %d bytes despite the error; half a file is worse than none", len(out))
	}
}

// TestExportingAllSkipsTerminatedClusters: a terminated cluster keeps its
// record, and a manifest for it would bind an object to something that no
// longer runs.
func TestExportingAllSkipsTerminatedClusters(t *testing.T) {
	cloud, sim := newCloud(t)
	sim.State().Seed(cloudsim.SeedSpec{DisplayName: "orders"})
	sim.State().Seed(cloudsim.SeedSpec{DisplayName: "billing"})
	gone := sim.State().Seed(cloudsim.SeedSpec{DisplayName: "old"})
	if err := sim.State().Terminate(gone.ID, false); err != nil {
		t.Fatalf("terminating: %v", err)
	}
	sim.State().Advance(time.Hour)

	out, err := All(context.Background(), cloud, Target{Account: "a"})
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var names []string
	for _, doc := range documents(t, out) {
		names = append(names, doc.Spec.DisplayName)
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "billing,orders" {
		t.Errorf("exported %v, want billing and orders", names)
	}
}

func TestObjectName(t *testing.T) {
	for in, want := range map[string]string{
		"orders":         "orders",
		"Legacy Prod":    "legacy-prod",
		"eu_orders.v2":   "eu-orders.v2",
		"--Edge--":       "edge",
		"!!!":            "cloudcluster",
		"Ünïcode Orders": "n-code-orders",
		// Cut to a label value's 63, then trimmed of the dash the cut left.
		strings.Repeat("a", 62) + "-tail": strings.Repeat("a", 62),
	} {
		if got := objectName(in); got != want {
			t.Errorf("objectName(%q) = %q, want %q", in, got, want)
		}
	}
}
