package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeySelector points at one key inside a Secret in the same namespace.
type SecretKeySelector struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key inside the Secret holding the value. Defaults to apiKey.
	// +kubebuilder:default=apiKey
	// +optional
	Key string `json:"key,omitempty"`
}

// LocalObjectReference names another object in the same namespace.
type LocalObjectReference struct {
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AccountDefaults are inherited by every CloudCluster that references this
// account, so provider, region and the like are set once rather than in every
// CloudCluster manifest.
type AccountDefaults struct {
	// Provider used when a CloudCluster does not name one.
	// +optional
	Provider Provider `json:"provider,omitempty"`

	// Region used when a CloudCluster does not name one.
	// +optional
	Region string `json:"region,omitempty"`

	// ReleaseChannel used when a CloudCluster does not name one. When this is
	// empty too, the operator falls back to the account default reported by
	// RavenDB Cloud.
	// +optional
	ReleaseChannel string `json:"releaseChannel,omitempty"`

	// A set, so server-side apply merges two writers entry by entry instead of
	// the last one replacing the whole list.

	// AllowedIPs used when a CloudCluster does not list any. RavenDB Cloud
	// requires at least one allow-list entry to create a cluster.
	//
	// The same rules apply as to spec.networking.allowedIPs on a CloudCluster:
	// every entry is in CIDR notation, so a single address needs a prefix
	// (203.0.113.7/32, not 203.0.113.7), order carries no meaning, and
	// duplicates are refused.
	// +kubebuilder:validation:MaxItems=200
	// +kubebuilder:validation:items:Format=cidr
	// +listType=set
	// +optional
	AllowedIPs []string `json:"allowedIPs,omitempty"`
}

// CloudAccountSpec is the credential and the shared defaults for one RavenDB
// Cloud account.
type CloudAccountSpec struct {
	// APIKeySecretRef points at the Secret holding the RavenDB Cloud API key.
	// The operator watches the Secret, so a new key takes effect as soon as it
	// is written there. The key is never written to status, events or logs.
	APIKeySecretRef SecretKeySelector `json:"apiKeySecretRef"`

	// There is no per-account endpoint, on purpose. The operator reads the
	// Secret this names and sends its value to RavenDB Cloud, so an endpoint
	// anyone who can write a CloudAccount could set would let them send any
	// Secret in the namespace to a server of their choosing.

	// Defaults are inherited by CloudClusters that reference this account.
	// +optional
	Defaults *AccountDefaults `json:"defaults,omitempty"`

	// RefreshIntervalSeconds is how often, in seconds, the operator re-reads
	// the region and release channel lists held in status.providers and
	// status.releaseChannels. Defaults to 3600, one hour, because the lists
	// rarely change.
	//
	// Instance types and disk offers are not covered. They are never cached,
	// and are read fresh whenever the operator creates a cluster.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=60
	// +optional
	RefreshIntervalSeconds *int32 `json:"refreshIntervalSeconds,omitempty"`
}

// ProviderRegions is the set of regions available for one provider.
type ProviderRegions struct {
	// Provider these regions belong to.
	Provider Provider `json:"provider"`

	// Regions available to this account.
	// +optional
	Regions []string `json:"regions,omitempty"`
}

// ReleaseChannelStatus is one RavenDB version stream offered to this account.
type ReleaseChannelStatus struct {
	// Name of the channel, as used in CloudCluster spec.releaseChannel.
	Name string `json:"name"`

	// RavenVersion currently served by the channel.
	// +optional
	RavenVersion string `json:"ravenVersion,omitempty"`

	// BuildType is Stable or Development.
	// +optional
	BuildType string `json:"buildType,omitempty"`
}

// Instance types and disk offers are not mirrored here: they vary by region as
// well as by provider, so mirroring them would cost a request per region on
// every refresh.

// CloudAccountStatus reports credential health and what this account may order.
//
// The regions and release channels are listed here, so kubectl describe
// cloudaccount answers what a CloudCluster using this account can ask for.
// Instance types and disk offers are not: the operator reads them from RavenDB
// Cloud when it creates a cluster, and names the ones on offer if a manifest
// asks for one that is not.
type CloudAccountStatus struct {
	// Conditions hold Ready, whose reason is the STATUS column:
	//
	//   Verified            RavenDB Cloud accepted the API key
	//   KeyMissing          the Secret or the key in it is not there yet
	//   KeyRejected         RavenDB Cloud does not accept the key; put a new one in the Secret
	//   CloudUnreachable    Unknown: RavenDB Cloud did not answer; retried with backoff
	//   UnexpectedResponse  Unknown: RavenDB Cloud answered in a way this operator cannot read
	//   InUse               the account is being deleted and CloudClusters still use it;
	//                       Ready keeps the status of the last verification
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DomainName is the DNS domain RavenDB Cloud assigned to this account.
	// +optional
	DomainName string `json:"domainName,omitempty"`

	// Providers lists the regions available, per cloud provider.
	// +optional
	Providers []ProviderRegions `json:"providers,omitempty"`

	// ReleaseChannels lists the RavenDB version streams available.
	// +optional
	ReleaseChannels []ReleaseChannelStatus `json:"releaseChannels,omitempty"`

	// DefaultReleaseChannel is what RavenDB Cloud picks when none is named.
	// +optional
	DefaultReleaseChannel string `json:"defaultReleaseChannel,omitempty"`

	// LastVerifiedTime is when the credential was last confirmed to work.
	// +optional
	LastVerifiedTime *metav1.Time `json:"lastVerifiedTime,omitempty"`

	// Kept apart from LastVerifiedTime because verifying the credential is one
	// cheap call, while re-reading the lists costs four.

	// RefreshedAt is when the region and release channel lists were last read
	// from RavenDB Cloud. They are re-read every spec.refreshIntervalSeconds.
	// +optional
	RefreshedAt *metav1.Time `json:"refreshedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rca;rcaccount,categories=ravendb
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.status.domainName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Default-Channel",type=string,priority=1,JSONPath=`.status.defaultReleaseChannel`
// +kubebuilder:printcolumn:name="Message",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=="Ready")].message`

// CloudAccount is a verified credential for a RavenDB Cloud account, plus the
// defaults its clusters inherit. Every CloudCluster references exactly one.
//
// Deleting a CloudAccount waits while any CloudCluster in the namespace still
// references it, and the Ready condition, with reason InUse, names them.
// Without the credential those clusters could not be reconciled, and one with
// deletionPolicy Terminate could not terminate its cluster either, so its own
// deletion would never finish. Delete the clusters first.
type CloudAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudAccountSpec   `json:"spec,omitempty"`
	Status CloudAccountStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CloudAccountList contains a list of CloudAccount.
type CloudAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudAccount `json:"items"`
}

func init() {
	registerType(&CloudAccount{}, &CloudAccountList{})
}
