package main

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
	"github.com/ravendb/ravendb-cloud-operator/internal/version"
)

// noCluster is a kubeconfig that leads nowhere, for the paths that must not
// need one.
func noCluster() (client.Client, string, error) {
	return nil, "", errors.New("no kubeconfig")
}

// Every way to call it wrong has to say what to do instead, before a single
// request goes out.
func TestAMisusedCommandSaysWhy(t *testing.T) {
	withKey := func(name string) string {
		if name == apiKeyEnvVar {
			return "a-key"
		}
		return ""
	}
	noKey := func(string) string { return "" }

	for _, tc := range []struct {
		name   string
		args   []string
		getenv func(string) string
		says   string
	}{
		{"no subcommand", []string{"--account", "a", "orders"}, withKey, "kubectl ravendb-cloud export"},
		{"no account", []string{"export", "--namespace", "apps", "orders"}, withKey, "--account is required"},
		{"nothing to export", []string{"export", "--account", "a"}, withKey, "or --all"},
		{"both", []string{"export", "--account", "a", "--all", "orders"}, withKey, "not both"},
		{"no key and no cluster", []string{"export", "--account", "a", "orders"}, noKey, apiKeyEnvVar},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), tc.args, tc.getenv, &out, noCluster)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q on a misuse", out.String())
			}
		})
	}
}

func TestAKeyInTheEnvironmentNeedsNoCluster(t *testing.T) {
	server := simulatorWith(t, "a-key", "orders")

	var out bytes.Buffer
	getenv := func(name string) string {
		switch name {
		case apiKeyEnvVar:
			return "a-key"
		case cloudapi.EndpointEnvVar:
			return server.URL
		}
		return ""
	}
	args := []string{"export", "--account", "ravendb-cloud", "-n", "apps", "orders"}
	if err := run(context.Background(), args, getenv, &out, noCluster); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"kind: CloudCluster", "name: orders", "namespace: apps", "managementPolicy: Full"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestTheKeyComesFromTheAccountsSecret is what the plugin is for: nobody
// copies the key onto a laptop, and the manifests land next to their account,
// in the kubeconfig's namespace when -n is not given.
func TestTheKeyComesFromTheAccountsSecret(t *testing.T) {
	server := simulatorWith(t, "a-key", "orders")
	kube := clusterWith(t,
		&v1alpha1.CloudAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "ravendb-cloud", Namespace: "apps"},
			Spec: v1alpha1.CloudAccountSpec{
				APIKeySecretRef: v1alpha1.SecretKeySelector{Name: "ravendb-cloud-key"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ravendb-cloud-key", Namespace: "apps"},
			Data:       map[string][]byte{"apiKey": []byte("a-key")},
		},
	)
	inApps := func() (client.Client, string, error) { return kube, "apps", nil }

	// The simulator stands in for RavenDB Cloud the way it does for the
	// operator: through the endpoint variable, never through the account.
	toSimulator := func(name string) string {
		if name == cloudapi.EndpointEnvVar {
			return server.URL
		}
		return ""
	}

	var out bytes.Buffer
	args := []string{"export", "--account", "ravendb-cloud", "orders"}
	if err := run(context.Background(), args, toSimulator, &out, inApps); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"name: orders", "namespace: apps", "name: ravendb-cloud"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestAnAccountInAnotherNamespaceSaysWhereToLook(t *testing.T) {
	kube := clusterWith(t)
	inDefault := func() (client.Client, string, error) { return kube, "default", nil }

	var out bytes.Buffer
	args := []string{"export", "--account", "ravendb-cloud", "orders"}
	err := run(context.Background(), args, func(string) string { return "" }, &out, inDefault)
	if err == nil || !strings.Contains(err.Error(), `namespace "default"`) || !strings.Contains(err.Error(), "-n") {
		t.Errorf("err = %v, want it to name the namespace it looked in and -n", err)
	}
}

func simulatorWith(t *testing.T, apiKey, displayName string) *httptest.Server {
	t.Helper()
	sim := cloudsim.New(cloudsim.Options{APIKey: apiKey, Domain: "example.ravendb.cloud"})
	server := httptest.NewServer(sim.Handler())
	t.Cleanup(server.Close)
	sim.State().Seed(cloudsim.SeedSpec{DisplayName: displayName})
	return server
}

func clusterWith(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return k8sfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// The version a bug report quotes has to be the one that was built.
func TestVersionPrintsTheStampedVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, func(string) string { return "" }, &out, noCluster); err != nil {
		t.Fatalf("version: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != version.Version {
		t.Errorf("printed %q, want %q", got, version.Version)
	}
}
