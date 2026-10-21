package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"net/http/httptest"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
)

// The controller tests run against controller-runtime's fake client and the
// cloud simulator rather than envtest, so they need no etcd or apiserver
// binaries and run anywhere, including in a plain golang container.
//
// The cloud side is the real REST client talking real HTTP to an in-process
// cloudsim, not a stub implementation of cloudapi.Client. That is a deliberate
// trade: these tests are a little slower and cannot force an exact Go error
// value, and in exchange every one of them also covers the request the operator
// actually builds, the JSON it sends, and how it classifies the status it gets
// back. A wire-mapping bug now fails the controller tests too, instead of
// waiting for a lab.

const (
	testNamespace   = "apps"
	testAccountName = "ravendb-cloud"
	testSecretName  = "ravendb-cloud-api-key"
	// testAPIKey is what the simulator accepts and what the credential Secret
	// carries. They have to agree: the simulator answers 401 otherwise, and the
	// whole point of going through HTTP is that authentication is real here.
	testAPIKey = "secret-api-key"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding operator scheme: %v", err)
	}
	return scheme
}

// harness bundles everything a controller test needs to drive one reconcile.
type harness struct {
	t        *testing.T
	k8s      client.Client
	cloud    *cloudsim.Sim
	cluster  *CloudClusterReconciler
	account  *CloudAccountReconciler
	recorder *events.FakeRecorder
	restart  *CloudClusterRestartReconciler
	onUpdate *func(client.Object)
	onStatus *func(client.Object) error
}

// interceptUpdates calls fn for every Update the reconcilers send to a resource
// itself, which is what tells a metadata patch apart from an object rewrite.
// Status().Update goes through the subresource client and is not seen here.
func (h *harness) interceptUpdates(fn func(client.Object)) {
	*h.onUpdate = fn
}

// interceptStatusUpdates calls fn for every Status().Update, and writes nothing
// when fn returns an error. That is the only way to stage the race a real API
// server settles with optimistic concurrency: somebody else wrote the object
// while this pass was computing a status for it.
func (h *harness) interceptStatusUpdates(fn func(client.Object) error) {
	*h.onStatus = fn
}

// newHarness wires the reconcilers against fakes. Objects are seeded through
// WithObjects so their status survives, which is how a Ready CloudAccount can
// exist before any reconcile runs.
func newHarness(t *testing.T, objects ...client.Object) *harness {
	t.Helper()
	scheme := testScheme(t)

	// onUpdate is set by interceptUpdates, after the client is built, which is
	// why it is a pointer the interceptor closes over rather than an argument.
	onUpdate := new(func(client.Object))
	onStatus := new(func(client.Object) error)

	k8s := k8sfake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if *onUpdate != nil {
					(*onUpdate)(obj)
				}
				return c.Update(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				// The real client refuses a context that has ended, and a
				// test of the pass deadline proves nothing unless this does too.
				if err := ctx.Err(); err != nil {
					return err
				}
				if *onStatus != nil {
					if err := (*onStatus)(obj); err != nil {
						return err
					}
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		}).
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.CloudCluster{}, &v1alpha1.CloudAccount{}, &v1alpha1.CloudClusterRestart{}).
		// The same index SetupWithManager registers, and for the same reason:
		// without it a lookup through it returns nothing instead of failing, so
		// a test of the Secret-to-account mapping would pass while proving
		// nothing.
		WithIndex(&v1alpha1.CloudAccount{}, secretRefIndex, indexAccountsBySecretRef).
		WithIndex(&v1alpha1.CloudCluster{}, accountRefIndex, indexClustersByAccountRef).
		WithIndex(&v1alpha1.CloudClusterRestart{}, clusterRefIndex, indexRestartsByClusterRef).
		WithIndex(&v1alpha1.CloudCluster{}, productIDIndex, indexClustersByProductID).
		Build()

	// ProvisionDuration and ChangeDuration are zero, so a created cluster is
	// Active by the next read. The requeue-through-Creating path has its own
	// tests that set them.
	cloud := cloudsim.New(cloudsim.Options{
		APIKey: testAPIKey,
		Domain: "example.ravendb.cloud",
	})
	cloudServer := httptest.NewServer(cloud.Handler())
	t.Cleanup(cloudServer.Close)

	// The operator-wide endpoint, which is what main sets from the
	// environment, so the tests point the client the way main does.
	clients := &cloudclients.Cache{
		Kube:     k8s,
		Endpoint: cloudServer.URL,
		Pacing: cloudclients.Pacing{
			// Production paces at one call per 1.1s, so a test that waited
			// would take minutes.
			RateLimit:      rate.Inf,
			RetryBaseDelay: time.Millisecond,
			// FailNext arms exactly one failure, so a retry would smooth it
			// over and make call counts meaningless.
			MaxRetries: -1,
		},
	}
	recorder := events.NewFakeRecorder(100)

	return &harness{
		t:        t,
		k8s:      k8s,
		cloud:    cloud,
		onUpdate: onUpdate,
		onStatus: onStatus,
		cluster: &CloudClusterReconciler{
			Client:   k8s,
			Scheme:   scheme,
			Clients:  clients,
			Recorder: recorder,
		},
		restart: &CloudClusterRestartReconciler{
			Client:   k8s,
			Scheme:   scheme,
			Clients:  clients,
			Recorder: recorder,
		},
		account: &CloudAccountReconciler{
			Client:   k8s,
			Clients:  clients,
			Recorder: recorder,
		},
		recorder: recorder,
	}
}

// reconcileCluster runs the cluster reconciler once.
func (h *harness) reconcileCluster(name string) reconcile.Result {
	h.t.Helper()
	result, err := h.cluster.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	if err != nil {
		h.t.Fatalf("reconcile %s: %v", name, err)
	}
	return result
}

// reconcileClusterUntilSettled drives reconciles until the cluster has been
// Running for two passes in a row, which is how the multi-pass create flow is
// exercised end to end.
//
// Between passes it steps the simulated clock by the requeue the controller
// asked for. That is what lets a cluster be given a realistic provisioning
// window and still settle in microseconds: the operator's own RequeueAfter
// decides how far time moves, so a test never sleeps and never guesses.
func (h *harness) reconcileClusterUntilSettled(name string, maxPasses int) *v1alpha1.CloudCluster {
	h.t.Helper()
	var wasRunning bool
	for i := 0; i < maxPasses; i++ {
		result := h.reconcileCluster(name)
		cluster := h.getCluster(name)
		if wasRunning && cluster.IsRunning() {
			return cluster
		}
		wasRunning = cluster.IsRunning()
		if result.RequeueAfter > 0 {
			h.cloud.State().Advance(result.RequeueAfter)
		}
	}
	return h.getCluster(name)
}

// hasEvent reports whether the reconciler recorded an event with this reason.
// Draining is fine: no test asserts on events twice.
func (h *harness) hasEvent(reason string) bool {
	h.t.Helper()
	for {
		select {
		case e := <-h.recorder.Events:
			if strings.Contains(e, reason) {
				return true
			}
		default:
			return false
		}
	}
}

func (h *harness) getCluster(name string) *v1alpha1.CloudCluster {
	h.t.Helper()
	var cluster v1alpha1.CloudCluster
	if err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &cluster); err != nil {
		h.t.Fatalf("getting CloudCluster %s: %v", name, err)
	}
	return &cluster
}

func (h *harness) getSecret(name string) *corev1.Secret {
	h.t.Helper()
	var secret corev1.Secret
	if err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &secret); err != nil {
		h.t.Fatalf("getting Secret %s: %v", name, err)
	}
	return &secret
}

// --- object builders -------------------------------------------------------

func credentialSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"apiKey": []byte(testAPIKey)},
	}
}

// readyAccount is a CloudAccount that has already been verified, with a cached
// lists, which is the state cluster reconciles expect to find.
func readyAccount() *v1alpha1.CloudAccount {
	return &v1alpha1.CloudAccount{
		ObjectMeta: metav1.ObjectMeta{Name: testAccountName, Namespace: testNamespace},
		Spec: v1alpha1.CloudAccountSpec{
			APIKeySecretRef: v1alpha1.SecretKeySelector{Name: testSecretName, Key: "apiKey"},
			Defaults: &v1alpha1.AccountDefaults{
				Provider:   v1alpha1.ProviderAWS,
				Region:     "eu-central-1",
				AllowedIPs: []string{"0.0.0.0/0"},
			},
		},
		Status: v1alpha1.CloudAccountStatus{
			DomainName:            "acme.ravendb.cloud",
			DefaultReleaseChannel: "Stable72",
			Providers: []v1alpha1.ProviderRegions{
				{Provider: v1alpha1.ProviderAWS, Regions: []string{"eu-central-1", "us-east-1"}},
			},
			Conditions: []metav1.Condition{{
				Type:               v1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             v1alpha1.ReasonVerified,
				Message:            "ok",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func provisionCluster(name string, mutate ...func(*v1alpha1.CloudCluster)) *v1alpha1.CloudCluster {
	cluster := &v1alpha1.CloudCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.CloudClusterSpec{
			AccountRef:   v1alpha1.LocalObjectReference{Name: testAccountName},
			Tier:         v1alpha1.TierProduction,
			InstanceType: "PB10",
			// 20 GB is the only size on both PB10's menu {10,20,30,50,75} and
			// PB20's {20,40,80,100,150}, and tests override the instance type
			// in both directions. A size valid for only one of them fails the
			// create rather than the assertion, which is a confusing way to find
			// out.
			Storage:    v1alpha1.StorageSpec{SizeGB: 20, Type: v1alpha1.StorageSsdStandard},
			Networking: v1alpha1.NetworkingSpec{AllowedIPs: []string{"0.0.0.0/0"}},
		},
	}
	for _, m := range mutate {
		m(cluster)
	}
	return cluster
}

// takeOverCluster is the smallest manifest that names a cluster already
// running: a name and the tier it has, which is Production for a seeded one.
func takeOverCluster(name string, mutate ...func(*v1alpha1.CloudCluster)) *v1alpha1.CloudCluster {
	cluster := &v1alpha1.CloudCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.CloudClusterSpec{
			AccountRef: v1alpha1.LocalObjectReference{Name: testAccountName},
			Tier:       v1alpha1.TierProduction,
		},
	}
	for _, m := range mutate {
		m(cluster)
	}
	return cluster
}

// seedCloudCluster puts an Active cluster in the simulated cloud, standing in
// for one a human created in the portal.
func (h *harness) seedCloudCluster(spec cloudsim.SeedSpec) *cloudsim.Cluster {
	h.t.Helper()
	return h.cloud.State().Seed(spec)
}

// cloudCluster reads one cluster out of the simulated cloud.
func (h *harness) cloudCluster(id string) (*cloudsim.Cluster, bool) {
	h.t.Helper()
	return h.cloud.State().Get(id)
}

// lastRequestTo decodes the body of the most recent request the operator made
// for one operation.
//
// Tests read the wire payload rather than a recorded Go argument because that
// is the thing the control plane will actually be handed. v is normally an
// anonymous struct naming only the fields under test.
func (h *harness) lastRequestTo(operation string, v any) {
	h.t.Helper()
	calls := h.cloud.Calls(operation)
	if len(calls) == 0 {
		h.t.Fatalf("no %s request was made", operation)
	}
	if err := json.Unmarshal(calls[len(calls)-1].Body, v); err != nil {
		h.t.Fatalf("decoding the %s request body %q: %v", operation, calls[len(calls)-1].Body, err)
	}
}

func conditionReason(conditions []metav1.Condition, condType string) string {
	if cond := meta.FindStatusCondition(conditions, condType); cond != nil {
		return cond.Reason
	}
	return ""
}
