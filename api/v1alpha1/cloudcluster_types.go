package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageSpec is the data disk you want.
//
// Sizes are plain integers in GB, the unit RavenDB Cloud sells disks in, rather
// than resource.Quantity, because RavenDB Cloud sells whole-GB disks from a
// fixed menu and a Quantity would invite "500Mi" only to have the cloud API
// reject it.
type StorageSpec struct {
	// SizeGB is the data disk size in GB. RavenDB Cloud sells a fixed set of
	// sizes per instance type.
	//
	// When the operator creates the cluster it checks this against the sizes
	// the instance type offers and, if it is not one of them, lists the ones
	// that are. After that the disk can only grow. A smaller size is reported
	// on the Synced condition and never sent, because cloud disks cannot
	// shrink, and so is a size RavenDB Cloud says this disk cannot move to.
	// Any other larger size is sent, and RavenDB Cloud judges it.
	// +kubebuilder:validation:Minimum=1
	// +optional
	SizeGB int32 `json:"sizeGB,omitempty"`

	// Not defaulted by the API server: the right class depends on
	// spec.provider, which a static default cannot see, and SsdStandard is
	// refused on GCP. EffectiveStorageType derives it instead.

	// Type is the disk class. Leave it unset and a cluster the operator
	// creates gets the right one for the provider: SsdStandard on AWS and
	// Azure, SsdPremium on GCP, which is the only class Google Cloud products
	// offer. On a cluster that already exists an unset type is not compared,
	// so the disk keeps the class it has.
	// +optional
	Type StorageType `json:"type,omitempty"`

	// IOPS provisions dedicated IOPS where the provider supports it. When the
	// operator creates the cluster it checks this against the range the disk
	// class allows. Left unset on a cluster that already exists, the disk
	// keeps the IOPS it has.
	// +kubebuilder:validation:Minimum=1
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// ThroughputMBps provisions dedicated disk throughput in MB/s where the
	// provider supports it. RavenDB Cloud takes a throughput change only
	// together with another storage change, so a change to this alone is
	// reported on the Synced condition and not applied.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ThroughputMBps *int32 `json:"throughputMBps,omitempty"`
}

// NetworkingSpec is the cloud-side firewall for the cluster.
type NetworkingSpec struct {
	// MaxItems is the largest any provider allows, so admission never rejects
	// a list RavenDB Cloud would take; the lower per-provider cap is checked in
	// internal/controller once the provider is resolved. A set, so server-side
	// apply merges two writers entry by entry instead of the last one replacing
	// the whole list.

	// AllowedIPs is the list of address ranges allowed to reach the cluster.
	// Leave it unset to use spec.defaults.allowedIPs on the CloudAccount.
	// RavenDB Cloud requires at least one entry to create a cluster.
	//
	// Every entry must be in CIDR notation, so a single address needs a
	// prefix: 203.0.113.7/32, not 203.0.113.7, which RavenDB Cloud refuses.
	// Order carries no meaning and duplicates are refused.
	//
	// RavenDB Cloud allows at most 24 entries on AWS and 200 on Azure and GCP.
	// The operator checks that before it creates the cluster.
	//
	// RavenDB Cloud takes this list at creation only. A later change is
	// reported on the Synced condition and not applied.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=200
	// +kubebuilder:validation:items:Format=cidr
	// +listType=set
	// +optional
	AllowedIPs []string `json:"allowedIPs,omitempty"`
}

// ConnectionSecretSpec controls the Secret the operator publishes so that
// applications in this namespace can reach the cluster: a pod mounts it and
// has the node URLs and the client certificate.
type ConnectionSecretSpec struct {
	// Name of the Secret to write. Defaults to "<cloudcluster-name>-connection".
	// It must be a valid Secret name, a DNS-1123 subdomain.
	//
	// If a Secret by that name already exists and was not created for this
	// cluster, the operator leaves it alone and Ready reports NameTaken.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	Name string `json:"name,omitempty"`

	// Enabled controls whether the operator publishes into the Secret.
	// Defaults to true.
	//
	// Setting it to false removes every key the operator wrote, the client
	// certificate and its password included. The Secret object itself is left
	// in place and is deleted with the CloudCluster. A pod that names one of
	// those keys, through secretKeyRef or a volume's items, then fails to
	// start until the keys are back; one that mounts the whole Secret sees an
	// empty directory.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// IncludeCertificate controls whether the client certificate downloaded
	// from RavenDB Cloud is placed in the Secret. Defaults to true. Turn it
	// off if certificates are distributed by another mechanism in your
	// cluster.
	// +kubebuilder:default=true
	// +optional
	IncludeCertificate *bool `json:"includeCertificate,omitempty"`
}

// subdomain and displayName are pinned harder than provider and region on
// purpose. displayName is the identity the operator looks the cluster up by,
// and both feed the create request, so letting one appear after the fact is
// how an operator ends up creating a second cluster nobody asked for. provider
// and region may still be added, so a manifest can pin a value it inherited
// from the CloudAccount defaults.

// CloudClusterSpec is the desired state of one RavenDB Cloud cluster.
//
// The display name is what identifies the cluster. It is unique within a
// RavenDB Cloud account and cannot be renamed there, so it names exactly one
// cluster for as long as that cluster exists. When a cluster by that name
// already exists, this object takes it over; when none does, the operator
// creates it. That is also what lets a Kubernetes cluster be rebuilt from git:
// every object finds its cluster again.
//
// Only the fields you set are managed. A field left out is left as the cluster
// has it, and on a cluster the operator creates it gets a default, which status
// reports.
//
// Fields fall into three groups, and it is worth knowing which is which before
// editing a live cluster:
//
//   - Applied, under managementPolicy Full. instanceType, nodes, and storage:
//     a larger sizeGB, type and iops, with throughputMBps riding along on any
//     of those. Each storage field is compared only when it is set.
//   - Rejected at admission. accountRef, tier, subdomain and displayName can
//     never change, and provider and region can never change away from a
//     value they already had. RavenDB Cloud cannot move a cluster between any
//     of them, so a manifest that asks is refused rather than left to differ
//     forever. subdomain cannot be added later either, and displayName only
//     as the object name it already defaulted to; provider and region can.
//   - Accepted and reported, never applied. releaseChannel and
//     networking.allowedIPs, which RavenDB Cloud takes at creation only, and
//     a change to storage.throughputMBps alone, which it refuses. The
//     operator reports the difference on the Synced condition and tells you
//     to change it in the portal.
//
// +kubebuilder:validation:XValidation:rule="self.accountRef.name == oldSelf.accountRef.name",message="accountRef is immutable: a cluster cannot move between RavenDB Cloud accounts. Delete this object and apply it again with the right accountRef; with the default deletionPolicy Retain nothing in RavenDB Cloud is touched",fieldPath=".accountRef.name"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider != 'GCP' || !has(self.storage) || !has(self.storage.type) || self.storage.type != 'SsdStandard'",message="Google Cloud products do not offer SsdStandard; leave spec.storage.type unset, or set it to SsdPremium",fieldPath=".storage.type"
// +kubebuilder:validation:XValidation:rule="!has(self.nodes) || self.tier == 'Production' || self.nodes == 1",message="a Free or Development cluster has exactly one node: additional nodes are a Production feature in RavenDB Cloud. Leave spec.nodes unset or set it to 1",fieldPath=".nodes"
// +kubebuilder:validation:XValidation:rule="!has(self.nodes) || self.tier != 'Production' || self.nodes >= 3",message="a Production cluster keeps its three standard nodes A, B and C, which RavenDB Cloud never removes. Set spec.nodes between 3 and 6",fieldPath=".nodes"
// +kubebuilder:validation:XValidation:rule="self.tier == oldSelf.tier",message="tier is immutable: RavenDB Cloud cannot move a cluster between tiers. To use another tier, delete this object and apply it again with another spec.displayName; with the default deletionPolicy Retain nothing in RavenDB Cloud is touched",fieldPath=".tier"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.provider) || (has(self.provider) && self.provider == oldSelf.provider)",message="provider is immutable: RavenDB Cloud cannot move a cluster between providers. To use another provider, delete this object and apply it again with another spec.displayName; with the default deletionPolicy Retain nothing in RavenDB Cloud is touched",fieldPath=".provider"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.region) || (has(self.region) && self.region == oldSelf.region)",message="region is immutable: RavenDB Cloud cannot move a cluster between regions. To use another region, delete this object and apply it again with another spec.displayName; with the default deletionPolicy Retain nothing in RavenDB Cloud is touched",fieldPath=".region"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.subdomain) == has(self.subdomain) && (!has(self.subdomain) || self.subdomain == oldSelf.subdomain)",message="subdomain is immutable, and cannot be added later either: it is baked into the cluster DNS names",fieldPath=".subdomain"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.creationMode) || (has(self.creationMode) && (self.creationMode == oldSelf.creationMode || (oldSelf.creationMode == 'TakeOverOrCreate' && self.creationMode == 'TakeOverOnly')))",message="creationMode cannot change, except from TakeOverOrCreate to TakeOverOnly, which confirms that taking over the cluster is meant. Delete this object and apply it again with another creationMode; with the default deletionPolicy Retain nothing in RavenDB Cloud is touched",fieldPath=".creationMode"
// +kubebuilder:validation:XValidation:rule="!has(self.creationMode) || self.creationMode != 'CreateOnly' || !has(self.managementPolicy) || self.managementPolicy != 'Observe'",message="creationMode CreateOnly needs managementPolicy Full: Observe never creates a cluster",fieldPath=".creationMode"
type CloudClusterSpec struct {
	// AccountRef selects the CloudAccount in this namespace whose credential
	// and defaults this cluster uses. It cannot be changed: a cluster cannot
	// move between RavenDB Cloud accounts.
	//
	// On Kubernetes 1.31 or newer, this lists the clusters of one account:
	//
	//   kubectl get cloudclusters --field-selector spec.accountRef.name=<account>
	AccountRef LocalObjectReference `json:"accountRef"`

	// DisplayName is the cluster name shown in the RavenDB Cloud portal, and
	// the name the operator finds the cluster by. Defaults to the CloudCluster
	// object name. RavenDB Cloud caps it at 100 characters.
	//
	// If a cluster by this name exists in the account, this object takes it
	// over; if none does, the operator creates one with this name. RavenDB
	// Cloud keeps display names unique per account, ignoring case, and has no
	// way to rename a cluster, which is what makes the name a safe identity.
	//
	// Set, it cannot be changed. Omitted, it can be added later only as the
	// object name it defaulted to, ignoring case, which is what the export
	// tool writes for a cluster this object already found.
	// +kubebuilder:validation:MaxLength=100
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Tier selects the RavenDB Cloud product tier. It cannot be changed:
	// RavenDB Cloud cannot move a cluster between tiers.
	//
	// Free clusters cannot be created through the RavenDB Cloud API, only in
	// the portal. A Free CloudCluster therefore only ever takes over a cluster
	// that already exists, and until one by its name does it reports
	// ClusterNotFound.
	//
	// For a cluster that already exists, this must be the tier it runs on. A
	// different tier means the name belongs to some other cluster: the object
	// does not bind to it and Ready reports NameTaken.
	Tier Tier `json:"tier"`

	// Provider is the hosting cloud. Defaults to the CloudAccount default when
	// the operator creates the cluster; on one it takes over, unset means
	// whatever the cluster runs on, and a provider that differs from it means
	// the name belongs to some other cluster (NameTaken).
	//
	// Once set it cannot be changed or removed. It can be added later, to pin
	// the value the cluster already has.
	// +optional
	Provider Provider `json:"provider,omitempty"`

	// Region is the provider region, for example eu-central-1. Defaults to the
	// CloudAccount default when the operator creates the cluster, and is then
	// checked against the regions the CloudAccount lists in status.providers;
	// if it is not one of them, the message lists those that are. On a cluster
	// it takes over, a region that differs means the name belongs to some
	// other cluster (NameTaken).
	//
	// Once set it cannot be changed or removed. It can be added later, to pin
	// the value the cluster already has.
	// +optional
	Region string `json:"region,omitempty"`

	// InstanceType is the RavenDB Cloud node shape, for example PB40. Leave it
	// unset to let the operator pick the smallest shape offered for the tier in
	// the chosen region when it creates the cluster; the resolved value is
	// written to status. A shape the tier does not offer is refused before the
	// cluster is created, and the message lists the ones it does.
	//
	// The smallest Production shape has a shared CPU and is sold without
	// additional nodes, so a cluster created from it stays at three. Name a PB
	// shape or larger for a cluster that will grow, or change this field later:
	// RavenDB Cloud moves a running cluster to another shape.
	// +optional
	InstanceType string `json:"instanceType,omitempty"`

	// ReleaseChannel is the RavenDB version stream, for example Stable72. The
	// ones the account may use are listed in the CloudAccount's status.
	// Defaults to the CloudAccount default, then to the account default
	// reported by RavenDB Cloud. RavenDB Cloud takes it at creation only, so a
	// later change is reported on the Synced condition and not applied.
	// +optional
	ReleaseChannel string `json:"releaseChannel,omitempty"`

	// The lowest count is not checked here: it is the node count of the
	// instance type, which only the live instance-type list knows.

	// Nodes is the desired cluster size. Leave it unset to accept the node
	// count that comes with the instance type. Changing it later makes the
	// operator add or remove nodes one at a time, which is the only way the
	// RavenDB Cloud API supports it. RavenDB Cloud refuses fewer nodes than
	// the instance type comes with.
	//
	// A Production cluster has between three and six: the three standard nodes
	// A, B and C, which RavenDB Cloud never removes, and at most three
	// additional ones on top. A Free or Development cluster has exactly one.
	// Some instance types are sold without additional nodes, and on those the
	// count cannot change at all; the operator reports that on the Synced
	// condition rather than asking.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=6
	// +optional
	Nodes *int32 `json:"nodes,omitempty"`

	// Storage is the data disk. Leave it unset to get the smallest size the
	// instance type offers, which status then reports: RavenDB Cloud sells
	// each instance type its own fixed set of sizes, so naming one before
	// choosing an instance type means guessing.
	//
	// Each field is compared only when it is set, and one left out keeps the
	// value the disk has. Growing sizeGB triggers an online resize.
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// Networking is the cloud-side firewall.
	// +optional
	Networking NetworkingSpec `json:"networking,omitempty"`

	// Subdomain is the DNS label under the account domain, and it ends up in
	// every node URL. Leave it unset to derive it from displayName. It is only
	// sent when the operator creates the cluster.
	//
	// RavenDB Cloud allows at most 13 characters, letters, digits and dashes
	// only, with no leading or trailing dash, and the name must not be taken
	// already. The first three are checked at kubectl apply. Whether a name is
	// taken only RavenDB Cloud knows, so a taken one comes back as a refused
	// create.
	//
	// Whichever way you leave this field on the first apply is how it stays:
	// set, it cannot be changed; omitted, it cannot be added. So if the
	// derived name turns out to be taken, no edit to this object fixes it.
	// Delete the object and apply it again with an explicit subdomain; the
	// cluster was never created, so nothing is lost.
	//
	// In a shared account, where names are likely to collide, setting it on
	// the first apply is worth it.
	// +kubebuilder:validation:MaxLength=13
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([-a-zA-Z0-9]*[a-zA-Z0-9])?$`
	// +optional
	Subdomain string `json:"subdomain,omitempty"`

	// ManagementPolicy caps what the operator may change in RavenDB Cloud.
	//
	//   Full     creates the cluster when none exists, and applies the changes
	//            the RavenDB Cloud API supports: instance type, storage and
	//            node count.
	//   Observe  changes nothing, creating included. The operator reads the
	//            cluster, publishes its connection Secret and reports on the
	//            Synced condition how the manifest differs.
	//
	// Left unset, it is Full for a cluster this object created and Observe for
	// one it found already running, so that taking a cluster over never
	// reshapes it until you say so. The resolved value is in
	// status.managementPolicy.
	//
	// deletionPolicy does not depend on it, even under Observe: what Terminate
	// may end is decided by creationMode.
	// +optional
	ManagementPolicy ManagementPolicy `json:"managementPolicy,omitempty"`

	// CreationMode decides what the operator does with the name it finds the
	// cluster by.
	//
	//   TakeOverOrCreate  takes over a cluster that already has the name, and
	//                     creates one when none does. The default.
	//   TakeOverOnly      never creates. With no cluster of that name the
	//                     object reports ClusterNotFound.
	//   CreateOnly        never takes over. A cluster that already has the
	//                     name is reported as NameTaken and left alone.
	//
	// It also decides what deletionPolicy Terminate may end. A cluster this
	// object created is terminated. One it took over is terminated only under
	// TakeOverOnly, where taking it over was asked for; one found under
	// TakeOverOrCreate is left running, and a Warning event says so.
	//
	// kubectl ravendb-cloud export writes TakeOverOnly, so a manifest applied again
	// from git never creates an empty cluster in place of one it could not
	// find. It cannot change once set, with one exception: TakeOverOrCreate
	// may become TakeOverOnly, which confirms that a cluster this object took
	// over is meant to be its own.
	// +kubebuilder:default=TakeOverOrCreate
	// +optional
	CreationMode CreationMode `json:"creationMode,omitempty"`

	// DeletionPolicy decides what deleting this object does to the cloud
	// cluster. Retain, the default, keeps the cluster running. Terminate
	// terminates it, which cannot be undone, when this object created it or
	// took it over under creationMode TakeOverOnly.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// ConnectionSecret controls the Secret the operator publishes with the
	// cluster's URLs and client certificate, for applications in this
	// namespace to mount.
	// +optional
	ConnectionSecret *ConnectionSecretSpec `json:"connectionSecret,omitempty"`

	// PollInterval is how often the operator re-reads a settled cluster from
	// RavenDB Cloud. Defaults to five minutes. While RavenDB Cloud is creating
	// or changing the cluster, the operator looks every 30 seconds, or at this
	// interval when it is shorter.
	// +kubebuilder:default="5m"
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`
}

// StorageStatus is the observed data disk.
type StorageStatus struct {
	// SizeGB is the disk size in GB.
	SizeGB int32 `json:"sizeGB,omitempty"`
	// Type is the disk class.
	Type StorageType `json:"type,omitempty"`
	// IOPS is the provisioned IOPS, when the disk has any.
	// +optional
	IOPS *int32 `json:"iops,omitempty"`
	// ThroughputMBps is the provisioned throughput in MB/s, when the disk has
	// any.
	// +optional
	ThroughputMBps *int32 `json:"throughputMBps,omitempty"`
}

// HardwareStatus is the observed node shape.
type HardwareStatus struct {
	// CPUs is the number of CPUs per node.
	// +optional
	CPUs int32 `json:"cpus,omitempty"`

	// CPUPriority is the CPU priority RavenDB Cloud reports for the shape.
	// +optional
	CPUPriority string `json:"cpuPriority,omitempty"`

	// Not GB: in whole GB the half-gigabyte shapes (Free and Dev10) truncate to
	// 0, and omitempty then drops the field.

	// RAMMB is the node memory in MB, 1024 to each GB RavenDB Cloud reports.
	// MB rather than GB because the smallest shapes have half a gigabyte.
	// +optional
	RAMMB int32 `json:"ramMB,omitempty"`

	// Storage is the observed data disk.
	// +optional
	Storage *StorageStatus `json:"storage,omitempty"`
}

// CloudClusterStatus is the observed state of the cluster in RavenDB Cloud.
type CloudClusterStatus struct {
	// Conditions answer two questions, each with one word in kubectl get.
	//
	// Ready, the STATUS column: can I use it? True exactly when the reason is
	// Running. Otherwise the reason names the stage or the blocker:
	//
	//   Creating               RavenDB Cloud is creating the cluster
	//   Updating               RavenDB Cloud is applying a change
	//   WaitingForCertificate  serving; the client certificate is not issued yet
	//   WaitingForAccount      the CloudAccount is not usable; the message says why
	//   InvalidSpec            the operator will not send this; the message names the field
	//   Refused                RavenDB Cloud refused to create the cluster
	//   QuotaReached           the account is at its product quota; retried on a timer
	//   NameTaken              the name belongs to another cluster or object, or the
	//                          connection Secret's name is taken
	//   ClusterNotFound        no cluster by that name, and this object may not create one
	//   DeletedInCloud         the cluster was deleted outside this operator
	//   AwaitingPayment        RavenDB Cloud waits for payment; resolve it in the portal
	//   CreationFailed         RavenDB Cloud could not create it and notified its support
	//   SecretNotWritten       serving, but the connection Secret could not be written
	//   Deleting               this object is being deleted; the message says what it waits for
	//   CloudUnreachable       Unknown: the RavenDB Cloud API did not answer
	//   KeyRejected            Unknown: RavenDB Cloud rejected the account's API key
	//   UnexpectedResponse     Unknown: RavenDB Cloud answered in a way this operator cannot read
	//
	// Unknown means the operator could not look. The cluster is probably still
	// serving, and the rest of status is what was seen at lastSyncTime.
	//
	// Synced, the CHANGES column: did the manifest reach RavenDB Cloud? Set once
	// the object is bound to a cluster.
	//
	//   Applied     everything the manifest sets matches RavenDB Cloud
	//   Applying    a change was sent and RavenDB Cloud is applying it
	//   Waiting     RavenDB Cloud takes the change later, after a storage window or
	//               once the cluster is no longer busy; asked again each poll
	//   Refused     RavenDB Cloud refused the change; not asked again until the spec changes
	//   NotApplied  a difference the operator may not or cannot apply; the message says which
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ProductID is the RavenDB Cloud cluster id this object is bound to. Once
	// set, this object manages that cluster and no other.
	// +optional
	ProductID string `json:"productID,omitempty"`

	// Adopted records whether this object found its cluster already running
	// rather than creating it. It decides the default managementPolicy.
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// ProvisionAttemptedAt is when the operator asked RavenDB Cloud to create
	// the cluster. It is set just before the request, so that if the operator
	// stops before recording the productID, the cluster it then finds by name
	// is known to be one this object created rather than one it found.
	// +optional
	ProvisionAttemptedAt *metav1.Time `json:"provisionAttemptedAt,omitempty"`

	// Recovery reads this, never spec. The CRD forbids editing
	// spec.displayName, but a status field the operator wrote itself is the
	// guarantee, and the CEL rule is the courtesy error message.

	// ProvisionAttemptedName is the display name sent with the create request
	// ProvisionAttemptedAt marks. Until productID is set, the operator looks
	// the cluster up by this name.
	// +optional
	ProvisionAttemptedName string `json:"provisionAttemptedName,omitempty"`

	// ManagementPolicy is the effective policy after defaulting.
	// +optional
	ManagementPolicy ManagementPolicy `json:"managementPolicy,omitempty"`

	// CloudStatus is the raw RavenDB Cloud status string, passed through
	// unmodified so an unexpected state is visible rather than flattened.
	// +optional
	CloudStatus string `json:"cloudStatus,omitempty"`

	// DisplayName as seen in the RavenDB Cloud portal.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Provider is the hosting cloud the cluster runs on.
	// +optional
	Provider Provider `json:"provider,omitempty"`
	// Region is the provider region the cluster runs in.
	// +optional
	Region string `json:"region,omitempty"`
	// Tier is the RavenDB Cloud product tier the cluster runs on.
	// +optional
	Tier Tier `json:"tier,omitempty"`
	// InstanceType is the node shape the cluster runs on.
	// +optional
	InstanceType string `json:"instanceType,omitempty"`
	// ReleaseChannel is the RavenDB version stream the cluster follows.
	// +optional
	ReleaseChannel string `json:"releaseChannel,omitempty"`

	// URLs are the per-node HTTPS endpoints, index-aligned with nodeTags.
	// +optional
	URLs []string `json:"urls,omitempty"`

	// NodeTags are the RavenDB cluster node tags (A, B, C ...).
	// +optional
	NodeTags []string `json:"nodeTags,omitempty"`

	// Nodes is the observed node count.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`

	// StudioURL opens the RavenDB Studio for this cluster.
	// +optional
	StudioURL string `json:"studioURL,omitempty"`

	// Hardware is the observed node shape.
	// +optional
	Hardware *HardwareStatus `json:"hardware,omitempty"`

	// AllowedIPs is the firewall allow list as RavenDB Cloud currently has it.
	// Compare it with spec to spot a difference; RavenDB Cloud cannot change
	// it after creation.
	// +optional
	AllowedIPs []string `json:"allowedIPs,omitempty"`

	// ConnectionSecretName is the Secret holding URLs and certificate.
	// +optional
	ConnectionSecretName string `json:"connectionSecretName,omitempty"`

	// CertificateRefreshedAt is when the client certificate was last downloaded.
	// +optional
	CertificateRefreshedAt *metav1.Time `json:"certificateRefreshedAt,omitempty"`

	// CertificateExpiresAt is when the published client certificate stops
	// working. RavenDB Cloud issues them for five years and regenerates one
	// month before expiry; the operator republishes the replacement shortly
	// after that, so this should only ever move forwards.
	// +optional
	CertificateExpiresAt *metav1.Time `json:"certificateExpiresAt,omitempty"`

	// LastSyncTime is when cloud state was last read successfully.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rcc;rcluster,categories=ravendb
// CRD field selectors need Kubernetes 1.31 or newer. An older API server
// prunes the field, and the selector is simply not offered.
// +kubebuilder:selectablefield:JSONPath=`.spec.accountRef.name`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Changes",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].reason`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.status.tier`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.status.region`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.nodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Display-Name",type=string,priority=1,JSONPath=`.status.displayName`
// +kubebuilder:printcolumn:name="Policy",type=string,priority=1,JSONPath=`.status.managementPolicy`
// +kubebuilder:printcolumn:name="Adopted",type=boolean,priority=1,JSONPath=`.status.adopted`
// +kubebuilder:printcolumn:name="Instance",type=string,priority=1,JSONPath=`.status.instanceType`
// +kubebuilder:printcolumn:name="Cluster-ID",type=string,priority=1,JSONPath=`.status.productID`
// +kubebuilder:printcolumn:name="URL",type=string,priority=1,JSONPath=`.status.urls[0]`
// +kubebuilder:printcolumn:name="Message",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=="Ready")].message`
// +kubebuilder:validation:XValidation:rule="has(oldSelf.spec.displayName) ? (has(self.spec.displayName) && self.spec.displayName == oldSelf.spec.displayName) : (!has(self.spec.displayName) || self.spec.displayName.lowerAscii() == self.metadata.name)",message="displayName is immutable: it is the name the operator finds the cluster by. It can be added later only as the name this object already uses, which is what the export tool writes",fieldPath=".spec.displayName"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 characters: the connection Secret carries it in the cloud.ravendb.net/cluster label, and a label value cannot be longer"

// CloudCluster is one RavenDB Cloud cluster, created by this operator or found
// already running and taken over.
//
// RavenDB Cloud calls it a product. It is one to six RavenDB nodes behind a
// shared DNS name.
type CloudCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudClusterSpec   `json:"spec,omitempty"`
	Status CloudClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CloudClusterList contains a list of CloudCluster.
type CloudClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudCluster `json:"items"`
}

func init() {
	registerType(&CloudCluster{}, &CloudClusterList{})
}
