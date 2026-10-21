package v1alpha1

// The CRD uses the spellings people write, AWS and GCP. The cloud API spells
// them Aws and Gcp; internal/cloudapi translates.

// Provider is a RavenDB Cloud hosting provider.
// +kubebuilder:validation:Enum=AWS;Azure;GCP
type Provider string

const (
	ProviderAWS   Provider = "AWS"
	ProviderAzure Provider = "Azure"
	ProviderGCP   Provider = "GCP"
)

// Tier is the RavenDB Cloud product tier. It decides which instance types and
// node counts you may ask for.
// +kubebuilder:validation:Enum=Free;Development;Production
type Tier string

const (
	TierFree        Tier = "Free"
	TierDevelopment Tier = "Development"
	TierProduction  Tier = "Production"
)

// StorageType is a RavenDB Cloud disk class.
// +kubebuilder:validation:Enum=SsdStandard;SsdPremium
type StorageType string

const (
	StorageSsdStandard StorageType = "SsdStandard"
	StorageSsdPremium  StorageType = "SsdPremium"
)

// ManagementPolicy decides how far the operator may go in RavenDB Cloud.
//
// Finding a cluster somebody built by hand should not give the operator
// permission to reshape it on the next pass, so a found cluster defaults to
// Observe.
// +kubebuilder:validation:Enum=Observe;Full
type ManagementPolicy string

const (
	// ManagementObserve reads cloud state, publishes connection details and
	// reports differences, but never changes anything in RavenDB Cloud, and
	// never creates a cluster either. Default for a cluster the object found.
	// deletionPolicy does not depend on it: whether Terminate ends the cluster
	// is decided by creationMode.
	ManagementObserve ManagementPolicy = "Observe"

	// ManagementFull creates the cluster when none exists, and applies the
	// changes the cloud API supports: instance type, storage and node count.
	// Default for a cluster the object created.
	ManagementFull ManagementPolicy = "Full"
)

// DeletionPolicy decides what happens to the cloud cluster when the Kubernetes
// object is deleted.
// +kubebuilder:validation:Enum=Retain;Terminate
type DeletionPolicy string

const (
	// DeletionRetain leaves the cloud cluster running and only drops the
	// Kubernetes object. This is the default, because a stray kubectl delete
	// must never destroy a production database.
	DeletionRetain DeletionPolicy = "Retain"

	// DeletionTerminate terminates the cloud cluster. Irreversible.
	DeletionTerminate DeletionPolicy = "Terminate"
)

// CreationMode decides what the operator may do with the name it finds the
// cluster by: take over a cluster that already has it, create one when none
// does, or both.
// +kubebuilder:validation:Enum=TakeOverOrCreate;TakeOverOnly;CreateOnly
type CreationMode string

const (
	// CreationTakeOverOrCreate takes over a cluster that has the name and
	// creates one when none does. The default.
	CreationTakeOverOrCreate CreationMode = "TakeOverOrCreate"

	// CreationTakeOverOnly never creates. No cluster with the name is reported
	// as ClusterNotFound, and nothing is created in its place.
	CreationTakeOverOnly CreationMode = "TakeOverOnly"

	// CreationCreateOnly never takes over. A cluster that already has the name
	// is reported as NameTaken and left alone.
	CreationCreateOnly CreationMode = "CreateOnly"
)
