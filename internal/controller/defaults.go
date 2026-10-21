package controller

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// Defaults that need the account or the published lists, and so cannot be a
// +kubebuilder:default.

// The most characters RavenDB Cloud accepts in a cluster subdomain.
const maxSubdomainLength = 13

// resolveSubdomain returns the DNS label to ask for. A derived one that does not
// fit is refused rather than truncated: it ends up in production URLs, so
// inventing a name is worse than asking for one. spec.subdomain cannot be added
// once omitted, so every refusal says to delete the object and apply it again.
func resolveSubdomain(explicit, displayName string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	derived := sanitizeSubdomain(displayName)
	switch {
	case derived == "":
		return "", fmt.Errorf(
			"cannot derive a subdomain from displayName %q: it has no letters, digits or dashes to use. spec.subdomain cannot be added to an existing object, so delete this one and apply it again with a subdomain of up to %d of them; nothing was created",
			displayName, maxSubdomainLength)
	case len(derived) > maxSubdomainLength:
		return "", fmt.Errorf(
			"the subdomain derived from displayName %q is %q, which is %d characters, and RavenDB Cloud accepts at most %d. spec.subdomain cannot be added to an existing object, so delete this one and apply it again with a subdomain of up to %d characters; nothing was created",
			displayName, derived, len(derived), maxSubdomainLength, maxSubdomainLength)
	}
	return derived, nil
}

func sanitizeSubdomain(in string) string {
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}

// smallestForTier is the instance type to ask for when the manifest names none:
// fewest vCPUs, then least RAM, then name. The cheapest shape the tier offers,
// so nobody is billed for a default they did not pick.
func smallestForTier(available []cloudapi.InstanceType, tier v1alpha1.Tier) (string, error) {
	var candidates []cloudapi.InstanceType
	for _, it := range available {
		if it.Tier == cloudapi.Tier(tier) {
			candidates = append(candidates, it)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no instance types available for tier %s in this region; set spec.instanceType explicitly", tier)
	}
	slices.SortFunc(candidates, func(a, b cloudapi.InstanceType) int {
		return cmp.Or(
			cmp.Compare(a.VirtualCPUs, b.VirtualCPUs),
			cmp.Compare(a.RAMGiB, b.RAMGiB),
			strings.Compare(a.Name, b.Name),
		)
	})
	return candidates[0].Name, nil
}
