package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// TestAddToSchemeRegistersEveryKind pins what registerType has to keep doing.
//
// Registration moved off controller-runtime's scheme.Builder so that importing
// these types does not pull controller-runtime in with them. That builder
// paired AddKnownTypes with AddToGroupVersion on every Register call, and
// dropping either half fails here rather than as an unrecognised-kind error in
// whichever controller happens to start first.
func TestAddToSchemeRegistersEveryKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{"CloudAccount", "CloudAccountList", "CloudCluster", "CloudClusterList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); err != nil {
			t.Errorf("kind %s is not registered: %v", kind, err)
		}
	}

	// AddToGroupVersion is what registers ListOptions and friends against the
	// group, which a client needs before it can list or watch.
	if !scheme.IsVersionRegistered(GroupVersion) {
		t.Errorf("group version %s is not registered", GroupVersion)
	}
}
