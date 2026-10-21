package controller

import (
	"fmt"
	"net"
	"strings"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
)

// Limits RavenDB Cloud enforces but never publishes, copied as constants and
// verified against RavenDB Cloud's source. If one drifts, RavenDB Cloud refuses
// the create in its own words: a worse experience, not a wrong one.

var (
	// The largest root disk RavenDB Cloud creates on each provider. The
	// operator only creates RootOnly products, the layout this limit applies
	// to.
	maxRootDiskGB = map[v1alpha1.Provider]int32{
		v1alpha1.ProviderAWS:   1000,
		v1alpha1.ProviderAzure: 1024,
		v1alpha1.ProviderGCP:   1000,
	}

	// Instance types RavenDB Cloud sells without additional nodes, so their
	// node count cannot change. Verified against RavenDB Cloud's product
	// validation, which refuses adding a node to all of these and removing one
	// from the PN types.
	withoutAdditionalNodes = []string{"PN10", "PN20", "PN30", "PN40", "PN50", "PN60", "PN70", "PS10", "PS20"}

	// The most allow-list entries RavenDB Cloud takes on each provider. AWS is
	// lower because each address costs two security group rules against a
	// quota of 60.
	maxAllowedIPs = map[v1alpha1.Provider]int{
		v1alpha1.ProviderAWS:   24,
		v1alpha1.ProviderAzure: 200,
		v1alpha1.ProviderGCP:   200,
	}
)

func validateRootDiskSize(provider v1alpha1.Provider, sizeGB int32) error {
	max, known := maxRootDiskGB[provider]
	if !known || sizeGB <= max {
		return nil
	}
	return fmt.Errorf("spec.storage.sizeGB %d exceeds the %d GB maximum RavenDB Cloud allows on a %s root disk",
		sizeGB, max, provider)
}

func validateAllowedIPCount(provider v1alpha1.Provider, allowedIPs []string) error {
	max, known := maxAllowedIPs[provider]
	if !known || len(allowedIPs) <= max {
		return nil
	}
	return fmt.Errorf("spec.networking.allowedIPs has %d entries, but %s allows at most %d",
		len(allowedIPs), provider, max)
}

// canonicalCIDR rewrites an allow-list entry the way RavenDB Cloud stores it:
// host bits masked off, so 192.168.1.10/24 becomes 192.168.1.0/24. Comparing
// without it reports drift no edit can resolve.
//
// An unparseable entry is returned unchanged; admission already rejects those.
func canonicalCIDR(entry string) string {
	_, network, err := net.ParseCIDR(strings.TrimSpace(entry))
	if err != nil {
		return entry
	}
	return network.String()
}

func canonicalCIDRs(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, canonicalCIDR(e))
	}
	return out
}

// validateAllowedIPs rejects a list RavenDB Cloud would refuse. Duplicates are
// compared after canonicalisation, as the cloud does, so 1.2.3.4/0 and
// 0.0.0.0/0 collide.
//
// Azure also refuses overlapping entries. That is not checked here: it needs
// range arithmetic, and getting it subtly wrong would reject lists the cloud
// accepts.
func validateAllowedIPs(provider v1alpha1.Provider, allowedIPs []string) error {
	if err := validateAllowedIPCount(provider, allowedIPs); err != nil {
		return err
	}
	seen := make(map[string]string, len(allowedIPs))
	for _, entry := range allowedIPs {
		key := canonicalCIDR(entry)
		first, dup := seen[key]
		switch {
		case !dup:
			seen[key] = entry
		case first == entry:
			return fmt.Errorf("spec.networking.allowedIPs lists %q twice", entry)
		default:
			return fmt.Errorf(
				"spec.networking.allowedIPs lists %q and %q, which are the same range once RavenDB Cloud normalises them (both become %q)",
				first, entry, key)
		}
	}
	return nil
}
