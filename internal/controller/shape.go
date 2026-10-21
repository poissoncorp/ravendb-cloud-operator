package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// Rules checked against the lists RavenDB Cloud publishes. An empty list means
// this deployment reported nothing, not that nothing is allowed, so the request
// passes and RavenDB Cloud judges it.

// validateRegion checks a region against the list the account mirrored. field
// names where the value came from, so the message says what to change.
func validateRegion(account *v1alpha1.CloudAccount, provider v1alpha1.Provider, region, field string) error {
	regions := regionsFor(account, provider)
	if len(regions) == 0 || slices.Contains(regions, region) {
		return nil
	}
	return fmt.Errorf("%s is %q, which is not available for %s; available regions: %s",
		field, region, provider, strings.Join(sortedCopy(regions), ", "))
}

// regionsFor is the region list the account mirrored for a provider, or nil
// when it has none yet.
func regionsFor(account *v1alpha1.CloudAccount, provider v1alpha1.Provider) []string {
	for _, entry := range account.Status.Providers {
		if entry.Provider == provider {
			return entry.Regions
		}
	}
	return nil
}

// validateReleaseChannel checks the requested channel against the list the
// account mirrored into status. The names are not what anyone guesses: RavenDB
// Cloud parses the field into a fixed list of versioned channels, so "Stable"
// is not a channel and "Stable72" is.
func validateReleaseChannel(account *v1alpha1.CloudAccount, channel, field string) error {
	if len(account.Status.ReleaseChannels) == 0 {
		return nil
	}
	available := make([]string, 0, len(account.Status.ReleaseChannels))
	for _, c := range account.Status.ReleaseChannels {
		if c.Name == channel {
			return nil
		}
		available = append(available, c.Name)
	}
	slices.Sort(available)
	return fmt.Errorf("%s is %q, which is not offered to this account; set it to one of: %s",
		field, channel, strings.Join(available, ", "))
}

func validateInstanceType(available []cloudapi.InstanceType, name string, tier v1alpha1.Tier) error {
	var forTier []string
	var found *cloudapi.InstanceType
	for i, it := range available {
		if it.Name == name {
			found = &available[i]
		}
		if it.Tier == cloudapi.Tier(tier) {
			forTier = append(forTier, it.Name)
		}
	}
	if found != nil && found.Tier == cloudapi.Tier(tier) {
		return nil
	}
	slices.Sort(forTier)
	problem := "is not offered in this region"
	if found != nil {
		problem = fmt.Sprintf("belongs to tier %s, not %s", found.Tier, tier)
	}
	if len(forTier) == 0 {
		return fmt.Errorf("spec.instanceType %q %s", name, problem)
	}
	return fmt.Errorf("spec.instanceType %q %s; set it to one the %s tier offers here: %s",
		name, problem, tier, strings.Join(forTier, ", "))
}

// smallestDiskFor is the disk size to ask for when the manifest names none: the
// smallest, and cheapest, the shape sells. Zero means the catalogue reported no
// menu, and RavenDB Cloud decides.
func smallestDiskFor(available []cloudapi.InstanceType, name string) int32 {
	instanceType := findInstanceType(available, name)
	if instanceType == nil || len(instanceType.DiskSizesGiB) == 0 {
		return 0
	}
	return slices.Min(instanceType.DiskSizesGiB)
}

// validateStorage checks a requested disk against what the chosen instance type
// offers. RavenDB Cloud sells whole disks off a fixed menu per shape, and an
// off-menu size is a 400 that reads like a server problem rather than a typo.
func validateStorage(available []cloudapi.InstanceType, name string, storage v1alpha1.StorageSpec, provider v1alpha1.Provider) error {
	instanceType := findInstanceType(available, name)
	if instanceType == nil {
		return nil
	}

	if storage.SizeGB > 0 && len(instanceType.DiskSizesGiB) > 0 &&
		!slices.Contains(instanceType.DiskSizesGiB, storage.SizeGB) {
		return fmt.Errorf("spec.storage.sizeGB %d is not offered for %s; set it to one of: %s",
			storage.SizeGB, name, joinInt32s(instanceType.DiskSizesGiB))
	}

	if len(instanceType.StorageOffers) == 0 {
		return nil
	}
	storageType := storage.EffectiveStorageType(provider)
	offer := findStorageOffer(instanceType.StorageOffers, storageType)
	if offer == nil {
		names := make([]string, 0, len(instanceType.StorageOffers))
		for _, o := range instanceType.StorageOffers {
			names = append(names, string(o.Type))
		}
		field := "spec.storage.type"
		if storage.Type == "" {
			field = "the default storage type for " + string(provider)
		}
		return fmt.Errorf("%s, %s, is not offered for %s; set spec.storage.type to one of: %s",
			field, storageType, name, strings.Join(names, ", "))
	}
	return validateIOPS(*offer, name, storage.IOPS)
}

// validateIOPS applies the rule RavenDB Cloud checks on create: a disk class
// whose IOPS you may choose refuses a create that does not choose, and a class
// you may not choose refuses one that tries.
//
// The published range is computed from the shape's largest disk, so it is wider
// than the chosen size accepts. That is deliberate: it never rejects a value the
// cloud would have taken.
func validateIOPS(offer cloudapi.StorageOffer, instanceType string, requested *int32) error {
	switch {
	case offer.IOPS == nil && requested != nil:
		return fmt.Errorf(
			"spec.storage.iops cannot be set for %s storage on %s, which does not offer provisioned IOPS",
			offer.Type, instanceType)
	case offer.IOPS == nil:
		return nil
	case requested == nil:
		return fmt.Errorf(
			"spec.storage.iops is required for %s storage on %s; RavenDB Cloud accepts %d to %d",
			offer.Type, instanceType, offer.IOPS.Min, offer.IOPS.Max)
	case *requested < offer.IOPS.Min || *requested > offer.IOPS.Max:
		return fmt.Errorf(
			"spec.storage.iops is %d, but %s storage on %s accepts %d to %d",
			*requested, offer.Type, instanceType, offer.IOPS.Min, offer.IOPS.Max)
	}
	return nil
}

func findInstanceType(available []cloudapi.InstanceType, name string) *cloudapi.InstanceType {
	for i := range available {
		if available[i].Name == name {
			return &available[i]
		}
	}
	return nil
}

func findStorageOffer(offers []cloudapi.StorageOffer, want v1alpha1.StorageType) *cloudapi.StorageOffer {
	for i := range offers {
		if offers[i].Type == cloudapi.StorageType(want) {
			return &offers[i]
		}
	}
	return nil
}

func joinInt32s(values []int32) string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, strconv.Itoa(int(v)))
	}
	return strings.Join(out, ", ")
}
