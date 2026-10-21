package cloudsim

import (
	"fmt"
	"slices"
)

// What the metadata endpoints serve is copied from production, because
// RavenDB Cloud refuses anything off its lists: the disk sizes, vCPU and RAM
// each instance type sells, and the AWS regions.

var regionsByProvider = map[string][]string{
	"Aws":   {"eu-central-1", "eu-west-1", "us-east-1", "us-west-2", "ap-southeast-1"},
	"Azure": {"westeurope", "northeurope", "eastus"},
	"Gcp":   {"europe-west3", "europe-west4", "us-central1"},
}

// No iopsRange means the class does not let you choose, which is what the real
// API says for a class it does not charge per IOPS for.
var storageTypes = []storageTypeInfo{
	{Name: "SsdStandard"},
	{Name: "SsdPremium", IopsRange: &iopsRange{Min: 500, Max: 16000}},
}

// The instance types are the same in every region, which keeps the labs
// predictable. The real API varies them by region, which is why the operator
// asks per region.
var instanceTypes = []instanceTypeItem{
	{
		Name: "Free", Tier: "Free",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 2, RAM: 0.5, NumberOfNodes: 1,
			AvailableDiskSizes:    []int32{10},
			AvailableStorageTypes: []storageTypeInfo{storageTypes[0]},
		},
	},
	{
		Name: "Dev10", Tier: "Development",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 2, RAM: 0.5, NumberOfNodes: 1,
			AvailableDiskSizes:    []int32{10, 15, 20, 25},
			AvailableStorageTypes: storageTypes,
		},
	},
	{
		Name: "Dev20", Tier: "Development",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 2, RAM: 1, NumberOfNodes: 1,
			AvailableDiskSizes:    []int32{10, 20, 30, 40, 50},
			AvailableStorageTypes: storageTypes,
		},
	},
	{
		Name: "Dev30", Tier: "Development",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 2, RAM: 2, NumberOfNodes: 1,
			AvailableDiskSizes:    []int32{10, 20, 30, 50, 75},
			AvailableStorageTypes: storageTypes,
		},
	},
	{
		Name: "PB10", Tier: "Production",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 2, RAM: 2, NumberOfNodes: 3,
			AvailableDiskSizes:    []int32{10, 20, 30, 50, 75},
			AvailableStorageTypes: storageTypes,
		},
	},
	{
		Name: "PB20", Tier: "Production",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 2, RAM: 4, NumberOfNodes: 3,
			AvailableDiskSizes:    []int32{20, 40, 80, 100, 150},
			AvailableStorageTypes: storageTypes,
		},
	},
	{
		Name: "PB30", Tier: "Production",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 2, RAM: 8, NumberOfNodes: 3,
			AvailableDiskSizes:    []int32{50, 100, 150, 200, 250},
			AvailableStorageTypes: storageTypes,
		},
	},
	{
		Name: "PB40", Tier: "Production",
		Parameters: instanceTypeParameters{
			CPUPriority: "Basic",
			VirtualCPUs: 4, RAM: 16, NumberOfNodes: 3,
			AvailableDiskSizes:    []int32{100, 200, 300, 400, 500},
			AvailableStorageTypes: storageTypes,
		},
	},
}

// Channel names must be ones RavenDB Cloud knows, because production parses
// the value back into a fixed list of channels and answers 400 for anything
// else.
var releaseChannels = releaseChannelsResponse{
	DefaultReleaseChannel: "Stable72",
	ReleaseChannels: []releaseChannelItem{
		{Name: "Stable72", RavenVersion: "7.2", BuildType: "Stable"},
		{Name: "Stable71", RavenVersion: "7.1", BuildType: "Stable"},
		{Name: "Stable62", RavenVersion: "6.2", BuildType: "Stable"},
	},
}

func regionExists(provider, region string) bool {
	return slices.Contains(regionsByProvider[provider], region)
}

// findInstanceType looks an instance type up in what a region offers. On a
// refusal it returns the zero value, which reports zero hardware.
func findInstanceType(provider, region, name string) (instanceTypeItem, error) {
	i := slices.IndexFunc(instanceTypes, func(it instanceTypeItem) bool { return it.Name == name })
	if i < 0 || !regionExists(provider, region) {
		return instanceTypeItem{}, fmt.Errorf("instance type %q is not available in %s/%s", name, provider, region)
	}
	return instanceTypes[i], nil
}

// findInstanceTypeForTier is findInstanceType for a product of the given tier.
func findInstanceTypeForTier(provider, region, name, tier string) (instanceTypeItem, error) {
	spec, err := findInstanceType(provider, region, name)
	if err != nil {
		return instanceTypeItem{}, err
	}
	if spec.Tier != tier {
		return instanceTypeItem{}, fmt.Errorf("instance type %q belongs to tier %s, not %s", name, spec.Tier, tier)
	}
	return spec, nil
}
