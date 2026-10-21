package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionComplete and ConditionFailed are a CloudClusterRestart's outcome,
// as on a Job. Complete is True once the node came back, and False while the
// restart waits, runs, or after it failed; its reason is the restart's STATUS
// column. Failed is True once the restart failed, and absent otherwise, so
// kubectl wait --for=condition=Complete never reports a failed restart as done.
const (
	ConditionComplete = "Complete"
	ConditionFailed   = "Failed"
)

// Reasons a CloudClusterRestart's Complete condition gives, besides Waiting,
// which means what it does on Synced: nothing is wrong, the operator is
// waiting for a moment it can act.
const (
	ReasonRestarting = "Restarting"
	ReasonSucceeded  = "Succeeded"
	ReasonFailed     = "Failed"
)

// CloudClusterRestartSpec names one node to restart, once.
//
// The spec cannot be changed after the object is created. To restart again,
// or to restart another node, create another CloudClusterRestart; each one is
// the record of a single restart.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a CloudClusterRestart is immutable: create another one to restart again"
type CloudClusterRestartSpec struct {
	// ClusterRef selects the CloudCluster in this namespace to act on.
	//
	// On Kubernetes 1.31 or newer, this lists the restarts of one cluster:
	//
	//   kubectl get cloudclusterrestarts --field-selector spec.clusterRef.name=<cluster>
	ClusterRef LocalObjectReference `json:"clusterRef"`

	// NodeTag is the RavenDB cluster node to restart, for example A: one of
	// the tags in the CloudCluster's status.nodeTags, which are single upper
	// case letters. A tag the cluster does not have fails the restart.
	// +kubebuilder:validation:Pattern=`^[A-Z]$`
	NodeTag string `json:"nodeTag"`
}

// CloudClusterRestartStatus is how far the restart got.
type CloudClusterRestartStatus struct {
	// Conditions hold Complete, whose reason is the STATUS column, Failed once
	// the restart failed, as on a Job, and Reconciling or Stalled for kstatus.
	// Complete is True only for Succeeded:
	//
	//   Waiting     not asked yet: the cluster is not Active, another restart of
	//               it is under way, it is not reachable, or its policy does
	//               not allow a restart; the message says which
	//   Restarting  RavenDB Cloud was asked and the node is coming back
	//   Succeeded   the node came back and the cluster is Active again
	//   Failed      it did not happen, or the cluster did not come back; the
	//               message says why. A restart runs once, so create a new
	//               CloudClusterRestart to try again
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// IssuedAt is when the operator asked RavenDB Cloud to restart the node.
	// It is set just before the request, so a node is restarted at most once,
	// even if the operator stops before it records the result.
	// +optional
	IssuedAt *metav1.Time `json:"issuedAt,omitempty"`

	// CompletedAt is when the cluster returned to Active afterwards.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// A separate kind rather than a CloudCluster field, because "restart node A"
// is not a state: nothing about the cluster afterwards is different, so no
// field value could mean "restarted", and a field would re-fire on every
// resync. An object that runs once and then holds its own result is the shape
// a Job already has.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rcr;rcrestart,categories=ravendb
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeTag`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Complete")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Message",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=="Complete")].message`
// Restarts are usually listed for one cluster. Needs Kubernetes 1.31 or newer.
// +kubebuilder:selectablefield:JSONPath=`.spec.clusterRef.name`

// CloudClusterRestart restarts one node of a CloudCluster. Like a Job, it runs
// once and then holds its own result.
//
// It obeys the cluster it points at. A CloudCluster whose
// managementPolicy is Observe, holds the restart back and says so rather than
// reaching into a cluster the operator was told not to touch. It runs as soon
// as that changes.
//
// Once it starts, the operator makes the CloudCluster its owner, so deleting
// the CloudCluster takes its restart records with it. Finished ones are
// otherwise yours to delete, the way a Job without ttlSecondsAfterFinished is.
//
// Create it with kubectl create, not from a repository a GitOps tool syncs. It
// remembers that it ran only in its status, so one recreated without it
// restarts the node again.
type CloudClusterRestart struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudClusterRestartSpec   `json:"spec,omitempty"`
	Status CloudClusterRestartStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CloudClusterRestartList contains a list of CloudClusterRestart.
type CloudClusterRestartList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudClusterRestart `json:"items"`
}

// IsFinished reports whether this restart is done and must never act again.
func (r *CloudClusterRestart) IsFinished() bool {
	return meta.IsStatusConditionTrue(r.Status.Conditions, ConditionComplete) || meta.IsStatusConditionTrue(r.Status.Conditions, ConditionFailed)
}

func init() {
	registerType(&CloudClusterRestart{}, &CloudClusterRestartList{})
}
