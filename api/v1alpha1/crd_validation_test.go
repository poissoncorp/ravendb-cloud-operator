package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
)

// These tests install the generated CRDs into a real kube-apiserver and check
// that the CEL validation rules do what the comments in the types claim.
//
// The rules are the load-bearing part of the CRD design: they are what stops a
// user from writing a manifest that cannot mean anything. Asserting them
// against the actual API machinery, rather than by reading the YAML, is the
// only way to know they compile and fire.
//
// Requires envtest binaries. Run with:
//
//	make test-crd
//
// and the tests skip cleanly when KUBEBUILDER_ASSETS is not set.

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Every test here skips and the package still reports "ok", which is
		// the most misleading output a run can produce: it looks like the
		// admission rules were verified when no API server existed.
		//
		// Warning from here does not help, because go test buffers a passing
		// package's output. The `test-unit` target says it instead, where a
		// human is actually looking.
		os.Exit(m.Run())
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic("starting envtest: " + err.Error())
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic("building client: " + err.Error())
	}

	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8sClient == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; run 'make test-crd'")
	}
}

func newNamespace(t *testing.T) string {
	t.Helper()
	// Derive a legal DNS label from the test name. Enumerating the characters to
	// replace does not work: a subtest name is prose, and the first one
	// containing a comma failed here with an error about metadata.name rather
	// than about the rule under test. Keep the safe characters instead of
	// guessing at the unsafe ones.
	var b strings.Builder
	for _, r := range strings.ToLower(t.Name()) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		// Collapse any run of separators into a single dash.
		if n := b.Len(); n == 0 || b.String()[n-1] != '-' {
			b.WriteRune('-')
		}
	}
	name := "test-" + b.String()
	if len(name) > 60 {
		name = name[:60]
	}
	name = strings.Trim(name, "-")
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(context.Background(), ns); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating namespace: %v", err)
	}
	return name
}

func baseCluster(namespace, name string) *v1alpha1.CloudCluster {
	return &v1alpha1.CloudCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.CloudClusterSpec{
			AccountRef: v1alpha1.LocalObjectReference{Name: "acct"},
			Tier:       v1alpha1.TierProduction,
			Provider:   v1alpha1.ProviderAWS,
			Region:     "eu-central-1",
			Storage:    v1alpha1.StorageSpec{SizeGB: 128},
			Networking: v1alpha1.NetworkingSpec{AllowedIPs: []string{"0.0.0.0/0"}},
		},
	}
}

// holdCluster records in status that the object has found its cluster, which
// is when its identity fields stop being editable. Status goes through its own
// subresource, as the operator writes it.
func holdCluster(t *testing.T, cluster *v1alpha1.CloudCluster) {
	t.Helper()
	cluster.Status.ProductID = "1-A"
	if err := k8sClient.Status().Update(context.Background(), cluster); err != nil {
		t.Fatalf("recording the cluster in status: %v", err)
	}
}

// TestCRDRequiresOnlyAnAccountAndATier pins the smallest manifest the API
// server takes: which account, and which tier. Everything else is defaulted,
// resolved by the operator, or read from the cluster the name finds.
//
// Free is the case that matters. RavenDB Cloud creates Free clusters in the
// portal only (its public API refuses them), so a Free
// CloudCluster can only ever take over one that already exists, and it has
// nothing to say beyond its tier. Refusing Free here would leave such a
// cluster out of reach; instead the operator reports ClusterNotFound until a
// Free cluster by that name exists.
//
// Tier stays required all the same. On a cluster that already exists it is what
// confirms the name found the right one.
func TestCRDRequiresOnlyAnAccountAndATier(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	// Unstructured, so that exactly these keys reach the API server. The typed
	// struct would also send an empty storage and networking block.
	cases := []struct {
		name     string
		spec     map[string]any
		accepted bool
	}{
		{"a Free cluster to take over", map[string]any{
			"accountRef": map[string]any{"name": "acct"},
			"tier":       "Free",
		}, true},
		{"no tier", map[string]any{
			"accountRef": map[string]any{"name": "acct"},
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &unstructured.Unstructured{}
			cluster.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("CloudCluster"))
			cluster.SetNamespace(newNamespace(t))
			cluster.SetName("minimal")
			if err := unstructured.SetNestedMap(cluster.Object, tc.spec, "spec"); err != nil {
				t.Fatalf("building spec: %v", err)
			}

			err := k8sClient.Create(ctx, cluster)
			if tc.accepted {
				if err != nil {
					t.Fatalf("%v should be accepted: %v", tc.spec, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%v should be refused", tc.spec)
			}
			if !strings.Contains(err.Error(), "spec.tier") {
				t.Errorf("message %q does not blame spec.tier", err.Error())
			}
		})
	}
}

func TestCRDRejectsChangingImmutableFields(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		mutate  func(*v1alpha1.CloudCluster)
		message string
		// field is where fieldPath on the rule should put the blame. Without
		// it every one of these reads "spec: Invalid value: object", which
		// names the whole spec rather than the field to fix.
		field string
	}{
		{"region", func(c *v1alpha1.CloudCluster) { c.Spec.Region = "us-east-1" }, "region cannot change", "spec.region"},
		{"provider", func(c *v1alpha1.CloudCluster) { c.Spec.Provider = v1alpha1.ProviderGCP }, "provider cannot change", "spec.provider"},
		{"tier", func(c *v1alpha1.CloudCluster) { c.Spec.Tier = v1alpha1.TierFree }, "tier cannot change", "spec.tier"},
		{"subdomain", func(c *v1alpha1.CloudCluster) { c.Spec.Subdomain = "elsewhere" }, "subdomain cannot change", "spec.subdomain"},
		{"displayName", func(c *v1alpha1.CloudCluster) { c.Spec.DisplayName = "somebody-elses" }, "displayName cannot change", "spec.displayName"},
		{"creationMode", func(c *v1alpha1.CloudCluster) { c.Spec.CreationMode = v1alpha1.CreationCreateOnly }, "creationMode cannot change", "spec.creationMode"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			cluster := baseCluster(ns, "immutable")
			// Set here rather than in baseCluster, which has to leave both out
			// for TestCRDRejectsAddingAnOmittedIdentityField.
			cluster.Spec.Subdomain = "pinned"
			cluster.Spec.DisplayName = "pinned-name"
			if err := k8sClient.Create(ctx, cluster); err != nil {
				t.Fatalf("creating: %v", err)
			}
			holdCluster(t, cluster)
			tc.mutate(cluster)
			err := k8sClient.Update(ctx, cluster)
			if err == nil {
				t.Fatalf("the API server allowed changing %s, which RavenDB Cloud cannot do", tc.name)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("message %q does not explain why", err.Error())
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("message %q does not blame %s", err.Error(), tc.field)
			}
		})
	}
}

// TestCRDRejectsAddingAnOmittedIdentityField closes the hole the table above
// cannot see. An "immutable" rule written as `!has(oldSelf.x) || ...` only
// fires when the field was already present, so a field omitted at create could
// be set to anything once the object holds its cluster, and the rule would
// wave it through.
//
// For subdomain and displayName that is not cosmetic. displayName is the name
// the operator finds its cluster by, and both feed the create request, so one
// appearing later could point this object at a different cluster: the operator
// would take over a stranger's, or create a second one and bill for both.
func TestCRDRejectsAddingAnOmittedIdentityField(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		mutate  func(*v1alpha1.CloudCluster)
		message string
	}{
		{"subdomain", func(c *v1alpha1.CloudCluster) { c.Spec.Subdomain = "sneaky" }, "subdomain cannot change"},
		{"displayName", func(c *v1alpha1.CloudCluster) { c.Spec.DisplayName = "somebody-elses" }, "displayName cannot change"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			// baseCluster leaves both of these unset, which is the whole point.
			cluster := baseCluster(ns, "omitted")
			if err := k8sClient.Create(ctx, cluster); err != nil {
				t.Fatalf("creating: %v", err)
			}
			if got := clusterField(cluster, tc.name); got != "" {
				t.Fatalf("%s was %q after create, so this test is not exercising the omitted case", tc.name, got)
			}
			holdCluster(t, cluster)

			tc.mutate(cluster)
			err := k8sClient.Update(ctx, cluster)
			if err == nil {
				t.Fatalf("the API server allowed setting %s after it was omitted at create", tc.name)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("message %q does not explain why", err.Error())
			}
		})
	}
}

// TestCRDRejectsRemovingAnIdentityField is the other direction: present at
// create, absent on update. The old rule allowed that too, because it only
// compared values when both sides had one.
func TestCRDRejectsRemovingAnIdentityField(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		remove  func(*v1alpha1.CloudCluster)
		message string
	}{
		{"subdomain", func(c *v1alpha1.CloudCluster) { c.Spec.Subdomain = "" }, "subdomain cannot change"},
		{"displayName", func(c *v1alpha1.CloudCluster) { c.Spec.DisplayName = "" }, "displayName cannot change"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			cluster := baseCluster(ns, "removal")
			cluster.Spec.Subdomain = "pinned"
			cluster.Spec.DisplayName = "pinned-name"
			if err := k8sClient.Create(ctx, cluster); err != nil {
				t.Fatalf("creating: %v", err)
			}
			holdCluster(t, cluster)

			tc.remove(cluster)
			err := k8sClient.Update(ctx, cluster)
			if err == nil {
				t.Fatalf("the API server allowed removing %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("message %q does not explain why", err.Error())
			}
		})
	}
}

func clusterField(c *v1alpha1.CloudCluster, name string) string {
	switch name {
	case "subdomain":
		return c.Spec.Subdomain
	case "displayName":
		return c.Spec.DisplayName
	}
	return ""
}

// TestCRDAllowsPinningAnInheritedProviderAndRegion is the deliberate gap in the
// provider and region rules. Unlike subdomain and displayName they may be added
// after create, so a manifest that inherited them from the CloudAccount
// defaults can later write down the value it got. Once written they are pinned:
// TestCRDRejectsChangingImmutableFields covers a change, and this covers taking
// one away again.
func TestCRDAllowsPinningAnInheritedProviderAndRegion(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()

	cluster := baseCluster(ns, "inherited")
	cluster.Spec.Provider = ""
	cluster.Spec.Region = ""
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating: %v", err)
	}
	holdCluster(t, cluster)

	cluster.Spec.Provider = v1alpha1.ProviderAWS
	cluster.Spec.Region = "eu-central-1"
	if err := k8sClient.Update(ctx, cluster); err != nil {
		t.Fatalf("the API server refused writing down an inherited provider and region: %v", err)
	}

	cluster.Spec.Region = ""
	err := k8sClient.Update(ctx, cluster)
	if err == nil {
		t.Fatal("the API server allowed removing region once it was set")
	}
	if !strings.Contains(err.Error(), "region cannot change") {
		t.Errorf("message %q does not explain why", err.Error())
	}
}

func TestCRDAllowsChangingMutableFields(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()

	cluster := baseCluster(ns, "mutable")
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating: %v", err)
	}

	// These three are exactly the reshaping operations the cloud API offers,
	// so the CRD must allow them.
	nodes := int32(5)
	cluster.Spec.Nodes = &nodes
	cluster.Spec.Storage.SizeGB = 512
	cluster.Spec.InstanceType = "PB40"

	if err := k8sClient.Update(ctx, cluster); err != nil {
		t.Fatalf("the API server rejected a change the cloud API supports: %v", err)
	}
}

func TestCRDRejectsMovingBetweenAccounts(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()

	cluster := baseCluster(ns, "rehome")
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating: %v", err)
	}
	holdCluster(t, cluster)

	cluster.Spec.AccountRef.Name = "another-account"
	err := k8sClient.Update(ctx, cluster)
	if err == nil {
		t.Fatal("the API server allowed moving a cluster between RavenDB Cloud accounts")
	}
	// No edit gets the user where they want to go, so the message is the only
	// place that can say what does: delete and re-apply.
	if !strings.Contains(err.Error(), "Delete this object and apply it again") {
		t.Errorf("message %q does not say what to do instead", err.Error())
	}
	if !strings.Contains(err.Error(), "spec.accountRef.name") {
		t.Errorf("message %q does not blame spec.accountRef.name", err.Error())
	}
}

func TestCRDAppliesDefaults(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()

	cluster := baseCluster(ns, "defaults")
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating: %v", err)
	}

	// deletionPolicy defaulting to Retain is the safety property that matters
	// most: a stray kubectl delete must not destroy a database.
	if cluster.Spec.DeletionPolicy != v1alpha1.DeletionRetain {
		t.Errorf("deletionPolicy defaulted to %q, want Retain", cluster.Spec.DeletionPolicy)
	}
	// storage.type is deliberately not defaulted here; the correct value
	// depends on the provider. TestCRDDefaultsAgreeWithAccessors covers that.
	if s := cluster.Spec.PollIntervalSeconds; s == nil || *s != 300 {
		t.Errorf("pollIntervalSeconds defaulted to %v, want 300", s)
	}
}

func TestCRDRejectsInvalidEnumValues(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	cluster := baseCluster(ns, "bad-enum")
	cluster.Spec.Provider = "Oracle"

	if err := k8sClient.Create(context.Background(), cluster); err == nil {
		t.Fatal("the API server accepted an unsupported provider")
	}
}

func TestCRDRequiresAtLeastOneAllowedIPWhenSpecified(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	// This goes through unstructured rather than the typed struct on purpose.
	// The typed field is omitempty, so an empty Go slice is dropped before it
	// reaches the server; what matters is what happens to a user who literally
	// writes "allowedIPs: []" in YAML.
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("CloudCluster"))
	cluster.SetNamespace(ns)
	cluster.SetName("empty-ips")
	if err := unstructured.SetNestedMap(cluster.Object, map[string]any{
		"accountRef": map[string]any{"name": "acct"},
		"tier":       "Production",
		"provider":   "AWS",
		"region":     "eu-central-1",
		"storage":    map[string]any{"sizeGB": int64(128)},
		"networking": map[string]any{"allowedIPs": []any{}},
	}, "spec"); err != nil {
		t.Fatalf("building spec: %v", err)
	}

	err := k8sClient.Create(context.Background(), cluster)
	if err == nil {
		t.Fatal("the API server accepted an explicitly empty allowedIPs list")
	}
	if !strings.Contains(err.Error(), "should have at least 1 item") {
		t.Errorf("unexpected rejection: %v", err)
	}
}

func TestCloudAccountCRDInstalls(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	account := &v1alpha1.CloudAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "acct", Namespace: ns},
		Spec: v1alpha1.CloudAccountSpec{
			APIKeySecretRef: v1alpha1.SecretKeySelector{Name: "key"},
		},
	}
	if err := k8sClient.Create(context.Background(), account); err != nil {
		t.Fatalf("creating CloudAccount: %v", err)
	}
	if account.Spec.APIKeySecretRef.Key != "apiKey" {
		t.Errorf("apiKeySecretRef.key defaulted to %q, want apiKey", account.Spec.APIKeySecretRef.Key)
	}
	// spec.endpoint is deliberately not defaulted here; see
	// TestCRDDoesNotDefaultTheEndpoint for why that matters.
}

// TestCRDDoesNotDefaultTheEndpoint guards the mechanism that replaced having a
// second implementation of the cloud client.
//

// TestCRDDefaultsAgreeWithAccessors pins every +kubebuilder:default against the
// Go accessor that reads the same field.
//
// Both exist for a reason. The API server applies the default, so it is what a
// real object carries and what shows up in `kubectl get -o yaml`. The accessor
// covers objects built in memory by a unit test, which never pass through
// admission. The danger is not that both exist, it is that one changes and the
// other does not, and nothing anywhere notices: unit tests only ever see the
// accessor, and a cluster only ever sees the default.
//
// So this test is the only place the two meet. It creates each object through a
// real API server, then compares the stored value against what the accessor
// returns for an empty one.
func TestCRDDefaultsAgreeWithAccessors(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()

	account := &v1alpha1.CloudAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: ns},
		Spec: v1alpha1.CloudAccountSpec{
			APIKeySecretRef: v1alpha1.SecretKeySelector{Name: "key"},
		},
	}
	if err := k8sClient.Create(ctx, account); err != nil {
		t.Fatalf("creating CloudAccount: %v", err)
	}

	cluster := &v1alpha1.CloudCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: ns},
		Spec: v1alpha1.CloudClusterSpec{
			AccountRef: v1alpha1.LocalObjectReference{Name: "defaults"},
			Tier:       v1alpha1.TierDevelopment,
			Storage:    v1alpha1.StorageSpec{SizeGB: 32},
			Networking: v1alpha1.NetworkingSpec{AllowedIPs: []string{"0.0.0.0/0"}},
		},
	}
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating CloudCluster: %v", err)
	}

	// An empty object, so each accessor has to fall back rather than read a
	// value the API server wrote.
	var (
		zeroAccount v1alpha1.CloudAccount
		zeroCluster v1alpha1.CloudCluster
		zeroStorage v1alpha1.StorageSpec
	)

	if got, want := account.Spec.APIKeySecretRef.Key, zeroAccount.EffectiveSecretKey(); got != want {
		t.Errorf("apiKeySecretRef.key: CRD default %q, accessor %q", got, want)
	}
	if account.Spec.RefreshIntervalSeconds == nil {
		t.Error("refreshIntervalSeconds was not defaulted by the API server")
	} else if got, want := time.Duration(*account.Spec.RefreshIntervalSeconds)*time.Second, zeroAccount.EffectiveRefreshInterval(); got != want {
		t.Errorf("refreshIntervalSeconds: CRD default %v, accessor %v", got, want)
	}

	if got, want := cluster.Spec.DeletionPolicy, zeroCluster.EffectiveDeletionPolicy(); got != want {
		t.Errorf("deletionPolicy: CRD default %q, accessor %q", got, want)
	}
	if got, want := cluster.Spec.CreationMode, zeroCluster.EffectiveCreationMode(); got != want {
		t.Errorf("creationMode: CRD default %q, accessor %q", got, want)
	}
	if cluster.Spec.PollIntervalSeconds == nil {
		t.Error("pollIntervalSeconds was not defaulted by the API server")
	} else if got, want := time.Duration(*cluster.Spec.PollIntervalSeconds)*time.Second, zeroCluster.EffectivePollInterval(); got != want {
		t.Errorf("pollIntervalSeconds: CRD default %v, accessor %v", got, want)
	}
	// storage.type is the one field here that deliberately carries no CRD
	// default. The correct value depends on the provider, which the API server
	// cannot consult while defaulting, and the obvious static default,
	// SsdStandard, is what GCP refuses outright. A default appearing here
	// would quietly break every minimal GCP manifest.
	if got := cluster.Spec.Storage.Type; got != "" {
		t.Errorf("storage.type was defaulted to %q; it must stay unset so the "+
			"operator can pick per provider", got)
	}
	if got := zeroStorage.EffectiveStorageType(v1alpha1.ProviderGCP); got != v1alpha1.StorageSsdPremium {
		t.Errorf("unset storage.type on GCP = %q, want SsdPremium", got)
	}
	if got := zeroStorage.EffectiveStorageType(v1alpha1.ProviderAWS); got != v1alpha1.StorageSsdStandard {
		t.Errorf("unset storage.type on AWS = %q, want SsdStandard", got)
	}

	// connectionSecret needs both routes checked, because the two defaults sit
	// at different depths and only one of them is reachable at a time.
	//
	// The +kubebuilder:default=true markers are on the fields INSIDE the block,
	// not on the block itself, so omitting connectionSecret entirely leaves it
	// nil and the API server defaults nothing. That is the common case in a
	// minimal manifest, and only the accessor covers it.
	if cluster.Spec.ConnectionSecret != nil {
		t.Errorf("connectionSecret is defaulted as a block now; this test assumed nil, rethink it")
	}
	if !zeroCluster.ConnectionSecretEnabled() {
		t.Error("an omitted connectionSecret block must still mean enabled")
	}
	if !zeroCluster.ConnectionSecretIncludesCertificate() {
		t.Error("an omitted connectionSecret block must still mean includeCertificate")
	}

	// Present but empty is the case where the field defaults do fire, so this is
	// where they can be compared against the accessors.
	withBlock := &v1alpha1.CloudCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults-secret", Namespace: ns},
		Spec: v1alpha1.CloudClusterSpec{
			AccountRef:       v1alpha1.LocalObjectReference{Name: "defaults"},
			ConnectionSecret: &v1alpha1.ConnectionSecretSpec{},
			Tier:             v1alpha1.TierDevelopment,
			Storage:          v1alpha1.StorageSpec{SizeGB: 32},
			Networking:       v1alpha1.NetworkingSpec{AllowedIPs: []string{"0.0.0.0/0"}},
		},
	}
	if err := k8sClient.Create(ctx, withBlock); err != nil {
		t.Fatalf("creating CloudCluster with an empty connectionSecret: %v", err)
	}
	if withBlock.Spec.ConnectionSecret.Enabled == nil {
		t.Error("connectionSecret.enabled was not defaulted inside a present block")
	} else if got, want := *withBlock.Spec.ConnectionSecret.Enabled, zeroCluster.ConnectionSecretEnabled(); got != want {
		t.Errorf("connectionSecret.enabled: CRD default %v, accessor %v", got, want)
	}
	if withBlock.Spec.ConnectionSecret.IncludeCertificate == nil {
		t.Error("connectionSecret.includeCertificate was not defaulted inside a present block")
	} else if got, want := *withBlock.Spec.ConnectionSecret.IncludeCertificate, zeroCluster.ConnectionSecretIncludesCertificate(); got != want {
		t.Errorf("connectionSecret.includeCertificate: CRD default %v, accessor %v", got, want)
	}
}

// TestCRDRefusesANameLongerThanALabelValue: the connection Secret carries the
// object name in a label, and a longer one would leave the Secret unwritable.
func TestCRDRefusesANameLongerThanALabelValue(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	if err := k8sClient.Create(context.Background(), baseCluster(ns, strings.Repeat("a", 63))); err != nil {
		t.Errorf("a 63 character name was refused: %v", err)
	}
	err := k8sClient.Create(context.Background(), baseCluster(ns, strings.Repeat("b", 64)))
	if err == nil || !strings.Contains(err.Error(), "at most 63 characters") {
		t.Errorf("a 64 character name gave %v; want it refused, saying why", err)
	}
}

// TestCRDAcceptsTheChangesThatOnlyConfirm: writing down the display name an
// object already finds its cluster by, which is what applying the export
// tool's manifest over a hand-written one does, and confirming a takeover by
// moving from TakeOverOrCreate to TakeOverOnly.
func TestCRDAcceptsTheChangesThatOnlyConfirm(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)
	cluster := baseCluster(ns, "legacy-prod")
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating: %v", err)
	}
	holdCluster(t, cluster)

	cluster.Spec.DisplayName = "Legacy-Prod"
	cluster.Spec.CreationMode = v1alpha1.CreationTakeOverOnly
	if err := k8sClient.Update(ctx, cluster); err != nil {
		t.Errorf("writing down the name and confirming the takeover was refused: %v", err)
	}

	cluster.Spec.CreationMode = v1alpha1.CreationTakeOverOrCreate
	if err := k8sClient.Update(ctx, cluster); err == nil || !strings.Contains(err.Error(), "creationMode cannot change") {
		t.Errorf("going back to TakeOverOrCreate gave %v; want it refused", err)
	}
}

// TestCRDAllowsConfirmingACreateOnlyCluster: an object restored from git
// without its status finds the cluster it once created, and CreateOnly then
// refuses it. Moving to TakeOverOnly is the way to say it is this object's.
func TestCRDAllowsConfirmingACreateOnlyCluster(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	cluster := baseCluster(newNamespace(t), "restored")
	cluster.Spec.CreationMode = v1alpha1.CreationCreateOnly
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating: %v", err)
	}
	holdCluster(t, cluster)

	cluster.Spec.CreationMode = v1alpha1.CreationTakeOverOnly
	if err := k8sClient.Update(ctx, cluster); err != nil {
		t.Errorf("moving CreateOnly to TakeOverOnly was refused: %v", err)
	}
}

// TestCRDAllowsFixingIdentityBeforeTheClusterExists: until the object has
// found or created its cluster nothing exists in RavenDB Cloud, so a typo is
// fixed by editing. Once a create has gone out, that is over, because the
// cluster may exist under what was sent.
func TestCRDAllowsFixingIdentityBeforeTheClusterExists(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	cluster := baseCluster(newNamespace(t), "typo")
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating: %v", err)
	}

	cluster.Spec.AccountRef.Name = "the-right-account"
	cluster.Spec.Tier = v1alpha1.TierDevelopment
	cluster.Spec.Provider = v1alpha1.ProviderAzure
	cluster.Spec.Region = "westeurope"
	cluster.Spec.Subdomain = "typo-fixed"
	cluster.Spec.DisplayName = "Typo Fixed"
	cluster.Spec.CreationMode = v1alpha1.CreationCreateOnly
	cluster.Spec.Storage = v1alpha1.StorageSpec{}
	if err := k8sClient.Update(ctx, cluster); err != nil {
		t.Fatalf("editing the identity of an object with no cluster yet was refused: %v", err)
	}

	attempted := metav1.Now()
	cluster.Status.ProvisionAttemptedAt = &attempted
	if err := k8sClient.Status().Update(ctx, cluster); err != nil {
		t.Fatalf("recording the create in status: %v", err)
	}
	cluster.Spec.Tier = v1alpha1.TierProduction
	if err := k8sClient.Update(ctx, cluster); err == nil || !strings.Contains(err.Error(), "tier cannot change") {
		t.Errorf("changing the tier after a create went out gave %v; want it refused", err)
	}
}

// TestCRDRefusesCreateOnlyUnderObserve: Observe never creates, so the pair
// could only ever report NameTaken or ClusterNotFound.
func TestCRDRefusesCreateOnlyUnderObserve(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	cluster := baseCluster(ns, "create-only")
	cluster.Spec.CreationMode = v1alpha1.CreationCreateOnly
	cluster.Spec.ManagementPolicy = v1alpha1.ManagementObserve

	err := k8sClient.Create(context.Background(), cluster)
	if err == nil {
		t.Fatal("the API server admitted creationMode CreateOnly with managementPolicy Observe")
	}
	if !strings.Contains(err.Error(), "spec.creationMode") || !strings.Contains(err.Error(), "Observe never creates") {
		t.Errorf("message %q does not blame spec.creationMode or say why", err.Error())
	}
}

// TestCRDEnforcesTheNodeCeiling pins the node rules. RavenDB Cloud gives a
// Production cluster the three standard nodes A, B and C, never removes them,
// and allows at most three additional ones; a Free or Development cluster has
// one node and no additional ones. Anything else would have the operator send
// an AddNode or RemoveNode the API refuses.
func TestCRDEnforcesTheNodeCeiling(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		tier     v1alpha1.Tier
		nodes    int32
		accepted bool
		message  string
	}{
		{"three on production is the base size", v1alpha1.TierProduction, 3, true, ""},
		{"six on production is the ceiling", v1alpha1.TierProduction, 6, true, ""},
		{"seven exceeds the ceiling", v1alpha1.TierProduction, 7, false, "should be less than or equal to 6"},
		{"two on production would drop a standard node", v1alpha1.TierProduction, 2, false, "standard nodes"},
		{"one on development is fine", v1alpha1.TierDevelopment, 1, true, ""},
		{"two on development needs additional nodes", v1alpha1.TierDevelopment, 2, false, "exactly one node"},
		{"four on free needs additional nodes too", v1alpha1.TierFree, 4, false, "exactly one node"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			cluster := baseCluster(ns, "nodes")
			cluster.Spec.Tier = tc.tier
			cluster.Spec.Nodes = &tc.nodes

			err := k8sClient.Create(ctx, cluster)
			if tc.accepted {
				if err != nil {
					t.Fatalf("nodes=%d on %s should be accepted: %v", tc.nodes, tc.tier, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("nodes=%d on %s should be refused", tc.nodes, tc.tier)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("message does not explain why: %v", err)
			}
		})
	}
}

// TestCRDRequiresCIDRAllowedIPs covers the most natural way to get this wrong.
//
// RavenDB Cloud's CIDR check is literally a test for "/", so a bare address is
// refused with a 400 halfway through a create. Writing 203.0.113.7 rather than
// 203.0.113.7/32 is an easy mistake and worth catching at admission.
//
// The accepted and refused values below are the ones RavenDB Cloud's own CIDR
// validation is tested with, so this also records where our rule is
// deliberately stricter: RavenDB Cloud's parser accepts the three-octet
// "00.0.0/0", which the API server's cidr format refuses. Rejecting that is
// fine.
func TestCRDRequiresCIDRAllowedIPs(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		ips      []string
		accepted bool
	}{
		{"open to the world", []string{"0.0.0.0/0"}, true},
		{"single host with a prefix", []string{"203.0.113.7/32"}, true},
		{"a range", []string{"198.51.100.0/24"}, true},
		{"several entries", []string{"0.0.0.0/0", "203.0.113.7/32"}, true},
		{"bare address, no prefix", []string{"203.0.113.7"}, false},
		{"prefix out of range", []string{"198.51.100.0/44"}, false},
		{"octet out of range", []string{"100.200.300.400/0"}, false},
		{"not an address at all", []string{"////88"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			cluster := baseCluster(ns, "ips")
			cluster.Spec.Networking.AllowedIPs = tc.ips

			err := k8sClient.Create(ctx, cluster)
			if tc.accepted && err != nil {
				t.Fatalf("%v should be accepted: %v", tc.ips, err)
			}
			if !tc.accepted && err == nil {
				t.Fatalf("%v should be refused", tc.ips)
			}
		})
	}
}

// TestCRDRefusesServerless records a deliberate narrowing.
//
// deploymentType was a field with one usable value. Serverless was removed
// first, once it turned out RavenDB Cloud puts four preconditions on a
// serverless product and this operator applied none of them, and then the
// field itself went: a setting nobody can meaningfully set is noise on the
// surface users read.
//
// The API server now rejects the key rather than the value, which is a
// stronger guarantee. If serverless is ever supported, the field comes back
// with the four rules in place: tier is not Free, instanceType is Dev30 or
// PB10 by tier, provider is not GCP, and the account's default serverless
// release channel is used.
func TestCRDRefusesServerless(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	// Unstructured, because the Go constant is gone: the point is that the API
	// server refuses the string, not that our types cannot express it.
	cluster := map[string]any{
		"apiVersion": "cloud.ravendb.net/v1alpha1",
		"kind":       "CloudCluster",
		"metadata":   map[string]any{"name": "serverless", "namespace": ns},
		"spec": map[string]any{
			"accountRef":     map[string]any{"name": "acct"},
			"tier":           "Production",
			"deploymentType": "Serverless",
			"storage":        map[string]any{"sizeGB": int64(128)},
			"networking":     map[string]any{"allowedIPs": []any{"0.0.0.0/0"}},
		},
	}
	obj := &unstructured.Unstructured{Object: cluster}

	// Strict field validation, because that is what kubectl apply sends. The
	// default is lenient and prunes an unknown key silently, which would mean
	// a user asking for Serverless got Standard and no warning. Verified
	// against a live cluster, where kubectl reports a strict decoding error
	// naming the unknown field.
	err := k8sClient.Create(context.Background(), obj, client.FieldValidation("Strict"))
	if err == nil {
		t.Fatal("the API server accepted deploymentType, which no longer exists")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("expected the field itself to be unknown, got: %v", err)
	}
}

// TestCRDRequiresAUsableSecretName covers a field that becomes a Kubernetes
// object name. Without the pattern the operator accepts the manifest and then
// fails to write the Secret on every reconcile, reporting the API server's
// complaint about a name rather than the field that produced it.
func TestCRDRequiresAUsableSecretName(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		secret   string
		accepted bool
	}{
		{"a plain name", "orders-connection", true},
		{"dotted, which a Secret allows", "orders.connection", true},
		{"capitals are not DNS-1123", "Orders-Connection", false},
		{"underscores are not either", "orders_connection", false},
		{"cannot start with a dash", "-orders", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			cluster := baseCluster(ns, "secretname")
			cluster.Spec.ConnectionSecret = &v1alpha1.ConnectionSecretSpec{Name: tc.secret}

			err := k8sClient.Create(ctx, cluster)
			if tc.accepted && err != nil {
				t.Fatalf("%q should be accepted: %v", tc.secret, err)
			}
			if !tc.accepted && err == nil {
				t.Fatalf("%q should be refused; it cannot name a Secret", tc.secret)
			}
		})
	}
}

// TestCRDRefusesStandardDiskOnGCP: Google Cloud products offer SsdPremium only,
// which RavenDB Cloud checks on create, so SsdStandard on GCP is refused at
// apply instead of after a round trip.
func TestCRDRefusesStandardDiskOnGCP(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		provider v1alpha1.Provider
		disk     v1alpha1.StorageType
		accepted bool
	}{
		{"GCP with the class it does not sell", v1alpha1.ProviderGCP, v1alpha1.StorageSsdStandard, false},
		{"GCP with premium", v1alpha1.ProviderGCP, v1alpha1.StorageSsdPremium, true},
		{"GCP with nothing named, so the operator picks", v1alpha1.ProviderGCP, "", true},
		{"AWS with standard", v1alpha1.ProviderAWS, v1alpha1.StorageSsdStandard, true},
		{"Azure with standard", v1alpha1.ProviderAzure, v1alpha1.StorageSsdStandard, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			cluster := baseCluster(ns, "disks")
			cluster.Spec.Provider = tc.provider
			cluster.Spec.Storage.Type = tc.disk

			err := k8sClient.Create(ctx, cluster)
			if tc.accepted {
				if err != nil {
					t.Fatalf("%s + %q should be accepted: %v", tc.provider, tc.disk, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s + %q should be refused", tc.provider, tc.disk)
			}
			if !strings.Contains(err.Error(), "SsdPremium") {
				t.Errorf("the message should name the value to use instead: %v", err)
			}
			if !strings.Contains(err.Error(), "spec.storage.type") {
				t.Errorf("the message should name the field to change: %v", err)
			}
		})
	}
}

// TestCRDAcceptsTheSmallestUsefulManifest is the first-run experience, pinned.
//
// tier plus an allow list, and nothing else. Everything absent is either
// defaulted by the API server or resolved by the operator from what RavenDB
// Cloud offers. Requiring storage would mean naming a disk size before knowing
// which instance type you get, and the valid sizes depend on the instance
// type.
func TestCRDAcceptsTheSmallestUsefulManifest(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	cluster := &v1alpha1.CloudCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "minimal", Namespace: ns},
		Spec: v1alpha1.CloudClusterSpec{
			AccountRef: v1alpha1.LocalObjectReference{Name: "acct"},
			Tier:       v1alpha1.TierDevelopment,
			Networking: v1alpha1.NetworkingSpec{AllowedIPs: []string{"0.0.0.0/0"}},
		},
	}
	if err := k8sClient.Create(context.Background(), cluster); err != nil {
		t.Fatalf("the minimal manifest should be accepted: %v", err)
	}
	if got := cluster.Spec.Storage.SizeGB; got != 0 {
		t.Errorf("storage.sizeGB defaulted to %d; it must stay unset so the "+
			"operator can pick the smallest the instance type sells", got)
	}
}

// TestCRDConstrainsTheEndpoint keeps two things out of spec.endpoint: a URL the
// client would reject anyway, and a URL carrying credentials.
//
// The scheme check earns its place by moving the failure. Without it a
// scheme-less endpoint is stored happily and surfaces on the first reconcile as
// a condition, which is a slower and vaguer way to learn about a typo.
//

// TestCRDRefusesDuplicateAllowedIPsOnBothLists pins the +listType=set markers.
//
// They are what makes server-side apply merge these lists entry by entry rather
// than treating each as one opaque value, and the API server enforces
// uniqueness as a side effect, which is the part a test can see. Both lists get
// the marker: the cluster's own, and the account default every cluster that
// omits one falls back to.
func TestCRDRefusesDuplicateAllowedIPsOnBothLists(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	dupes := []string{"10.0.0.0/8", "10.0.0.0/8"}

	t.Run("CloudCluster spec.networking.allowedIPs", func(t *testing.T) {
		cluster := baseCluster(newNamespace(t), "dupes")
		cluster.Spec.Networking.AllowedIPs = dupes
		if err := k8sClient.Create(ctx, cluster); err == nil {
			t.Fatal("duplicate CIDRs should be refused on a set")
		}
	})

	t.Run("CloudAccount spec.defaults.allowedIPs", func(t *testing.T) {
		account := &v1alpha1.CloudAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "dupes", Namespace: newNamespace(t)},
			Spec: v1alpha1.CloudAccountSpec{
				APIKeySecretRef: v1alpha1.SecretKeySelector{Name: "api-key"},
				Defaults:        &v1alpha1.AccountDefaults{AllowedIPs: dupes},
			},
		}
		if err := k8sClient.Create(ctx, account); err == nil {
			t.Fatal("duplicate CIDRs should be refused on a set")
		}
	})
}

// A CloudClusterRestart is a record of something that happened. There is no
// edit that means anything: repointing a finished one at another node asks for
// a second restart while claiming to be the record of the first, and editing a
// running one would aim a request that has already gone out.
func TestCRDRejectsEditingARestart(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.CloudClusterRestart)
	}{
		{"nodeTag", func(r *v1alpha1.CloudClusterRestart) { r.Spec.NodeTag = "B" }},
		{"clusterRef", func(r *v1alpha1.CloudClusterRestart) { r.Spec.ClusterRef.Name = "elsewhere" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			restart := &v1alpha1.CloudClusterRestart{
				ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: ns},
				Spec: v1alpha1.CloudClusterRestartSpec{
					ClusterRef: v1alpha1.LocalObjectReference{Name: "orders"},
					NodeTag:    "A",
				},
			}
			if err := k8sClient.Create(ctx, restart); err != nil {
				t.Fatalf("creating: %v", err)
			}

			tc.mutate(restart)
			err := k8sClient.Update(ctx, restart)
			if err == nil {
				t.Fatal("the API server allowed editing a restart that has already been requested")
			}
			if !strings.Contains(err.Error(), "immutable") {
				t.Errorf("message %q does not say the object is immutable", err.Error())
			}
		})
	}
}

// The tag shape is decidable from the object alone, so it belongs at admission.
// Whether the node exists is not, and stays in the controller.
func TestCRDChecksTheRestartNodeTagShape(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	for _, tc := range []struct {
		tag     string
		allowed bool
	}{
		{"A", true},
		{"C", true},
		{"a", false},
		{"AB", false},
		{"1", false},
		{"", false},
	} {
		t.Run("tag "+tc.tag, func(t *testing.T) {
			ns := newNamespace(t)
			err := k8sClient.Create(ctx, &v1alpha1.CloudClusterRestart{
				ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: ns},
				Spec: v1alpha1.CloudClusterRestartSpec{
					ClusterRef: v1alpha1.LocalObjectReference{Name: "orders"},
					NodeTag:    tc.tag,
				},
			})
			if tc.allowed && err != nil {
				t.Fatalf("node tag %q was refused: %v", tc.tag, err)
			}
			if !tc.allowed && err == nil {
				t.Fatalf("node tag %q was accepted; RavenDB Cloud tags nodes with one upper case letter", tc.tag)
			}
		})
	}
}
