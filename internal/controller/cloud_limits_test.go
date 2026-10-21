package controller

import (
	"strings"
	"testing"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// The two limits RavenDB Cloud does not publish, so they are copied constants
// and worth pinning against the citations in cloud_limits.go.
func TestValidateProviderCaps(t *testing.T) {
	t.Run("root disk", func(t *testing.T) {
		cases := []struct {
			provider v1alpha1.Provider
			sizeGB   int32
			refused  bool
		}{
			{v1alpha1.ProviderAWS, 1000, false},
			{v1alpha1.ProviderAWS, 1024, true},
			{v1alpha1.ProviderAzure, 1024, false},
			{v1alpha1.ProviderAzure, 2048, true},
			{v1alpha1.ProviderGCP, 1000, false},
			{v1alpha1.ProviderGCP, 2048, true},
			// An unset provider cannot be checked, so it falls through.
			{v1alpha1.Provider(""), 999999, false},
		}
		for _, tc := range cases {
			err := validateRootDiskSize(tc.provider, tc.sizeGB)
			if tc.refused && err == nil {
				t.Errorf("%s %d GiB should be refused", tc.provider, tc.sizeGB)
			}
			if !tc.refused && err != nil {
				t.Errorf("%s %d GiB should be allowed: %v", tc.provider, tc.sizeGB, err)
			}
		}
	})

	t.Run("allowed IPs", func(t *testing.T) {
		ips := func(n int) []string {
			out := make([]string, n)
			for i := range out {
				out[i] = "0.0.0.0/0"
			}
			return out
		}
		// AWS is the one that bites: each address costs two security group
		// rules against a quota of 60.
		if err := validateAllowedIPCount(v1alpha1.ProviderAWS, ips(24)); err != nil {
			t.Errorf("24 entries is the AWS maximum: %v", err)
		}
		if err := validateAllowedIPCount(v1alpha1.ProviderAWS, ips(25)); err == nil {
			t.Error("25 entries should be refused on AWS")
		}
		if err := validateAllowedIPCount(v1alpha1.ProviderAzure, ips(25)); err != nil {
			t.Errorf("25 entries is fine on Azure: %v", err)
		}
		if err := validateAllowedIPCount(v1alpha1.ProviderGCP, ips(200)); err != nil {
			t.Errorf("200 entries is the GCP maximum: %v", err)
		}
	})
}

// TestCanonicalCIDRMatchesProduction uses the values RavenDB Cloud's own CIDR
// normalisation is tested with, so a change on either side shows up here.
func TestCanonicalCIDRMatchesProduction(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0/0":       "0.0.0.0/0",
		"1.2.3.4/0":       "0.0.0.0/0",
		"1.2.3.4/32":      "1.2.3.4/32",
		"15.2.2.2/32":     "15.2.2.2/32",
		"192.168.1.10/24": "192.168.1.0/24",
		// Admission rejects this, so it must pass through rather than be invented into something.
		"not a cidr": "not a cidr",
	}
	for in, want := range cases {
		if got := canonicalCIDR(in); got != want {
			t.Errorf("canonicalCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}

// A byte-literal drift comparison makes a manifest written with host bits set
// report Synced=False on every poll, and no edit can fix it.
func TestAllowedIPDriftIgnoresCanonicalisation(t *testing.T) {
	spec := &v1alpha1.CloudClusterSpec{
		Networking: v1alpha1.NetworkingSpec{AllowedIPs: []string{"192.168.1.10/24", "0.0.0.0/0"}},
		Storage:    v1alpha1.StorageSpec{SizeGB: 20},
	}
	observed := &cloudapi.Cluster{
		// What the cloud stores and reports back after masking.
		AllowedIPs: []string{"0.0.0.0/0", "192.168.1.0/24"},
		Hardware:   cloudapi.Hardware{Storage: cloudapi.Storage{SizeGiB: 20, Type: cloudapi.StorageSsdStandard}},
	}
	for _, d := range shapeDifferences(spec, observed, nil) {
		if strings.Contains(d.report, "allowedIPs") {
			t.Errorf("reported drift a user cannot resolve: %s", d.report)
		}
	}
}

func TestDuplicateAllowedIPsAreRefused(t *testing.T) {
	// Same range, written two ways, which RavenDB Cloud counts as a duplicate.
	err := validateAllowedIPs(v1alpha1.ProviderAWS, []string{"1.2.3.4/0", "0.0.0.0/0"})
	if err == nil {
		t.Fatal("two entries that normalise to the same range should be refused")
	}
	if !strings.Contains(err.Error(), "same range") {
		t.Errorf("message %q should explain that the two collide after normalisation", err)
	}
	if err := validateAllowedIPs(v1alpha1.ProviderAWS, []string{"10.0.0.0/8", "0.0.0.0/0"}); err != nil {
		t.Errorf("two genuinely different ranges should be accepted: %v", err)
	}
}

func TestANodeCountOnAnInstanceTypeWithoutAdditionalNodesIsOnlyReported(t *testing.T) {
	four := int32(4)
	spec := &v1alpha1.CloudClusterSpec{Nodes: &four}
	observed := &cloudapi.Cluster{ID: "1-A", InstanceType: "PN20", NodeTags: []string{"A", "B", "C"}}

	differences := nodeDifference(spec, observed, nil)
	if len(differences) != 1 || differences[0].apply != nil {
		t.Fatalf("differences %+v; want one, reported and never sent", differences)
	}
	if !strings.Contains(differences[0].report, "PN20 without additional nodes") {
		t.Errorf("report %q does not say why", differences[0].report)
	}
}
