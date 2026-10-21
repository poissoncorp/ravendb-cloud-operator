package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
	"github.com/ravendb/ravendb-cloud-operator/internal/export"
)

// --- Creating a cluster ----------------------------------------------------

func TestProvisionCreatesClusterAndPublishesConnectionSecret(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	// A real provisioning window, so the Creating state and the requeue that
	// polls through it are both exercised. reconcileClusterUntilSettled steps
	// the simulated clock by whatever the controller asks to be requeued
	// after, so this costs no wall-clock time.
	h.cloud.State().SetProvisionDuration(30 * time.Second)

	cluster := h.reconcileClusterUntilSettled("orders", 6)

	if !cluster.IsRunning() {
		t.Fatalf("Ready = %q, want Running (conditions: %+v)",
			conditionReason(cluster.Status.Conditions, v1alpha1.ConditionReady), cluster.Status.Conditions)
	}
	if h.cloud.CallCount("CreateCluster") != 1 {
		t.Errorf("CreateCluster called %d times, want exactly 1", h.cloud.CallCount("CreateCluster"))
	}
	if cluster.Status.ProductID == "" {
		t.Error("status.productID was not recorded")
	}
	if cluster.Status.Adopted {
		t.Error("a cluster this object created must not be marked adopted")
	}
	if cluster.Status.ManagementPolicy != v1alpha1.ManagementFull {
		t.Errorf("managementPolicy = %q, want Full by default for a created cluster", cluster.Status.ManagementPolicy)
	}
	// Running means published too, so the message has to say where.
	if cond := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady); !contains(cond.Message, "orders-connection") {
		t.Errorf("Ready message %q does not name the Secret", cond.Message)
	}
	if got := conditionReason(cluster.Status.Conditions, v1alpha1.ConditionSynced); got != v1alpha1.ReasonApplied {
		t.Errorf("Synced = %q, want Applied", got)
	}
	if !h.hasEvent(v1alpha1.EventCreated) {
		t.Error("expected a Created event")
	}

	// The Secret is the deliverable: an application should be able to mount it.
	secret := h.getSecret("orders-connection")
	if len(secret.Data[secretKeyURL]) == 0 {
		t.Error("connection Secret has no url")
	}
	if len(secret.OwnerReferences) != 1 {
		t.Errorf("connection Secret has %d owner references, want 1 so it is garbage collected", len(secret.OwnerReferences))
	}
}

func TestProvisionDefaultsInstanceTypeFromTier(t *testing.T) {
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.InstanceType = "" // let the operator choose
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)

	settled := h.reconcileClusterUntilSettled("orders", 6)

	// The fake instance types offer PB10 (2 vCPU) and PB20 (4 vCPU) in Production.
	// Smallest-first means PB10.
	if settled.Status.InstanceType != "PB10" {
		t.Errorf("instanceType = %q, want the smallest Production shape PB10", settled.Status.InstanceType)
	}
}

func TestProvisionRejectsUnavailableRegionWithoutCallingCreate(t *testing.T) {
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.Region = "moon-base-1"
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)

	h.reconcileCluster("orders") // adds the finalizer
	h.reconcileCluster("orders")

	settled := h.getCluster("orders")
	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("a region that is not in the cached list must be rejected before calling the cloud API")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("Ready condition = %+v, want reason InvalidSpec", cond)
	}
	// The message should tell the user what they could have written instead.
	if !contains(cond.Message, "eu-central-1") {
		t.Errorf("message %q does not list the available regions", cond.Message)
	}
	// And how to get there: spec.region cannot be edited once set, so the edit
	// the message would otherwise invite is one the API server refuses.
	if !contains(cond.Message, "spec.region") {
		t.Errorf("message %q does not name spec.region as the field that cannot change", cond.Message)
	}
}

// TestProvisionRecoversFromCrashBetweenCreateAndStatusWrite covers the window
// where the operator has already called create but has not recorded the id.
func TestProvisionRecoversFromCrashBetweenCreateAndStatusWrite(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	// The cluster exists in the cloud and the marker proves this object asked
	// for it, but the id never made it into status.
	created := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})

	cluster := h.getCluster("orders")
	attempted := metav1.Now()
	cluster.Status.ProvisionAttemptedAt = &attempted
	if err := h.k8s.Status().Update(context.Background(), cluster); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	settled := h.reconcileClusterUntilSettled("orders", 6)

	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("recovery must re-bind to the in-flight cluster, not create a second one")
	}
	if settled.Status.ProductID != created.ID {
		t.Errorf("productID = %q, want the recovered %q", settled.Status.ProductID, created.ID)
	}
	// This object made it, so it is managed as one it made: Full, not the
	// Observe a found cluster starts in.
	if settled.Status.Adopted || settled.Status.ManagementPolicy != v1alpha1.ManagementFull {
		t.Errorf("adopted = %v, policy = %q; a recovered create is this object's own and runs Full",
			settled.Status.Adopted, settled.Status.ManagementPolicy)
	}
}

// --- Taking a cluster over -------------------------------------------------

// TestAManifestTakesOverTheClusterItsNameFinds is the Atlas model: the display
// name identifies the cluster, so a manifest whose name is already running in
// the account binds to it instead of asking RavenDB Cloud for a second one,
// which it would refuse anyway (display names are unique per account).
func TestAManifestTakesOverTheClusterItsNameFinds(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("legacy-prod"))
	existing := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "legacy-prod"})

	settled := h.reconcileClusterUntilSettled("legacy-prod", 5)

	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Fatal("a cluster that already runs must be found, not created")
	}
	if settled.Status.ProductID != existing.ID {
		t.Errorf("productID = %q, want %q", settled.Status.ProductID, existing.ID)
	}
	if !settled.Status.Adopted {
		t.Error("status.adopted should be true for a cluster this object found")
	}
	if !settled.IsRunning() {
		t.Errorf("Ready = %q, want Running", conditionReason(settled.Status.Conditions, v1alpha1.ConditionReady))
	}
	// A found cluster starts read-only, so taking it over reshapes nothing.
	if settled.Status.ManagementPolicy != v1alpha1.ManagementObserve {
		t.Errorf("managementPolicy = %q, want Observe by default for a found cluster", settled.Status.ManagementPolicy)
	}
	// A manifest that names only the tier asks for nothing that could differ.
	if got := conditionReason(settled.Status.Conditions, v1alpha1.ConditionSynced); got != v1alpha1.ReasonApplied {
		t.Errorf("Synced = %q, want Applied: the manifest sets nothing the cluster does not have", got)
	}
	if !h.hasEvent(v1alpha1.EventAdopted) {
		t.Error("expected an Adopted event saying the cluster was found")
	}
	// Connectivity is what taking a cluster over delivers first.
	secret := h.getSecret("legacy-prod-connection")
	if len(secret.Data[secretKeyTLSCert]) == 0 && len(secret.Data[secretKeyRawBundle]) == 0 {
		t.Error("the found cluster published no certificate material")
	}
}

// TestAnExportedManifestTakesOverWithoutChangingAnything is what the export
// tool is for: a manifest written from the running cluster matches it, so it
// can say managementPolicy Full and still change nothing on the first apply.
func TestAnExportedManifestTakesOverWithoutChangingAnything(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount())
	seeded := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "legacy-prod", InstanceType: "PB20"})

	// The same endpoint the harness gives the operator, so this reads the
	// same cloud.
	cloud, err := cloudapi.NewRESTClient(cloudapi.Options{
		Endpoint: h.cluster.Clients.Endpoint, APIKey: testAPIKey, RateLimit: rate.Inf, MaxRetries: -1,
	})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	out, err := export.Named(context.Background(), cloud,
		export.Target{Account: testAccountName, Namespace: testNamespace}, []string{"legacy-prod"})
	if err != nil {
		t.Fatalf("exporting: %v", err)
	}
	var cluster v1alpha1.CloudCluster
	if err := yaml.Unmarshal(out, &cluster); err != nil {
		t.Fatalf("the export does not parse as a CloudCluster: %v\n%s", err, out)
	}
	if err := h.k8s.Create(context.Background(), &cluster); err != nil {
		t.Fatalf("creating the exported CloudCluster: %v", err)
	}

	settled := h.reconcileClusterUntilSettled(cluster.Name, 5)

	if settled.Status.ProductID != seeded.ID {
		t.Fatalf("productID = %q, want the exported cluster %q", settled.Status.ProductID, seeded.ID)
	}
	if settled.Status.ManagementPolicy != v1alpha1.ManagementFull {
		t.Errorf("managementPolicy = %q, want the Full the export wrote", settled.Status.ManagementPolicy)
	}
	if got := conditionReason(settled.Status.Conditions, v1alpha1.ConditionSynced); got != v1alpha1.ReasonApplied {
		t.Errorf("Synced = %q, want Applied: the export describes exactly what runs", got)
	}
	for _, change := range []string{"CreateCluster", "ChangeInstanceType", "ChangeStorage", "AddNode", "RemoveNode"} {
		if n := h.cloud.CallCount(change); n != 0 {
			t.Errorf("%s called %d times taking over an exported cluster, want 0", change, n)
		}
	}
}

// TestAFoundClusterIsNotReshapedUntilThePolicySaysSo is the safety rail that
// stands in for Atlas's export step. A hand-written manifest that gets a field
// wrong reports the difference first, and only Full carries it through.
func TestAFoundClusterIsNotReshapedUntilThePolicySaysSo(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("legacy-prod", func(c *v1alpha1.CloudCluster) {
		c.Spec.InstanceType = "PB20" // the seeded cluster is PB10
	}))
	h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "legacy-prod"})

	settled := h.reconcileClusterUntilSettled("legacy-prod", 5)

	if h.cloud.CallCount("ChangeInstanceType") != 0 {
		t.Fatal("a found cluster must not be reshaped while the policy is the default Observe")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonNotApplied {
		t.Fatalf("Synced = %+v, want NotApplied", cond)
	}
	if !contains(cond.Message, "instanceType differs") || !contains(cond.Message, "set spec.managementPolicy to Full") {
		t.Errorf("message %q should name the difference and the way to apply it", cond.Message)
	}

	settled.Spec.ManagementPolicy = v1alpha1.ManagementFull
	if err := h.k8s.Update(context.Background(), settled); err != nil {
		t.Fatalf("updating spec: %v", err)
	}
	h.reconcileCluster("legacy-prod")

	if h.cloud.CallCount("ChangeInstanceType") != 1 {
		t.Errorf("ChangeInstanceType called %d times after Full, want 1", h.cloud.CallCount("ChangeInstanceType"))
	}
}

// TestANameOnAnotherKindOfClusterIsNotTakenOver is what keeps a new
// Development manifest from binding to a Production cluster that happens to
// share its name. Tier cannot change, so a different tier means the name is
// somebody else's cluster, not this manifest's.
func TestANameOnAnotherKindOfClusterIsNotTakenOver(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.Tier = v1alpha1.TierDevelopment
	}))
	h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"}) // Production

	h.reconcileCluster("orders")
	h.reconcileCluster("orders")

	settled := h.getCluster("orders")
	if settled.Status.ProductID != "" {
		t.Errorf("bound to %q, a cluster of another tier", settled.Status.ProductID)
	}
	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("the name is taken, so asking to create would only be refused")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonNameTaken {
		t.Fatalf("Ready = %+v, want NameTaken", cond)
	}
	if !contains(cond.Message, "Production") || !contains(cond.Message, "displayName") {
		t.Errorf("message %q should say what the cluster is and which field to change", cond.Message)
	}
}

// TestTwoObjectsCannotManageOneCluster: with the name as the identity, a
// second manifest for the same name would otherwise bind to the same cluster,
// and two objects applying two specs to one cluster fight forever.
func TestTwoObjectsCannotManageOneCluster(t *testing.T) {
	second := takeOverCluster("orders-copy", func(c *v1alpha1.CloudCluster) {
		c.Spec.DisplayName = "orders"
	})
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders"), second)
	existing := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})

	h.reconcileClusterUntilSettled("orders", 5)
	h.reconcileCluster("orders-copy")
	h.reconcileCluster("orders-copy")

	if got := h.getCluster("orders").Status.ProductID; got != existing.ID {
		t.Fatalf("the first object did not bind: productID = %q", got)
	}
	copied := h.getCluster("orders-copy")
	if copied.Status.ProductID != "" {
		t.Errorf("the second object bound to %q as well", copied.Status.ProductID)
	}
	cond := meta.FindStatusCondition(copied.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonNameTaken || !contains(cond.Message, testNamespace+"/orders") {
		t.Errorf("Ready = %+v, want NameTaken naming the object that holds the cluster", cond)
	}
}

func TestAnAmbiguousNameBindsToNothing(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders"))
	// Seeding skips the name check a create makes, which is how two clusters
	// that raced each other's create look.
	h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})
	h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders", SubdomainName: "orders2"})

	h.reconcileCluster("orders")
	h.reconcileCluster("orders")

	settled := h.getCluster("orders")
	if settled.Status.ProductID != "" {
		t.Errorf("bound to %q despite an ambiguous name", settled.Status.ProductID)
	}
	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("an ambiguous name must not lead to a third cluster")
	}
	if got := conditionReason(settled.Status.Conditions, v1alpha1.ConditionReady); got != v1alpha1.ReasonNameTaken {
		t.Errorf("Ready reason = %q, want NameTaken", got)
	}
}

// TestObserveNeverCreatesACluster: Observe changes nothing in RavenDB Cloud,
// and creating is a change. A manifest written to consume a cluster that
// turns out to be misnamed must not order a new, billed one.
func TestObserveNeverCreatesACluster(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("nope", func(c *v1alpha1.CloudCluster) {
		c.Spec.ManagementPolicy = v1alpha1.ManagementObserve
	}))

	h.reconcileCluster("nope")
	h.reconcileCluster("nope")

	settled := h.getCluster("nope")
	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("Observe must never create a cluster")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonClusterNotFound {
		t.Fatalf("Ready = %+v, want ClusterNotFound", cond)
	}
	if !contains(cond.Message, "spec.managementPolicy") || !contains(cond.Message, "Full") {
		t.Errorf("message %q does not name the field and value that would create it", cond.Message)
	}
}

// TestAFreeClusterIsOnlyEverFound: the public API refuses to create a Free
// product, so a Free manifest waits for one made in the portal, and binds to
// it once it appears.
func TestAFreeClusterIsOnlyEverFound(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("sandbox", func(c *v1alpha1.CloudCluster) {
		c.Spec.Tier = v1alpha1.TierFree
	}))

	h.reconcileCluster("sandbox")
	h.reconcileCluster("sandbox")

	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Fatal("a Free cluster cannot be created through the API, so the operator must not ask")
	}
	cond := meta.FindStatusCondition(h.getCluster("sandbox").Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonClusterNotFound || !contains(cond.Message, "portal") {
		t.Fatalf("Ready = %+v, want ClusterNotFound pointing at the portal", cond)
	}

	free := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "sandbox", Tier: "Free", InstanceType: "Free"})
	settled := h.reconcileClusterUntilSettled("sandbox", 5)
	if settled.Status.ProductID != free.ID {
		t.Errorf("productID = %q, want the Free cluster %q made in the portal", settled.Status.ProductID, free.ID)
	}
}

// TestANameATerminatedClusterHoldsSaysHowToFreeIt: RavenDB Cloud keeps a
// terminated cluster's record, and counts its name as taken until somebody
// hides it in the portal. The lookup rightly skips terminated clusters, so the
// create goes out and is refused, and the refusal has to say why.
func TestANameATerminatedClusterHoldsSaysHowToFreeIt(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	old := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders", SubdomainName: "orders-old"})
	if err := h.cloud.State().Terminate(old.ID, false); err != nil {
		t.Fatalf("terminating the old cluster: %v", err)
	}
	h.cloud.State().Advance(time.Hour)

	h.reconcileCluster("orders")
	h.reconcileCluster("orders")

	cond := meta.FindStatusCondition(h.getCluster("orders").Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonRefused {
		t.Fatalf("Ready = %+v, want Refused", cond)
	}
	if !contains(cond.Message, "spec.displayName") || !contains(cond.Message, "hidden in the RavenDB Cloud portal") {
		t.Errorf("message %q should name the field and how the name is freed", cond.Message)
	}
}

// --- Day-2 and policies ----------------------------------------------------

func TestObservePolicyReportsDriftWithoutMutating(t *testing.T) {
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.ManagementPolicy = v1alpha1.ManagementObserve
		c.Spec.InstanceType = "PB20"
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)
	// Observe never creates, so the cluster has to exist already.
	h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders", InstanceType: "PB20"})
	h.reconcileClusterUntilSettled("orders", 6)

	// Force a divergence the way a portal edit would. Patch is the same lever
	// the labs pull over /_lab.
	settled := h.getCluster("orders")
	h.cloud.State().Patch(settled.Status.ProductID, "PB10", 0, nil, "")

	h.reconcileCluster("orders")
	settled = h.getCluster("orders")

	if h.cloud.CallCount("ChangeInstanceType") != 0 {
		t.Error("Observe policy must not mutate the cloud cluster")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonNotApplied {
		t.Fatalf("Synced condition = %+v, want reason NotApplied", cond)
	}
	if !contains(cond.Message, "instanceType differs") {
		t.Errorf("message %q does not name the difference", cond.Message)
	}
}

func TestFullPolicyResizesStorage(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	settled := h.getCluster("orders")
	settled.Spec.Storage.SizeGB = 75
	if err := h.k8s.Update(context.Background(), settled); err != nil {
		t.Fatalf("updating spec: %v", err)
	}

	h.reconcileCluster("orders")

	if h.cloud.CallCount("ChangeStorage") != 1 {
		t.Fatalf("ChangeStorage called %d times, want 1", h.cloud.CallCount("ChangeStorage"))
	}
	// Assert on the payload rather than on a recorded Go argument: diskSize is
	// the field name the control plane reads, and a rename on either side of
	// the wire mapping has to fail something.
	var sent struct {
		Size int32 `json:"size"`
	}
	h.lastRequestTo("ChangeStorage", &sent)
	if sent.Size != 75 {
		t.Errorf("resize to %d GiB, want 75", sent.Size)
	}
	settled = h.getCluster("orders")
	if got := conditionReason(settled.Status.Conditions, v1alpha1.ConditionReady); got != v1alpha1.ReasonUpdating {
		t.Errorf("Ready = %q on the pass that sent a change, want Updating", got)
	}
	if !h.hasEvent(v1alpha1.EventApplying) {
		t.Error("expected an Applying event for the change")
	}
}

func TestShrinkingStorageIsRefusedAsDrift(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	settled := h.getCluster("orders")
	settled.Spec.Storage.SizeGB = 10 // on PB10's menu, and below the 20 created
	if err := h.k8s.Update(context.Background(), settled); err != nil {
		t.Fatalf("updating spec: %v", err)
	}

	h.reconcileCluster("orders")
	settled = h.getCluster("orders")

	if h.cloud.CallCount("ChangeStorage") != 0 {
		t.Error("the operator must not attempt to shrink a cloud disk")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonNotApplied {
		t.Fatalf("Synced = %+v, want NotApplied", cond)
	}
	if !contains(cond.Message, "10 GB") || !contains(cond.Message, "20 GB") {
		t.Errorf("message %q does not give the size asked for and the size it has", cond.Message)
	}
}

// premiumDisk is a created cluster on a Premium disk with dedicated IOPS, the
// disk an unset storage.type must not be compared against a default.
func premiumDisk(c *v1alpha1.CloudCluster) {
	iops := int32(3000)
	c.Spec.Storage = v1alpha1.StorageSpec{SizeGB: 20, Type: v1alpha1.StorageSsdPremium, IOPS: &iops}
}

func TestAStorageFieldLeftOutIsNotCompared(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", premiumDisk))
	h.reconcileClusterUntilSettled("orders", 6)

	settled := h.getCluster("orders")
	settled.Spec.Storage = v1alpha1.StorageSpec{SizeGB: 20}
	if err := h.k8s.Update(context.Background(), settled); err != nil {
		t.Fatalf("updating spec: %v", err)
	}
	h.reconcileCluster("orders")

	if n := h.cloud.CallCount("ChangeStorage"); n != 0 {
		t.Errorf("ChangeStorage called %d times for a manifest that leaves the disk class out", n)
	}
	if got := conditionReason(h.getCluster("orders").Status.Conditions, v1alpha1.ConditionSynced); got != v1alpha1.ReasonApplied {
		t.Errorf("Synced = %q, want Applied", got)
	}
}

func TestAResizeKeepsTheDiskClassAndIOPSItLeavesOut(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", premiumDisk))
	h.reconcileClusterUntilSettled("orders", 6)

	settled := h.getCluster("orders")
	settled.Spec.Storage = v1alpha1.StorageSpec{SizeGB: 75}
	if err := h.k8s.Update(context.Background(), settled); err != nil {
		t.Fatalf("updating spec: %v", err)
	}
	h.reconcileCluster("orders")

	var sent struct {
		StorageType string `json:"storageType"`
		Size        int32  `json:"size"`
		IOPS        *int32 `json:"iops"`
	}
	h.lastRequestTo("ChangeStorage", &sent)
	if sent.Size != 75 || sent.StorageType != "SsdPremium" || sent.IOPS == nil || *sent.IOPS != 3000 {
		t.Errorf("sent size %d, type %q, iops %v; want 75, SsdPremium, 3000", sent.Size, sent.StorageType, int32PtrText(sent.IOPS))
	}
}

func TestStorageFieldsWithoutASizeAreStillApplied(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	settled := h.getCluster("orders")
	iops := int32(3000)
	settled.Spec.Storage = v1alpha1.StorageSpec{Type: v1alpha1.StorageSsdPremium, IOPS: &iops}
	if err := h.k8s.Update(context.Background(), settled); err != nil {
		t.Fatalf("updating spec: %v", err)
	}
	h.reconcileCluster("orders")

	var sent struct {
		StorageType string `json:"storageType"`
		Size        int32  `json:"size"`
	}
	h.lastRequestTo("ChangeStorage", &sent)
	if sent.Size != 20 || sent.StorageType != "SsdPremium" {
		t.Errorf("sent size %d, type %q; want the current 20 and SsdPremium", sent.Size, sent.StorageType)
	}
}

func TestScaleUpAddsOneNodePerPass(t *testing.T) {
	five := int32(5)
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		// Production, and it has to be. Additional nodes are a Production-only
		// feature and the control plane refuses them anywhere else, so a
		// Development-tier version of this test would be asserting against an
		// API that answers 400. PB10 starts at three nodes, which is why the
		// target here is five rather than three.
		c.Spec.InstanceType = "PB10"
		c.Spec.Tier = v1alpha1.TierProduction
		c.Spec.Nodes = &five
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)
	h.reconcileClusterUntilSettled("orders", 10)

	if got := h.cloud.CallCount("AddNode"); got != 2 {
		t.Errorf("AddNode called %d times, want 2 (3 nodes -> 5)", got)
	}
	settled := h.getCluster("orders")
	if settled.Status.Nodes != 5 {
		t.Errorf("status.nodes = %d, want 5", settled.Status.Nodes)
	}
}

// TestARegionAddedLaterIsReportedWhenItDiffers: the CRD lets a manifest add a
// region it first inherited, so that it can be pinned. Adding a different one
// cannot move the cluster, and has to say so rather than pass silently.
func TestARegionAddedLaterIsReportedWhenItDiffers(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	settled := h.getCluster("orders")
	settled.Spec.Region = "us-east-1" // created in the account default, eu-central-1
	if err := h.k8s.Update(context.Background(), settled); err != nil {
		t.Fatalf("updating spec: %v", err)
	}
	h.reconcileCluster("orders")

	cond := meta.FindStatusCondition(h.getCluster("orders").Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonNotApplied || !contains(cond.Message, "region differs") {
		t.Errorf("Synced = %+v, want NotApplied naming the region", cond)
	}
}

// --- Deletion --------------------------------------------------------------

func TestDeleteWithRetainPolicyLeavesCloudClusterRunning(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	settled := h.getCluster("orders")
	productID := settled.Status.ProductID

	if err := h.k8s.Delete(context.Background(), settled); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	h.reconcileCluster("orders")

	if h.cloud.CallCount("TerminateCluster") != 0 {
		t.Fatal("Retain is the default: deleting the object must not terminate the cluster")
	}
	if _, stillThere := h.cloudCluster(productID); !stillThere {
		t.Error("the cloud cluster disappeared despite deletionPolicy Retain")
	}
	// The finalizer must be released, or the object would hang forever.
	var remaining v1alpha1.CloudClusterList
	if err := h.k8s.List(context.Background(), &remaining); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(remaining.Items) != 0 {
		t.Errorf("CloudCluster still present after deletion: %+v", remaining.Items[0].Finalizers)
	}
}

func TestDeleteWithTerminatePolicyDestroysCloudCluster(t *testing.T) {
	cluster := provisionCluster("scratch", func(c *v1alpha1.CloudCluster) {
		c.Spec.DeletionPolicy = v1alpha1.DeletionTerminate
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)
	h.reconcileClusterUntilSettled("scratch", 6)
	settled := h.getCluster("scratch")
	productID := settled.Status.ProductID

	if err := h.k8s.Delete(context.Background(), settled); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	h.reconcileCluster("scratch")

	if h.cloud.CallCount("TerminateCluster") != 1 {
		t.Fatalf("TerminateCluster called %d times, want 1", h.cloud.CallCount("TerminateCluster"))
	}
	if !h.hasEvent(v1alpha1.EventTerminating) {
		t.Error("expected a Terminating event, which outlives the object")
	}
	// Terminate is asynchronous: the cluster enters Terminating, and once the
	// window passes it reaches Terminated. It does not disappear, because
	// RavenDB Cloud does not delete the record.
	h.cloud.State().Advance(time.Minute)
	remote, stillThere := h.cloudCluster(productID)
	if !stillThere {
		t.Fatal("the record should survive termination; only its status changes")
	}
	if remote.Status != "Terminated" {
		t.Errorf("cloud status = %q, want Terminated", remote.Status)
	}
}

// --- Prerequisites ---------------------------------------------------------

func TestClusterWaitsForAccountToBecomeReady(t *testing.T) {
	account := readyAccount()
	account.Status.Conditions = []metav1.Condition{{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonKeyMissing,
		Message:            "Secret \"ravendb-cloud-api-key\" has no key \"apiKey\"",
		LastTransitionTime: metav1.Now(),
	}}
	h := newHarness(t, credentialSecret(), account, provisionCluster("orders"))

	h.reconcileCluster("orders")
	settled := h.getCluster("orders")

	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("nothing should be created while the account is not Ready")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonWaitingForAccount {
		t.Fatalf("Ready condition = %+v, want reason WaitingForAccount", cond)
	}
	// The account's own error should be quoted, so the user does not have to
	// go hunting across two objects.
	if !contains(cond.Message, `"apiKey"`) {
		t.Errorf("message %q does not carry the account's own reason", cond.Message)
	}
}

// TestAMistypedAccountSaysHowToFixIt: accountRef cannot be edited, so the
// advice for a name that matches no account has to be the one that works.
func TestAMistypedAccountSaysHowToFixIt(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.AccountRef.Name = "ravendb-clod"
	}))

	h.reconcileCluster("orders")

	cond := meta.FindStatusCondition(h.getCluster("orders").Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonWaitingForAccount {
		t.Fatalf("Ready = %+v, want WaitingForAccount", cond)
	}
	if !contains(cond.Message, "spec.accountRef") {
		t.Errorf("message %q does not name the field a typo is in", cond.Message)
	}
}

func TestBoundClusterDeletedInPortalIsReportedNotRecreated(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	settled := h.getCluster("orders")

	// Gone with no Terminating state and no warning. RavenDB Cloud itself keeps
	// the record, so this is the rarer case; the next test is the usual one.
	if !h.cloud.State().Vanish(settled.Status.ProductID) {
		t.Fatalf("cluster %s was not in the simulated cloud to begin with", settled.Status.ProductID)
	}

	h.reconcileCluster("orders")
	settled = h.getCluster("orders")

	if h.cloud.CallCount("CreateCluster") != 1 {
		t.Error("the operator must not silently re-create a cluster that was deleted out of band")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonDeletedInCloud {
		t.Fatalf("Ready = %+v, want DeletedInCloud", cond)
	}
	if !contains(cond.Message, "Delete this object") {
		t.Errorf("message %q does not give the way out", cond.Message)
	}
}

// TestAClusterTerminatedInThePortalSaysSo is the usual way a cluster goes
// away without its object: terminated in the portal, which leaves a record
// with status Terminated rather than a 404, which must not read as a generic
// cloud error.
func TestAClusterTerminatedInThePortalSaysSo(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)
	settled := h.getCluster("orders")

	h.cloud.State().Patch(settled.Status.ProductID, "", 0, nil, "Terminated")
	h.reconcileCluster("orders")

	cond := meta.FindStatusCondition(h.getCluster("orders").Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonDeletedInCloud {
		t.Fatalf("Ready = %+v, want DeletedInCloud", cond)
	}
	// Applying again would be refused on the name until the old record is
	// hidden, so the advice has to include that step.
	if !contains(cond.Message, "hiding the terminated one") {
		t.Errorf("message %q does not say the name is held until the cluster is hidden", cond.Message)
	}
	// Nothing is left to compare the manifest with, so CHANGES says nothing
	// rather than go on claiming Applied.
	if synced := meta.FindStatusCondition(h.getCluster("orders").Status.Conditions, v1alpha1.ConditionSynced); synced != nil {
		t.Errorf("Synced = %+v on a cluster that is gone, want it removed", synced)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// --- Subdomain derivation --------------------------------------------------

// The RavenDB Cloud subdomain rules are strict and undocumented in the OpenAPI
// spec: required despite being marked nullable, at most 13 characters, letters
// digits and dashes only. These cover the derivation that keeps a minimal
// manifest working without inventing a name the user did not choose.
func TestResolveSubdomain(t *testing.T) {
	cases := []struct {
		name        string
		explicit    string
		displayName string
		want        string
		wantErr     string
	}{
		{name: "explicit wins", explicit: "orders", displayName: "anything at all", want: "orders"},
		{name: "derived from display name", displayName: "orders", want: "orders"},
		{name: "at the boundary", displayName: "thirteenchars", want: "thirteenchars"},
		{name: "sanitised", displayName: "My_Orders.v2", want: "MyOrdersv2"},
		{name: "dashes trimmed", displayName: "-orders-", want: "orders"},
		{
			// Nothing exists yet, so the field can still be added.
			name: "too long is refused, not truncated", displayName: "orders-production",
			wantErr: "Set spec.subdomain",
		},
		{
			name: "nothing usable left", displayName: "___",
			wantErr: "cannot derive a subdomain",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSubdomain(tc.explicit, tc.displayName)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveSubdomain(%q, %q) = %q, want an error", tc.explicit, tc.displayName, got)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSubdomain(%q, %q): %v", tc.explicit, tc.displayName, err)
			}
			if got != tc.want {
				t.Errorf("resolveSubdomain(%q, %q) = %q, want %q", tc.explicit, tc.displayName, got, tc.want)
			}
			if len(got) > maxSubdomainLength {
				t.Errorf("returned %d characters, more than the RavenDB Cloud limit of %d", len(got), maxSubdomainLength)
			}
		})
	}
}

// TestProvisionSendsADerivedSubdomain is the end-to-end half: a manifest with
// no subdomain must still produce a create call that carries one, because the
// cloud rejects an empty value.
func TestProvisionSendsADerivedSubdomain(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))

	h.reconcileClusterUntilSettled("orders", 6)

	if got := h.cloud.CallCount("CreateCluster"); got != 1 {
		t.Fatalf("CreateCluster called %d times, want 1", got)
	}
	var sent struct {
		SubdomainName string `json:"subdomainName"`
	}
	h.lastRequestTo("CreateCluster", &sent)
	if sent.SubdomainName != "orders" {
		t.Errorf("subdomainName sent = %q, want %q derived from the object name", sent.SubdomainName, "orders")
	}
}

// TestProvisionRefusesAnUnusableDerivedSubdomain proves the failure is a
// readable condition rather than a 400 from the control plane.
func TestProvisionRefusesAnUnusableDerivedSubdomain(t *testing.T) {
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.DisplayName = "orders-production-eu"
	})
	h := newHarness(t, credentialSecret(), readyAccount(), cluster)

	h.reconcileCluster("orders")
	h.reconcileCluster("orders")

	settled := h.getCluster("orders")
	if h.cloud.CallCount("CreateCluster") != 0 {
		t.Error("a subdomain the cloud would reject must not reach the create call")
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("Ready condition = %+v, want reason InvalidSpec", cond)
	}
	if !contains(cond.Message, "spec.subdomain") {
		t.Errorf("message %q does not say which field to set", cond.Message)
	}
}

// TestRejectedCreateStopsRetrying: RavenDB Cloud answers a request it will
// never accept with a 4xx, and keeps answering that way while the spec says the
// same thing. Requeueing on a timer regardless spends one call every 30
// seconds, forever, and never converges, which is why a non-429 4xx is
// terminal.
func TestRejectedCreateStopsRetrying(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("rejected"))
	h.cloud.FailNext("CreateCluster", http.StatusBadRequest)

	result := h.reconcileCluster("rejected")

	if result.RequeueAfter != 0 {
		t.Errorf("result = %+v, want no requeue: the API will refuse this again", result)
	}

	cluster := h.getCluster("rejected")
	cond := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonRefused {
		t.Fatalf("Ready condition = %+v, want reason Refused", cond)
	}
	// Both halves of the policy: no timer, and an edit is the way on. Every
	// field can still change, since nothing was created.
	if !contains(cond.Message, "not retried on a timer") {
		t.Errorf("message %q does not say the operator stopped trying", cond.Message)
	}
	if !contains(cond.Message, "every spec field can still be changed") {
		t.Errorf("message %q does not say the spec can still be edited", cond.Message)
	}

	// The refusal has to reach kubectl describe, not just the condition, since
	// nothing else is going to happen until a human reads it.
	if !h.hasEvent(v1alpha1.ReasonRefused) {
		t.Error("expected a Warning event carrying the refusal")
	}

	// Worth being precise about the scope: this stops the operator scheduling
	// another attempt. A reconcile triggered from outside, by a resync or a
	// watch, still tries, which is right because the world may have changed.
	// What is gone is the 30 second timer that retried a permanent refusal
	// forever.
}

// A 500 is the other half of the rule: the cloud might well answer differently
// next time, so that one is handed back to controller-runtime to retry.
func TestServerErrorOnCreateIsStillRetried(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("rejected"))
	h.cloud.FailNext("CreateCluster", http.StatusInternalServerError)

	_, err := h.cluster.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "rejected"},
	})
	if err == nil {
		t.Fatal("a 500 should be returned so controller-runtime backs off and retries")
	}

	cluster := h.getCluster("rejected")
	cond := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonCloudUnreachable {
		t.Fatalf("Ready condition = %+v, want reason CloudUnreachable, not a terminal Refused", cond)
	}
}

// A quota refusal is the one 4xx that lifts without anyone touching this
// object, so it keeps a slow timer where every other refusal parks. Treating
// all of them alike would leave the object stuck for up to controller-runtime's
// ten hour resync after the user had already freed a slot.
func TestQuotaRefusalKeepsRetryingSlowly(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("quota"))
	somebodyElses := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "elsewhere"})
	h.cloud.State().SetProductQuota(1)

	result := h.reconcileCluster("quota")

	if result.RequeueAfter == 0 {
		t.Error("a quota refusal must keep a timer: nothing the operator watches can see the quota free up")
	}
	if result.RequeueAfter < time.Minute {
		t.Errorf("RequeueAfter = %v, want a slow retry rather than a hot loop", result.RequeueAfter)
	}

	cluster := h.getCluster("quota")
	cond := meta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonQuotaReached {
		t.Fatalf("Ready condition = %+v, want reason QuotaReached", cond)
	}
	if !contains(cond.Message, "Terminate a cluster") {
		t.Errorf("message %q should name the actual remedy", cond.Message)
	}

	// Somebody frees the slot, and the next timed pass creates the cluster.
	h.cloud.State().Patch(somebodyElses.ID, "", 0, nil, "Terminated")
	h.reconcileCluster("quota")
	if h.getCluster("quota").Status.ProductID == "" {
		t.Error("the create did not go through once the quota had room")
	}
}

// TestAClusterBeingTerminatedIsNotTakenOver: the lookup skips a cluster that is
// already Terminated but not one on its way there, and binding to it would
// leave the object reporting DeletedInCloud for good once it got there.
func TestAClusterBeingTerminatedIsNotTakenOver(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders"))
	dying := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "orders"})
	h.cloud.State().Patch(dying.ID, "", 0, nil, "Terminating")

	h.reconcileCluster("orders")
	h.reconcileCluster("orders")

	settled := h.getCluster("orders")
	if settled.Status.ProductID != "" {
		t.Errorf("bound to %q, a cluster being terminated", settled.Status.ProductID)
	}
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonNameTaken || !contains(cond.Message, "being terminated") {
		t.Errorf("Ready = %+v, want NameTaken saying the cluster is being terminated", cond)
	}
}

// TestANameMatchesWhateverItsCase: RavenDB Cloud treats "Orders" and "orders"
// as the same name, so a manifest has to find its cluster either way rather
// than try to create one the cloud would refuse.
func TestANameMatchesWhateverItsCase(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), takeOverCluster("orders"))
	existing := h.seedCloudCluster(cloudsim.SeedSpec{DisplayName: "Orders", SubdomainName: "orders"})

	settled := h.reconcileClusterUntilSettled("orders", 5)

	if settled.Status.ProductID != existing.ID || h.cloud.CallCount("CreateCluster") != 0 {
		t.Errorf("productID = %q after %d creates, want the cluster named Orders and no create",
			settled.Status.ProductID, h.cloud.CallCount("CreateCluster"))
	}
}

// TestDeletingAClusterAlreadyGoneTouchesNothing: once RavenDB Cloud has
// terminated the cluster, deleting its object has nothing to terminate and
// nothing to retain, so it must neither announce "leaving RavenDB Cloud
// cluster 17-A running" about a cluster that is gone nor send a terminate.
func TestDeletingAClusterAlreadyGoneTouchesNothing(t *testing.T) {
	for _, policy := range []v1alpha1.DeletionPolicy{v1alpha1.DeletionRetain, v1alpha1.DeletionTerminate} {
		t.Run(string(policy), func(t *testing.T) {
			h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
				c.Spec.DeletionPolicy = policy
			}))
			h.reconcileClusterUntilSettled("orders", 6)
			h.cloud.State().Patch(h.getCluster("orders").Status.ProductID, "", 0, nil, "Terminated")
			h.reconcileCluster("orders")
			h.events()

			if err := h.k8s.Delete(context.Background(), h.getCluster("orders")); err != nil {
				t.Fatalf("deleting: %v", err)
			}
			h.reconcileCluster("orders")

			if n := h.cloud.CallCount("TerminateCluster"); n != 0 {
				t.Errorf("TerminateCluster called %d times for a cluster already terminated", n)
			}
			if events := h.events(); len(events) != 0 {
				t.Errorf("events %q about a cluster that is already gone", events)
			}
			var left v1alpha1.CloudClusterList
			if err := h.k8s.List(context.Background(), &left); err != nil || len(left.Items) != 0 {
				t.Errorf("the object is still there: %d left, %v", len(left.Items), err)
			}
		})
	}
}
