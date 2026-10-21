package controller

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// buildCloudCertificateZip reproduces the archive RavenDB Cloud actually
// serves: a date-stamped PFX pair at the root, PEM material under PEM/, plus
// PASSWORD.TXT and README.TXT.
func buildCloudCertificateZip(t *testing.T, subdomain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	write := func(name, content string) {
		t.Helper()
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("creating zip entry %q: %v", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("writing zip entry %q: %v", name, err)
		}
	}

	write("PASSWORD.TXT", "s3cr3t-pfx-password\n")
	write("README.TXT", "General FAQ ...")
	write(subdomain+".client.certificate.2026-08-28.pfx", "PFX-NO-PASSWORD")
	write(subdomain+".client.certificate.2026-08-28.with.password.pfx", "PFX-WITH-PASSWORD")
	write("PEM/"+subdomain+".client.certificate.crt", "-----BEGIN CERTIFICATE-----\nMII...\n-----END CERTIFICATE-----\n")
	write("PEM/"+subdomain+".client.certificate.key", "-----BEGIN RSA PRIVATE KEY-----\nMII...\n-----END RSA PRIVATE KEY-----\n")
	write("PEM/"+subdomain+".client.certificate.pem", "-----BEGIN CERTIFICATE-----\nMII...\n-----BEGIN RSA PRIVATE KEY-----\n")

	if err := w.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// connectionData is the two halves of the Secret as an application sees them,
// which is the only place they are ever combined now that each is applied by
// its own field manager.
func connectionData(t *testing.T, cluster *cloudapi.Cluster, cert *cloudapi.ClientCertificate) (map[string][]byte, error) {
	t.Helper()
	data := endpointData(cluster)
	if cert == nil {
		return data, nil
	}
	certificate, err := certificateData(cert)
	for k, v := range certificate {
		data[k] = v
	}
	return data, err
}

func TestConnectionDataUnpacksCertificateArchive(t *testing.T) {
	cluster := &cloudapi.Cluster{
		ID:       "products/7-A",
		URLs:     []string{"https://a.orders.acme.ravendb.cloud", "https://b.orders.acme.ravendb.cloud"},
		NodeTags: []string{"A", "B"},
	}
	cert := &cloudapi.ClientCertificate{Data: buildCloudCertificateZip(t, "orders.acme")}

	data, err := connectionData(t, cluster, cert)
	if err != nil {
		t.Fatalf("connectionData: %v", err)
	}

	cases := map[string]string{
		secretKeyProductID:             "products/7-A",
		secretKeyURL:                   "https://a.orders.acme.ravendb.cloud",
		secretKeyURLs:                  "https://a.orders.acme.ravendb.cloud,https://b.orders.acme.ravendb.cloud",
		secretKeyNodeTags:              "A,B",
		secretKeyStudioURL:             "https://a.orders.acme.ravendb.cloud/studio/index.html",
		secretKeyClientPFX:             "PFX-NO-PASSWORD",
		secretKeyClientPFXWithPassword: "PFX-WITH-PASSWORD",
		secretKeyPFXPassword:           "s3cr3t-pfx-password",
	}
	for key, want := range cases {
		if got := string(data[key]); got != want {
			t.Errorf("secret[%q] = %q, want %q", key, got, want)
		}
	}

	// tls.crt and tls.key are the whole point: a pod can mount this Secret and
	// use it the same way it would any other Kubernetes TLS material.
	if !strings.Contains(string(data[secretKeyTLSCert]), "BEGIN CERTIFICATE") {
		t.Errorf("secret[%q] does not look like a PEM certificate", secretKeyTLSCert)
	}
	if !strings.Contains(string(data[secretKeyTLSKey]), "PRIVATE KEY") {
		t.Errorf("secret[%q] does not look like a PEM private key", secretKeyTLSKey)
	}
	if !strings.Contains(string(data[secretKeyClientPEM]), "BEGIN CERTIFICATE") {
		t.Errorf("secret[%q] missing combined PEM", secretKeyClientPEM)
	}
	// README.TXT carries no credentials and would only be noise in a Secret.
	if _, ok := data["README.TXT"]; ok {
		t.Error("README.TXT should not be published into the Secret")
	}
}

func TestConnectionDataWithoutCertificate(t *testing.T) {
	cluster := &cloudapi.Cluster{ID: "x", URLs: []string{"https://a.example"}, NodeTags: []string{"A"}}

	data, err := connectionData(t, cluster, nil)
	if err != nil {
		t.Fatalf("connectionData: %v", err)
	}
	if _, ok := data[secretKeyTLSCert]; ok {
		t.Error("no certificate was supplied, so tls.crt must be absent")
	}
	if string(data[secretKeyURL]) != "https://a.example" {
		t.Errorf("url = %q", data[secretKeyURL])
	}
}

func TestConnectionDataFallsBackToRawBundle(t *testing.T) {
	cluster := &cloudapi.Cluster{ID: "x", URLs: []string{"https://a.example"}}
	cert := &cloudapi.ClientCertificate{Data: []byte("not-a-zip-at-all")}

	data, err := connectionData(t, cluster, cert)
	if err == nil {
		t.Fatal("expected an error describing the unusable archive")
	}
	// The payload is still published: a caller who knows the format can use it,
	// and dropping material the cloud handed us would be worse.
	if string(data[secretKeyRawBundle]) != "not-a-zip-at-all" {
		t.Errorf("raw bundle = %q", data[secretKeyRawBundle])
	}
	// Endpoints are a separate field manager now, so an unusable certificate
	// cannot take them down with it.
	if string(data[secretKeyURL]) != "https://a.example" {
		t.Error("endpoints must still be published when the certificate is unusable")
	}
}

func TestExtractRejectsArchiveWithoutCertificate(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("README.TXT")
	_, _ = f.Write([]byte("nothing useful"))
	_ = w.Close()

	if _, err := extractCertificateBundle(buf.Bytes()); err == nil {
		t.Fatal("expected an error when the archive holds no PEM certificate")
	}
}

func TestStudioURL(t *testing.T) {
	for input, want := range map[string]string{
		"https://a.orders.example":  "https://a.orders.example/studio/index.html",
		"https://a.orders.example/": "https://a.orders.example/studio/index.html",
	} {
		if got := studioURL(input); got != want {
			t.Errorf("studioURL(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestCertificateIsRefreshedBeforeItExpires covers a failure that arrives on a
// clock rather than on a change.
//
// RavenDB Cloud issues client certificates for five years and regenerates them
// automatically thirty days before expiry. Checking "does the Secret have a
// certificate" rather than "is the certificate still going to work" downloads
// the bundle once and never again: nothing fails for five years, and then
// every application mounting that Secret does.
func TestCertificateIsRefreshedBeforeItExpires(t *testing.T) {
	cases := []struct {
		name        string
		notAfter    time.Duration
		wantRefetch bool
	}{
		{"a fresh five year certificate", 5 * 365 * 24 * time.Hour, false},
		{"a year left", 365 * 24 * time.Hour, false},
		{"just outside the window", 30 * 24 * time.Hour, false},
		{"inside the window, the replacement already exists", 14 * 24 * time.Hour, true},
		{"already expired", -24 * time.Hour, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pemBytes := selfSignedPEM(t, time.Now().Add(tc.notAfter))
			if got := expiringSoon(pemBytes); got != tc.wantRefetch {
				t.Errorf("expiringSoon = %v, want %v for a certificate with %v left", got, tc.wantRefetch, tc.notAfter)
			}
			if exp := certificateExpiry(pemBytes); exp == nil {
				t.Error("certificateExpiry returned nil for a valid certificate")
			}
		})
	}
}

// Garbage answers false rather than true, so one unreadable Secret cannot turn
// into a download on every poll forever.
func TestUnreadableCertificateDoesNotLoop(t *testing.T) {
	for _, junk := range [][]byte{nil, []byte("not pem at all"), []byte("-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydA==\n-----END CERTIFICATE-----\n")} {
		if expiringSoon(junk) {
			t.Errorf("expiringSoon(%q) = true; an unreadable certificate must not trigger a refetch", junk)
		}
		if certificateExpiry(junk) != nil {
			t.Errorf("certificateExpiry(%q) should be nil", junk)
		}
	}
}

func selfSignedPEM(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestConnectionSecretRefusesToAdoptSomebodyElsesSecret is a privilege
// boundary, not a convenience check.
//
// The operator holds cluster-wide write on Secrets and stamps an owner
// reference with BlockOwnerDeletion on whatever it publishes. Without this
// refusal, pointing spec.connectionSecret.name at any existing Secret in the
// namespace made the operator adopt it, merge the cluster's keys into it, and
// garbage collect it when the CloudCluster went away. That turns "may create a
// CloudCluster" into "may delete any Secret in the namespace", which is not a
// permission the RBAC for creating a CloudCluster grants.
func TestConnectionSecretRefusesToAdoptSomebodyElsesSecret(t *testing.T) {
	bystander := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte("not-ours")},
	}
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.ConnectionSecret = &v1alpha1.ConnectionSecretSpec{Name: "someone-elses"}
	})
	h := newHarness(t, credentialSecret(), readyAccount(), bystander, cluster)

	h.reconcileClusterUntilSettled("orders", 6)

	var got corev1.Secret
	ref := types.NamespacedName{Namespace: testNamespace, Name: "someone-elses"}
	if err := h.k8s.Get(context.Background(), ref, &got); err != nil {
		t.Fatalf("the bystander Secret should still exist: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("the operator took ownership of a Secret it did not create: %+v", got.OwnerReferences)
	}
	if string(got.Data["password"]) != "not-ours" {
		t.Errorf("the original data was disturbed: %q", string(got.Data["password"]))
	}
	for _, key := range []string{secretKeyURLs, secretKeyURL, secretKeyTLSCert} {
		if _, ok := got.Data[key]; ok {
			t.Errorf("the operator wrote %q into a Secret it does not own", key)
		}
	}

	// And the user is told why, on the object rather than only in a log.
	settled := h.getCluster("orders")
	// NameTaken, not Running. The cluster itself is fine, but the object did
	// not get the Secret its spec asked for, and STATUS is the printer column,
	// so Running here would say "done" about a manifest that was not honoured.
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonNameTaken {
		t.Fatalf("Ready = %+v, want reason NameTaken", cond)
	}
	if !contains(cond.Message, "spec.connectionSecret.name") {
		t.Errorf("message %q does not name the field to change", cond.Message)
	}

	// Terminal, not retried. reconcileCluster fails the test on a returned
	// error, so reaching here at all proves controller-runtime was not handed
	// one to back off on. The requeue is the ordinary poll, which is what
	// notices if somebody frees the name.
	result := h.reconcileCluster("orders")
	if result.RequeueAfter != settled.EffectivePollInterval() {
		t.Errorf("requeueAfter = %s, want the poll interval %s",
			result.RequeueAfter, settled.EffectivePollInterval())
	}
}

// TestTurningTheCertificateOffRemovesIt is the case the two field managers
// exist for, and it was a real bug before them.
//
// The old write merged keys into the Secret and never removed any, so setting
// includeCertificate to false on a Secret that already had a certificate left
// the private key published. The user asked the operator to stop publishing it
// and the operator kept publishing it.
//
// Applying the certificate manager with no keys is what prunes them, and the
// endpoint keys have to survive that, because they belong to the other manager.
func TestTurningTheCertificateOffRemovesIt(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	before := h.getSecret("orders-connection")
	if len(before.Data[secretKeyTLSKey]) == 0 {
		t.Fatal("no certificate was published, so this test cannot prove it is removed")
	}

	cluster := h.getCluster("orders")
	off := false
	cluster.Spec.ConnectionSecret = &v1alpha1.ConnectionSecretSpec{IncludeCertificate: &off}
	if err := h.k8s.Update(context.Background(), cluster); err != nil {
		t.Fatalf("turning the certificate off: %v", err)
	}
	h.reconcileClusterUntilSettled("orders", 6)

	after := h.getSecret("orders-connection")
	for _, key := range certificateSecretKeys {
		if _, ok := after.Data[key]; ok {
			t.Errorf("%q is still published after includeCertificate went false", key)
		}
	}
	// The endpoints belong to the other field manager and must be untouched.
	for _, key := range []string{secretKeyURLs, secretKeyURL, secretKeyNodeTags, secretKeyProductID, secretKeyStudioURL} {
		if len(after.Data[key]) == 0 {
			t.Errorf("%q was lost when the certificate was removed", key)
		}
	}
	if !h.getCluster("orders").IsRunning() {
		t.Error("the Secret is exactly what was asked for, so the cluster should be Running")
	}
}

// TestRenamingTheSecretEmptiesTheOldOne is the third way to reach the same
// bug, and the one nothing was looking for.
//
// The operator publishes into spec.connectionSecret.name. Change that name and
// it publishes into the new one and never touches the old, which keeps the
// private key and the PFX password at an address the spec no longer mentions.
// It is owned, so deleting the CloudCluster still collects it, but until then
// it is a credential sitting in the namespace that nothing accounts for.
//
// Lab 03 renames twice and then asserts garbage collection at the end, so both
// leftovers were visible in its output the whole time and read as ordinary.
func TestRenamingTheSecretEmptiesTheOldOne(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	before := h.getSecret("orders-connection")
	if len(before.Data[secretKeyTLSKey]) == 0 {
		t.Fatal("no certificate was published, so this test cannot prove it moves")
	}

	cluster := h.getCluster("orders")
	cluster.Spec.ConnectionSecret = &v1alpha1.ConnectionSecretSpec{Name: "orders-elsewhere"}
	if err := h.k8s.Update(context.Background(), cluster); err != nil {
		t.Fatalf("renaming the Secret: %v", err)
	}
	h.reconcileClusterUntilSettled("orders", 6)

	old := h.getSecret("orders-connection")
	if len(old.Data) != 0 {
		t.Errorf("the old Secret still holds %d keys after the rename: %v",
			len(old.Data), slices.Sorted(maps.Keys(old.Data)))
	}
	// Still owned, so garbage collection still reaches it. Dropping the owner
	// reference here would strand the object instead of emptying it.
	if len(old.OwnerReferences) == 0 {
		t.Error("the old Secret lost its owner reference, so nothing will collect it")
	}

	// And the rename actually happened, or the check above proves nothing.
	renamed := h.getSecret("orders-elsewhere")
	if len(renamed.Data[secretKeyTLSKey]) == 0 {
		t.Error("the new Secret has no certificate, so the keys went nowhere")
	}
	if got := h.getCluster("orders").Status.ConnectionSecretName; got != "orders-elsewhere" {
		t.Errorf("status.connectionSecretName = %q, want the new name", got)
	}
}

// TestRenamingAndDisablingTogetherLeavesNothingBehind is the edit that reaches
// two Secrets at once, and the reason retirement is driven by
// status.connectionSecretName rather than by which field changed.
//
// The operator never wrote the new name, so there is nothing there to take
// back; the keys are all at the old one, which the spec no longer mentions and
// which enabled: false on its own would not have looked at.
func TestRenamingAndDisablingTogetherLeavesNothingBehind(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	cluster := h.getCluster("orders")
	off := false
	cluster.Spec.ConnectionSecret = &v1alpha1.ConnectionSecretSpec{Name: "orders-elsewhere", Enabled: &off}
	if err := h.k8s.Update(context.Background(), cluster); err != nil {
		t.Fatalf("renaming and disabling: %v", err)
	}
	h.reconcileClusterUntilSettled("orders", 6)

	old := h.getSecret("orders-connection")
	if len(old.Data) != 0 {
		t.Errorf("the Secret the keys were actually in still holds %v",
			slices.Sorted(maps.Keys(old.Data)))
	}
}

// TestTurningPublicationOffRemovesEverything is the same bug as
// TestTurningTheCertificateOffRemovesIt, one field over, and it survived that
// fix: measured against the live API, flipping enabled to false left all eleven
// keys in the Secret, tls.key and the PFX password included. Only the status
// field and the condition changed, so the object said the operator had stopped
// publishing while the credential was still sitting in the namespace.
func TestTurningPublicationOffRemovesEverything(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	before := h.getSecret("orders-connection")
	if len(before.Data[secretKeyTLSKey]) == 0 || len(before.Data[secretKeyURLs]) == 0 {
		t.Fatal("nothing was published, so this test cannot prove it is taken back")
	}

	cluster := h.getCluster("orders")
	off := false
	cluster.Spec.ConnectionSecret = &v1alpha1.ConnectionSecretSpec{Enabled: &off}
	if err := h.k8s.Update(context.Background(), cluster); err != nil {
		t.Fatalf("turning publication off: %v", err)
	}
	h.reconcileClusterUntilSettled("orders", 6)

	after := h.getSecret("orders-connection")
	if len(after.Data) != 0 {
		t.Errorf("enabled went false and the Secret still holds %d keys: %v",
			len(after.Data), slices.Sorted(maps.Keys(after.Data)))
	}

	// The object survives on purpose. The operator has no delete on Secrets,
	// and the owner reference is what collects this one with the CloudCluster.
	if len(after.OwnerReferences) == 0 {
		t.Error("the owner reference was dropped, so nothing will collect the Secret")
	}

	settled := h.getCluster("orders")
	if settled.Status.ConnectionSecretName != "" {
		t.Errorf("status.connectionSecretName = %q, want empty", settled.Status.ConnectionSecretName)
	}
	// Publishing nothing is what was asked for, so the cluster is Running, and
	// the message says the Secret is empty on purpose.
	cond := meta.FindStatusCondition(settled.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != v1alpha1.ReasonRunning || !contains(cond.Message, "enabled is false") {
		t.Fatalf("Ready = %+v, want Running saying publishing is off", cond)
	}
}

// TestPublicationOffDoesNotTouchSomebodyElsesSecret closes the same privilege
// boundary as TestConnectionSecretRefusesToAdoptSomebodyElsesSecret on the path
// that takes keys out rather than putting them in.
//
// Unpublishing writes with the same two field managers, and the manager that
// owns the object stamps the owner reference. Run against a Secret this cluster
// does not control, it would hand a stranger's Secret a BlockOwnerDeletion
// reference to the CloudCluster and have it garbage collected along with it.
// Deleting through an apply that claims no keys is still deleting.
func TestPublicationOffDoesNotTouchSomebodyElsesSecret(t *testing.T) {
	bystander := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte("not-ours")},
	}
	off := false
	cluster := provisionCluster("orders", func(c *v1alpha1.CloudCluster) {
		c.Spec.ConnectionSecret = &v1alpha1.ConnectionSecretSpec{Name: "someone-elses", Enabled: &off}
	})
	h := newHarness(t, credentialSecret(), readyAccount(), bystander, cluster)

	h.reconcileClusterUntilSettled("orders", 6)

	got := h.getSecret("someone-elses")
	if len(got.OwnerReferences) != 0 {
		t.Errorf("unpublishing took ownership of a Secret the cluster does not own: %+v", got.OwnerReferences)
	}
	if string(got.Data["password"]) != "not-ours" {
		t.Errorf("the operator emptied a Secret it does not own: data = %v", slices.Sorted(maps.Keys(got.Data)))
	}
}

// TestAnEndpointsOnlyPassLeavesTheCertificateAlone is the invariant that makes
// the split safe. Most reconciles have nothing to say about the certificate,
// and a single field manager owning both halves would prune it on every one of
// them.
func TestAnEndpointsOnlyPassLeavesTheCertificateAlone(t *testing.T) {
	h := newHarness(t, credentialSecret(), readyAccount(), provisionCluster("orders"))
	h.reconcileClusterUntilSettled("orders", 6)

	before := h.getSecret("orders-connection")
	downloads := h.cloud.CallCount("GetClientCertificate")
	if len(before.Data[secretKeyTLSCert]) == 0 {
		t.Fatal("no certificate to preserve")
	}

	for range 3 {
		h.reconcileCluster("orders")
	}

	after := h.getSecret("orders-connection")
	for _, key := range certificateSecretKeys {
		if string(after.Data[key]) != string(before.Data[key]) {
			t.Errorf("%q changed on a pass that did not download a certificate", key)
		}
	}
	if got := h.cloud.CallCount("GetClientCertificate"); got != downloads {
		t.Errorf("GetClientCertificate went from %d to %d; a stored certificate must not be re-downloaded", downloads, got)
	}
}
