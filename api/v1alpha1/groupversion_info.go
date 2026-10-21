// Package v1alpha1 contains the API schema for the RavenDB Cloud operator.
//
// The group is cloud.ravendb.net, deliberately separate from the self-hosted
// RavenDB operator, so both can be installed in one cluster without either kind
// shadowing the other in kubectl.
//
// +kubebuilder:object:generate=true
// +groupName=cloud.ravendb.net
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "cloud.ravendb.net", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	//
	// This is apimachinery's builder rather than controller-runtime's, so that
	// importing these types to build a client does not drag in
	// controller-runtime. Each types file registers itself through registerType
	// in its init.
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// registerType adds a kind and its list kind to the group-version.
func registerType(objects ...runtime.Object) {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, objects...)
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
