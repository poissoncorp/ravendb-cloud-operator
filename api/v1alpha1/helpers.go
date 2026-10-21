package v1alpha1

import (
	"cmp"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types. Each answers one question, and its reason is the one word
// kubectl get shows for it.
const (
	// ConditionReady answers "can I use it?" and is the STATUS column of a
	// CloudCluster and a CloudAccount.
	//
	// On a CloudCluster it is True exactly when the reason is Running: the
	// cluster is Active and its connection details are published. Unknown
	// means the operator could not look, because RavenDB Cloud did not
	// answer, rejected the account's API key, or answered in a way the
	// operator cannot read: the cluster is probably still serving, and status
	// shows what was last seen at lastSyncTime.
	ConditionReady = "Ready"

	// ConditionSynced answers "did my manifest reach RavenDB Cloud?" and is the
	// CHANGES column of a CloudCluster. It is set once the object is bound to a
	// cluster.
	ConditionSynced = "Synced"

	// ConditionReconciling and ConditionStalled are the two conditions kstatus
	// reads, and through it Flux, Argo CD health checks and kubectl wait. Both
	// are present only while True, carrying the reason and message of the
	// condition they summarize. Reconciling: the operator is still working
	// towards the spec. Stalled: it is stuck until a person changes something.
	ConditionReconciling = "Reconciling"
	ConditionStalled     = "Stalled"
)

// Reasons a CloudCluster's Ready condition gives, which is its STATUS column.
// These are part of the user-facing contract, so they are stable strings.
const (
	ReasonRunning               = "Running"
	ReasonCreating              = "Creating"
	ReasonUpdating              = "Updating"
	ReasonWaitingForCertificate = "WaitingForCertificate"
	ReasonWaitingForAccount     = "WaitingForAccount"
	ReasonInvalidSpec           = "InvalidSpec"
	ReasonRefused               = "Refused"
	ReasonQuotaReached          = "QuotaReached"
	ReasonNameTaken             = "NameTaken"
	ReasonClusterNotFound       = "ClusterNotFound"
	ReasonDeletedInCloud        = "DeletedInCloud"
	ReasonAwaitingPayment       = "AwaitingPayment"
	ReasonCreationFailed        = "CreationFailed"
	ReasonSecretNotWritten      = "SecretNotWritten"
	ReasonDeleting              = "Deleting"
	ReasonCloudUnreachable      = "CloudUnreachable"
	ReasonKeyRejected           = "KeyRejected"
	ReasonUnexpectedResponse    = "UnexpectedResponse"
)

// Reasons a CloudCluster's Synced condition gives, which is its CHANGES
// column. Refused is shared with Ready: a refused create and a refused change
// are the same answer from RavenDB Cloud.
const (
	ReasonApplied    = "Applied"
	ReasonApplying   = "Applying"
	ReasonWaiting    = "Waiting"
	ReasonNotApplied = "NotApplied"
)

// Reasons a CloudAccount's Ready condition gives, besides KeyRejected,
// CloudUnreachable and UnexpectedResponse, which mean the same as on a
// CloudCluster.
const (
	ReasonVerified   = "Verified"
	ReasonKeyMissing = "KeyMissing"
	ReasonInUse      = "InUse"
)

// Event reasons for things the operator did. A Warning about a problem uses
// the condition reason it is about instead.
const (
	EventCreated         = "Created"
	EventAdopted         = "Adopted"
	EventApplying        = "Applying"
	EventSecretPublished = "SecretPublished"
	EventRetained        = "Retained"
	EventTerminating     = "Terminating"
	EventRestarting      = "Restarting"
	EventRestarted       = "Restarted"
	EventOpenAllowList   = "OpenAllowList"
)

// ConditionReason is the named condition's reason, or "" when it is not set.
// It is the word kubectl get shows for that condition.
func ConditionReason(conditions []metav1.Condition, condType string) string {
	if cond := meta.FindStatusCondition(conditions, condType); cond != nil {
		return cond.Reason
	}
	return ""
}

// EffectiveSecretKey returns the Secret key holding the API key.
func (a *CloudAccount) EffectiveSecretKey() string {
	if a.Spec.APIKeySecretRef.Key != "" {
		return a.Spec.APIKeySecretRef.Key
	}
	return "apiKey"
}

// EffectiveRefreshInterval returns how often to re-read the region and release
// channel lists.
func (a *CloudAccount) EffectiveRefreshInterval() time.Duration {
	if a.Spec.RefreshInterval != nil && a.Spec.RefreshInterval.Duration > 0 {
		return a.Spec.RefreshInterval.Duration
	}
	return time.Hour
}

// EffectiveManagementPolicy resolves the policy: what the spec says, and
// otherwise Observe for a cluster this object found and Full for one it
// created, or is about to.
func (c *CloudCluster) EffectiveManagementPolicy() ManagementPolicy {
	if c.Spec.ManagementPolicy != "" {
		return c.Spec.ManagementPolicy
	}
	if c.Status.Adopted {
		return ManagementObserve
	}
	return ManagementFull
}

// MayMutate reports whether the operator is allowed to change the cloud
// cluster.
func (c *CloudCluster) MayMutate() bool {
	return c.EffectiveManagementPolicy() == ManagementFull
}

// EffectiveDisplayName is the name the cluster has, or will have, in RavenDB
// Cloud, and the name the operator finds it by.
func (c *CloudCluster) EffectiveDisplayName() string {
	if c.Spec.DisplayName != "" {
		return c.Spec.DisplayName
	}
	return c.Name
}

// IsRunning reports whether the cluster is Active with its connection details
// published, which is what Ready True means.
func (c *CloudCluster) IsRunning() bool {
	return meta.IsStatusConditionTrue(c.Status.Conditions, ConditionReady)
}

// ConnectionSecretEnabled reports whether a connection Secret should be written.
func (c *CloudCluster) ConnectionSecretEnabled() bool {
	if c.Spec.ConnectionSecret == nil || c.Spec.ConnectionSecret.Enabled == nil {
		return true
	}
	return *c.Spec.ConnectionSecret.Enabled
}

// ConnectionSecretIncludesCertificate reports whether the client certificate
// should be embedded in the connection Secret.
func (c *CloudCluster) ConnectionSecretIncludesCertificate() bool {
	if c.Spec.ConnectionSecret == nil || c.Spec.ConnectionSecret.IncludeCertificate == nil {
		return true
	}
	return *c.Spec.ConnectionSecret.IncludeCertificate
}

// ConnectionSecretName is the Secret to write, defaulted from the object name.
func (c *CloudCluster) ConnectionSecretName() string {
	if c.Spec.ConnectionSecret != nil && c.Spec.ConnectionSecret.Name != "" {
		return c.Spec.ConnectionSecret.Name
	}
	return c.Name + "-connection"
}

// EffectivePollInterval is how often to re-read a settled cluster.
func (c *CloudCluster) EffectivePollInterval() time.Duration {
	if c.Spec.PollInterval != nil && c.Spec.PollInterval.Duration > 0 {
		return c.Spec.PollInterval.Duration
	}
	return 5 * time.Minute
}

// EffectiveStorageType is the disk class to ask for when the operator creates
// a cluster, given the provider it is going to. Drift never reads it: on an
// existing cluster an unset type is left as the disk has it.
//
// Unlike the other Effective* accessors, this has no +kubebuilder:default
// behind it, deliberately. The right value depends on the provider, which the
// API server cannot see when defaulting one field, and RavenDB Cloud refuses
// SsdStandard on GCP. An explicit SsdStandard on GCP is refused at admission,
// so this only ever answers the unset case.
func (s *StorageSpec) EffectiveStorageType(provider Provider) StorageType {
	if s != nil && s.Type != "" {
		return s.Type
	}
	if provider == ProviderGCP {
		return StorageSsdPremium
	}
	return StorageSsdStandard
}

// EffectiveTransitionalInterval is how soon to look again while RavenDB Cloud
// is mid-operation.
//
// Thirty seconds by default, because creating takes minutes and a tighter loop
// would make calls for nothing. But a shorter spec.pollInterval wins: someone
// who writes 5s is asking to be told sooner, and the operator's pacing, not
// this constant, is what keeps the calls in check.
func (c *CloudCluster) EffectiveTransitionalInterval() time.Duration {
	if poll := c.EffectivePollInterval(); poll < defaultTransitionalInterval {
		return poll
	}
	return defaultTransitionalInterval
}

const defaultTransitionalInterval = 30 * time.Second

// EffectiveDeletionPolicy defaults to Retain, so deleting the Kubernetes object
// never destroys a database by accident.
// EffectiveCreationMode backstops +kubebuilder:default=TakeOverOrCreate for an
// object that never passed admission.
func (c *CloudCluster) EffectiveCreationMode() CreationMode {
	return cmp.Or(c.Spec.CreationMode, CreationTakeOverOrCreate)
}

func (c *CloudCluster) EffectiveDeletionPolicy() DeletionPolicy {
	if c.Spec.DeletionPolicy != "" {
		return c.Spec.DeletionPolicy
	}
	return DeletionRetain
}
