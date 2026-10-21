// Package cloudapi is the operator's only door to the RavenDB Cloud control plane.
//
// Controllers depend on the Client interface in client.go, never on the HTTP
// implementation. There is one implementation and no fake: tests point the
// real client at internal/cloudsim over HTTP, so the wire mapping and the retry
// and pacing logic stay under test.
//
// The types here are the operator's own domain model, decoupled from the wire
// shapes in wire.go, so a rename on the cloud side does not ripple into the
// CRDs.
//
// RavenDB Cloud calls a provisioned cluster a "product". This package calls it
// a Cluster: Kubernetes CloudCluster <-> cloudapi.Cluster <-> RavenDB Cloud
// product.
package cloudapi

import "time"

// Provider is a RavenDB Cloud hosting provider.
type Provider string

const (
	ProviderAWS   Provider = "AWS"
	ProviderAzure Provider = "Azure"
	ProviderGCP   Provider = "GCP"
)

// Tier is the RavenDB Cloud product tier. It decides which instance types,
// node counts and features are available.
type Tier string

const (
	TierFree        Tier = "Free"
	TierDevelopment Tier = "Development"
	TierProduction  Tier = "Production"
)

// StorageType is a RavenDB Cloud disk class.
type StorageType string

const (
	StorageSsdStandard StorageType = "SsdStandard"
	StorageSsdPremium  StorageType = "SsdPremium"
)

// DeploymentType is the product layout. RavenDB Cloud also sells Serverless,
// which this operator does not offer and so does not name.
type DeploymentType string

// DeploymentStandard is the only layout the operator creates.
const DeploymentStandard DeploymentType = "Standard"

// ClusterStatus is the lifecycle state RavenDB Cloud reports for a cluster.
// These values mirror the cloud API exactly so that an unexpected state is
// visible to the user rather than silently collapsed into "not ready".
type ClusterStatus string

const (
	StatusCreating         ClusterStatus = "Creating"
	StatusActive           ClusterStatus = "Active"
	StatusDeployingChanges ClusterStatus = "DeployingChanges"
	StatusTerminating      ClusterStatus = "Terminating"
	StatusTerminated       ClusterStatus = "Terminated"
	StatusError            ClusterStatus = "Error"
	StatusAwaitingPayment  ClusterStatus = "AwaitingPayment"
)

// IsTransitional reports whether the cloud side is mid-operation, meaning the
// operator should requeue and look again rather than act.
func (s ClusterStatus) IsTransitional() bool {
	return s == StatusCreating || s == StatusDeployingChanges || s == StatusTerminating
}

// IsServing reports whether the cluster can accept client connections.
func (s ClusterStatus) IsServing() bool { return s == StatusActive }

// AccountInfo identifies the RavenDB Cloud account behind an API key.
// Fetching it is how the operator proves a key is valid.
type AccountInfo struct {
	DomainName string
}

// ClusterRef is the cheap identity of a cluster, as returned by the list
// endpoint. Finding a cluster by its display name goes through these.
type ClusterRef struct {
	ID   string
	Name string
}

// Storage describes a cluster's data disk.
type Storage struct {
	SizeGiB        int32
	Type           StorageType
	IOPS           *int32
	ThroughputMBps *float64

	// ResizableTo is the sizes this disk may grow to, narrower than the
	// create-time menu: the current size and up. Reported only, so empty on a
	// create request.
	ResizableTo []int32
}

// Hardware is the resolved shape of a running cluster's nodes.
type Hardware struct {
	CPUPriority string
	CPUs        int32
	RAMGiB      float64
	Storage     Storage
}

// Cluster is the full observed state of one RavenDB Cloud cluster.
type Cluster struct {
	ID             string
	DisplayName    string
	Provider       Provider
	Region         string
	Tier           Tier
	InstanceType   string
	ReleaseChannel string
	Status         ClusterStatus

	// URLs are the per-node HTTPS endpoints clients connect to.
	URLs []string
	// NodeTags are RavenDB cluster node tags (A, B, C ...), index-aligned with URLs.
	NodeTags []string
	// AllowedIPs is the cloud-side firewall allow list.
	AllowedIPs []string

	Hardware Hardware
}

// NodeCount is the number of nodes currently in the cluster.
func (c *Cluster) NodeCount() int32 { return int32(len(c.NodeTags)) }

// CreateClusterRequest is everything RavenDB Cloud needs to provision a
// cluster. ValidateCreate says which fields are required.
type CreateClusterRequest struct {
	DisplayName    string
	Provider       Provider
	Region         string
	Tier           Tier
	InstanceType   string
	ReleaseChannel string
	SubdomainName  string
	DeploymentType DeploymentType

	Storage    Storage
	AllowedIPs []string
}

// StorageChange is a request to resize or re-class a cluster's disk.
type StorageChange struct {
	SizeGiB        int32
	Type           StorageType
	IOPS           *int32
	ThroughputMBps *float64
}

// StorageOffer is one disk class a shape sells.
type StorageOffer struct {
	Type StorageType

	// IOPS is nil when the class does not let you choose. RavenDB Cloud
	// publishes a range only for classes that charge per IOPS, and for those a
	// create without an iops value is refused.
	IOPS *IOPSRange
}

// IOPSRange is the provisioned IOPS a disk class accepts, inclusive.
type IOPSRange struct {
	Min int32
	Max int32
}

// InstanceType is one orderable node shape in a region.
type InstanceType struct {
	Name        string
	Tier        Tier
	VirtualCPUs int32
	RAMGiB      float64
	// NumberOfNodes is the node count the shape comes with.
	NumberOfNodes int32
	DiskSizesGiB  []int32
	StorageOffers []StorageOffer
}

// ReleaseChannel is a RavenDB version stream (for example "Stable72").
type ReleaseChannel struct {
	Name         string
	RavenVersion string
	BuildType    string
}

// ReleaseChannels is the list of version streams plus the account default.
type ReleaseChannels struct {
	Default  string
	Channels []ReleaseChannel
}

// ClientCertificate is the downloadable certificate bundle that grants access
// to a cluster.
type ClientCertificate struct {
	Data []byte
	// RetrievedAt lets the controller age out and refresh the material.
	RetrievedAt time.Time
}
