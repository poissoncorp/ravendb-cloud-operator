package controller

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// Secret keys published for applications. These names are the operator's
// user-facing contract; tls.crt and tls.key are what every other Kubernetes
// workload already expects.
const (
	secretKeyURLs                  = "urls"
	secretKeyURL                   = "url"
	secretKeyNodeTags              = "nodeTags"
	secretKeyStudioURL             = "studioURL"
	secretKeyProductID             = "productID"
	secretKeyTLSCert               = "tls.crt"
	secretKeyTLSKey                = "tls.key"
	secretKeyClientPEM             = "client.pem"
	secretKeyClientPFX             = "client.pfx"
	secretKeyClientPFXWithPassword = "client.with-password.pfx"
	secretKeyPFXPassword           = "certificate.password"
	secretKeyRawBundle             = "certificate.bundle"
)

var (
	// errSecretNotOurs means the target Secret exists and this cluster does not
	// control it.
	errSecretNotOurs = errors.New("connection Secret is owned by something else")

	// errCertificateDownload marks a failure fetching the certificate, as
	// opposed to writing the Secret.
	errCertificateDownload = errors.New("downloading the client certificate")
)

// noEndpointsError means RavenDB Cloud reported the cluster Active with no node
// addresses. A renamed dns field upstream looks exactly like this.
type noEndpointsError struct {
	productID, secret string
}

func (e *noEndpointsError) Error() string {
	return fmt.Sprintf(
		"RavenDB Cloud reports cluster %s as Active with no node addresses, so there is nothing to publish; "+
			"Secret %q keeps what it had. This usually means the RavenDB Cloud API changed and this operator "+
			"needs upgrading.", e.productID, e.secret)
}

// secretState is what the connection Secret ended up holding, as the Ready
// condition says it.
type secretState struct {
	reason, message string
}

// The Secret is written by server-side apply under two field managers, because
// endpoints are applied on every pass and the certificate only when it changes.
// Fields a manager stops sending are pruned, so one manager for both would
// delete the certificate on every endpoints-only pass.
const (
	fieldOwnerEndpoints   = "ravendb-cloud-operator"
	fieldOwnerCertificate = "ravendb-cloud-operator-certificate"
)

// certificateSecretKeys is everything the certificate manager owns.
var certificateSecretKeys = []string{
	secretKeyTLSCert,
	secretKeyTLSKey,
	secretKeyClientPEM,
	secretKeyClientPFX,
	secretKeyClientPFXWithPassword,
	secretKeyPFXPassword,
	secretKeyRawBundle,
}

// Real certificate bundles are a few kilobytes; these bounds stop a malformed or
// hostile archive from exhausting operator memory.
const (
	maxCertificateEntry = 1 << 20  // 1 MiB
	maxCertificateTotal = 16 << 20 // 16 MiB
)

// endpointData is the half of the Secret an application needs to find the
// cluster.
func endpointData(cluster *cloudapi.Cluster) map[string][]byte {
	data := map[string][]byte{
		secretKeyProductID: []byte(cluster.ID),
		secretKeyURLs:      []byte(strings.Join(cluster.URLs, ",")),
		secretKeyNodeTags:  []byte(strings.Join(cluster.NodeTags, ",")),
	}
	if len(cluster.URLs) > 0 {
		data[secretKeyURL] = []byte(cluster.URLs[0])
		data[secretKeyStudioURL] = []byte(studioURL(cluster.URLs[0]))
	}
	return data
}

// certificateData is the half an application needs to authenticate, unpacked
// from the zip RavenDB Cloud serves. An archive that will not unpack is still
// returned, raw, so a caller who knows the format can use it.
func certificateData(cert *cloudapi.ClientCertificate) (map[string][]byte, error) {
	extracted, err := extractCertificateBundle(cert.Data)
	if err != nil {
		return map[string][]byte{secretKeyRawBundle: cert.Data}, err
	}
	return extracted, nil
}

// storedCertificate returns the certificate keys a Secret already carries.
func storedCertificate(secret *corev1.Secret) map[string][]byte {
	out := map[string][]byte{}
	if secret == nil {
		return out
	}
	for _, k := range certificateSecretKeys {
		if v, ok := secret.Data[k]; ok {
			out[k] = v
		}
	}
	return out
}

// bundleNotUnpacked reports a raw bundle with no tls.crt beside it: a download
// that landed and an unpack that did not. The unpack is retried on those bytes,
// so it needs no second download.
func bundleNotUnpacked(data map[string][]byte) bool {
	return len(data[secretKeyTLSCert]) == 0 && len(data[secretKeyRawBundle]) > 0
}

// StudioURL returns the RavenDB Studio address for a node URL.
func studioURL(nodeURL string) string {
	return strings.TrimRight(nodeURL, "/") + "/studio/index.html"
}

// extractCertificateBundle maps the RavenDB Cloud certificate archive onto
// conventional Secret keys. Entry names embed the subdomain and a date stamp,
// so matching is by suffix and directory:
//
//	PEM/<subdomain>.client.certificate.crt   -> tls.crt
//	PEM/<subdomain>.client.certificate.key   -> tls.key
//	PEM/<subdomain>.client.certificate.pem   -> client.pem
//	<subdomain>.client.certificate.<date>.pfx              -> client.pfx
//	<subdomain>.client.certificate.<date>.with.password.pfx -> client.with-password.pfx
//	PASSWORD.TXT                             -> certificate.password
func extractCertificateBundle(raw []byte) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("certificate payload is not a zip archive: %w", err)
	}

	out := map[string][]byte{}
	var total int64
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		key := classifyCertificateEntry(file.Name)
		if key == "" {
			continue
		}
		// The first test keeps a zip64 header that declares an absurd size from
		// wrapping the int64 conversion negative and slipping past the second.
		if file.UncompressedSize64 > maxCertificateTotal || total+int64(file.UncompressedSize64) > maxCertificateTotal {
			return out, fmt.Errorf("certificate archive exceeds %d bytes", maxCertificateTotal)
		}
		content, err := readZipEntry(file)
		if err != nil {
			return out, fmt.Errorf("reading %q from certificate archive: %w", file.Name, err)
		}
		total += int64(len(content))
		if key == secretKeyPFXPassword {
			content = bytes.TrimSpace(content)
		}
		out[key] = content
	}
	if _, ok := out[secretKeyTLSCert]; !ok {
		return out, fmt.Errorf("certificate archive contained no PEM certificate")
	}
	return out, nil
}

func classifyCertificateEntry(name string) string {
	normalized := strings.ReplaceAll(name, "\\", "/")
	base := path.Base(normalized)
	upper := strings.ToUpper(base)
	inPEMDir := strings.HasPrefix(strings.ToUpper(normalized), "PEM/")

	switch {
	case upper == "PASSWORD.TXT":
		return secretKeyPFXPassword
	case strings.HasSuffix(upper, ".WITH.PASSWORD.PFX"):
		return secretKeyClientPFXWithPassword
	case strings.HasSuffix(upper, ".PFX"):
		return secretKeyClientPFX
	case inPEMDir && strings.HasSuffix(upper, ".CRT"):
		return secretKeyTLSCert
	case inPEMDir && strings.HasSuffix(upper, ".KEY"):
		return secretKeyTLSKey
	case inPEMDir && strings.HasSuffix(upper, ".PEM"):
		return secretKeyClientPEM
	}
	return ""
}

func readZipEntry(file *zip.File) ([]byte, error) {
	rc, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// One byte past the cap, so an oversized entry is an error rather than a
	// truncated private key. archive/zip verifies the CRC32 and declared size
	// only for an entry read to its own EOF.
	content, err := io.ReadAll(io.LimitReader(rc, maxCertificateEntry+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxCertificateEntry {
		return nil, fmt.Errorf("entry exceeds %d bytes", maxCertificateEntry)
	}
	return content, nil
}

// publishConnectionSecret writes the Secret applications mount, and says what
// it ended up holding.
func (r *CloudClusterReconciler) publishConnectionSecret(
	ctx context.Context,
	cluster *v1alpha1.CloudCluster,
	observed *cloudapi.Cluster,
	cloud cloudapi.Client,
) (secretState, error) {
	var none secretState
	retired, err := r.retirePreviousSecret(ctx, cluster)
	if err != nil {
		return none, err
	}

	if !cluster.ConnectionSecretEnabled() {
		cluster.Status.ConnectionSecretName = ""
		return secretState{v1alpha1.ReasonRunning, fmt.Sprintf(
			"The cluster is Active with %d node(s). spec.connectionSecret.enabled is false, so Secret %q holds none of "+
				"this operator's keys.", observed.NodeCount(), cluster.ConnectionSecretName())}, nil
	}

	name := cluster.ConnectionSecretName()
	if retired != "" {
		r.secretPublished(ctx, cluster, "Connection Secret is now %q; %q was emptied.", name, retired)
	}

	existing, err := r.publishEndpoints(ctx, cluster, observed, name)
	if err != nil {
		return none, err
	}

	cert, certPending, err := r.downloadCertificate(ctx, cluster, existing, observed, cloud)
	if err != nil {
		return none, err
	}
	certificate, extractErr := certificateFor(cluster, existing, cert)
	if err := r.writeCertificate(ctx, cluster, name, existing, certificate); err != nil {
		return none, err
	}

	// A pass that left the certificate alone leaves tls.crt as it was.
	stored := storedCertificate(existing)[secretKeyTLSCert]
	if certificate != nil {
		stored = certificate[secretKeyTLSCert]
	}

	cluster.Status.ConnectionSecretName = name
	if cert != nil {
		refreshed := metav1.NewTime(cert.RetrievedAt)
		cluster.Status.CertificateRefreshedAt = &refreshed
	}
	cluster.Status.CertificateExpiresAt = certificateExpiry(stored)

	return describeSecret(cluster, observed, name, stored, extractErr, certPending), nil
}

// publishEndpoints applies the endpoint half of the Secret, and returns the
// Secret as it was before, or nil when it did not exist.
func (r *CloudClusterReconciler) publishEndpoints(
	ctx context.Context, cluster *v1alpha1.CloudCluster, observed *cloudapi.Cluster, name string,
) (*corev1.Secret, error) {
	if len(observed.URLs) == 0 {
		// The Secret keeps what it holds: addresses from the last good read beat
		// none, and this is most likely the API changing shape.
		return nil, &noEndpointsError{productID: observed.ID, secret: name}
	}
	existing, err := r.existingConnectionSecret(ctx, cluster, name)
	if err != nil {
		return nil, err
	}

	// An owner reference alone would not stop taking over a Secret made by
	// hand: it has no controller reference for SetControllerReference to
	// refuse. Claiming it would have it garbage collected with the CloudCluster,
	// turning "may create a CloudCluster" into "may delete any Secret".
	if existing != nil && !metav1.IsControlledBy(existing, cluster) {
		return nil, errSecretNotOurs
	}

	if err := r.applyEndpoints(ctx, cluster, name, endpointData(observed)); err != nil {
		return nil, err
	}
	if existing == nil {
		r.secretPublished(ctx, cluster, "Connection Secret %q created.", name)
	}
	return existing, nil
}

// writeCertificate applies what certificateFor decided: nothing, the removal of
// every certificate key, or new ones.
func (r *CloudClusterReconciler) writeCertificate(
	ctx context.Context, cluster *v1alpha1.CloudCluster, name string, existing *corev1.Secret, certificate map[string][]byte,
) error {
	switch {
	case certificate == nil:
		return nil
	case len(certificate) == 0:
		if err := r.pruneCertificate(ctx, cluster, name, existing); err != nil {
			return err
		}
		r.secretPublished(ctx, cluster, "Removed the client certificate from Secret %q: includeCertificate is false.", name)
		return nil
	}
	return r.applyCertificate(ctx, cluster, name, certificate)
}

// secretPublished records a change to what the connection Secret holds, as an
// event and the matching log line.
func (r *CloudClusterReconciler) secretPublished(ctx context.Context, cluster *v1alpha1.CloudCluster, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	emit(r.Recorder, cluster, corev1.EventTypeNormal, v1alpha1.EventSecretPublished, "%s", message)
	log.FromContext(ctx).Info(message)
}

// existingConnectionSecret returns the named Secret in the cluster's namespace,
// or nil when it does not exist.
func (r *CloudClusterReconciler) existingConnectionSecret(
	ctx context.Context, cluster *v1alpha1.CloudCluster, name string,
) (*corev1.Secret, error) {
	var secret corev1.Secret
	ref := types.NamespacedName{Namespace: cluster.Namespace, Name: name}
	if err := r.Get(ctx, ref, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &secret, nil
}

// certificateFor decides what the certificate manager should apply. A nil map
// means "apply nothing", which leaves what is stored untouched and is the
// common case. An empty map means "apply no keys", which prunes them.
func certificateFor(
	cluster *v1alpha1.CloudCluster,
	existing *corev1.Secret,
	cert *cloudapi.ClientCertificate,
) (map[string][]byte, error) {
	stored := storedCertificate(existing)

	if !cluster.ConnectionSecretIncludesCertificate() {
		if len(stored) == 0 {
			return nil, nil
		}
		return map[string][]byte{}, nil
	}

	if cert != nil {
		data, err := certificateData(cert)
		if err != nil {
			// The new archive's bytes are published, and the certificate already
			// in place is kept: a stale certificate beats none.
			carryForward(data, stored)
		}
		return data, err
	}

	// Retrying the unpack costs no API call, so a truncated payload heals on the
	// next poll.
	if bundleNotUnpacked(stored) {
		recovered, err := extractCertificateBundle(stored[secretKeyRawBundle])
		if err != nil {
			return nil, err
		}
		recovered[secretKeyRawBundle] = stored[secretKeyRawBundle]
		return recovered, nil
	}
	return nil, nil
}

// carryForward copies into data every stored key it does not already have.
func carryForward(data, stored map[string][]byte) {
	for k, v := range stored {
		if _, replaced := data[k]; !replaced {
			data[k] = v
		}
	}
}

// applyEndpoints writes the Secret itself: its metadata, its type, and the
// endpoint keys it is given. This manager owns the object, so its owner
// reference has it garbage collected. The name is a parameter because, after a
// rename, unpublishing names a Secret the spec no longer does.
func (r *CloudClusterReconciler) applyEndpoints(
	ctx context.Context, cluster *v1alpha1.CloudCluster, name string, data map[string][]byte,
) error {
	owner, err := controllerReference(cluster, r.Scheme)
	if err != nil {
		return err
	}
	secret := connectionSecretApply(cluster, name, data).
		WithLabels(map[string]string{"cloud.ravendb.net/cluster": cluster.Name}).
		WithType(corev1.SecretTypeOpaque).
		WithOwnerReferences(owner)
	return r.Apply(ctx, secret,
		client.FieldOwner(fieldOwnerEndpoints), client.ForceOwnership)
}

// retirePreviousSecret empties the Secret status still names when the spec has
// moved on (a rename, or publishing turned off), and returns the name it
// emptied. Otherwise a private key stays in a Secret the operator never writes
// again. Only status can say which Secret that is: the spec no longer names it.
func (r *CloudClusterReconciler) retirePreviousSecret(
	ctx context.Context, cluster *v1alpha1.CloudCluster,
) (string, error) {
	published := cluster.Status.ConnectionSecretName
	if published == "" {
		return "", nil
	}
	if cluster.ConnectionSecretEnabled() && published == cluster.ConnectionSecretName() {
		return "", nil
	}
	return published, r.unpublishConnectionSecret(ctx, cluster, published)
}

// unpublishConnectionSecret takes back every key the operator published. The
// Secret object stays: the operator has no delete on Secrets, and the owner
// reference collects it when the CloudCluster goes.
func (r *CloudClusterReconciler) unpublishConnectionSecret(
	ctx context.Context, cluster *v1alpha1.CloudCluster, name string,
) error {
	existing, err := r.existingConnectionSecret(ctx, cluster, name)
	if err != nil {
		return err
	}
	if existing == nil || !metav1.IsControlledBy(existing, cluster) || len(existing.Data) == 0 {
		return nil
	}
	if err := r.pruneCertificate(ctx, cluster, name, existing); err != nil {
		return err
	}
	return r.applyEndpoints(ctx, cluster, name, map[string][]byte{})
}

// pruneCertificate removes the certificate keys, claiming them first. An apply
// removes only what its own manager owns, and keys written before the operator
// used apply belong to a manager that no longer writes.
func (r *CloudClusterReconciler) pruneCertificate(
	ctx context.Context, cluster *v1alpha1.CloudCluster, name string, existing *corev1.Secret,
) error {
	if err := r.applyCertificate(ctx, cluster, name, storedCertificate(existing)); err != nil {
		return err
	}
	return r.applyCertificate(ctx, cluster, name, map[string][]byte{})
}

// applyCertificate writes the certificate keys and only those, so the keys it
// stops sending are the only ones pruned.
func (r *CloudClusterReconciler) applyCertificate(
	ctx context.Context, cluster *v1alpha1.CloudCluster, name string, data map[string][]byte,
) error {
	return r.Apply(ctx, connectionSecretApply(cluster, name, data),
		client.FieldOwner(fieldOwnerCertificate), client.ForceOwnership)
}

// connectionSecretApply is the part of a write both managers share. Apply owns
// exactly the fields set here.
func connectionSecretApply(
	cluster *v1alpha1.CloudCluster, name string, data map[string][]byte,
) *corev1ac.SecretApplyConfiguration {
	return corev1ac.Secret(name, cluster.Namespace).WithData(data)
}

// controllerReference is SetControllerReference for an apply configuration,
// which controller-runtime has no helper for. It skips the check for another
// controlling owner because publishEndpoints makes a stricter one first.
// BlockOwnerDeletion is why the cloudclusters/finalizers grant exists.
func controllerReference(
	owner *v1alpha1.CloudCluster, scheme *runtime.Scheme,
) (*metav1ac.OwnerReferenceApplyConfiguration, error) {
	gvk, err := apiutil.GVKForObject(owner, scheme)
	if err != nil {
		return nil, fmt.Errorf("resolving the kind of %s: %w", owner.Name, err)
	}
	return metav1ac.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(owner.Name).
		WithUID(owner.UID).
		WithController(true).
		WithBlockOwnerDeletion(true), nil
}

// downloadCertificate fetches the client certificate when the Secret needs one
// it does not already have. Downloading on every poll would make calls for
// nothing.
//
// A nil certificate with pending true means RavenDB Cloud has not issued one
// yet, which is not a failure.
func (r *CloudClusterReconciler) downloadCertificate(
	ctx context.Context,
	cluster *v1alpha1.CloudCluster,
	existing *corev1.Secret,
	observed *cloudapi.Cluster,
	cloud cloudapi.Client,
) (cert *cloudapi.ClientCertificate, pending bool, err error) {
	if !cluster.ConnectionSecretIncludesCertificate() || !certificateNeeded(existing) {
		return nil, false, nil
	}

	cert, err = cloud.GetClientCertificate(ctx, observed.ID)
	switch {
	case err == nil:
		return cert, false, nil
	case cloudapi.IsNotFound(err):
		// A cluster can be Active before its certificate exists, and the
		// endpoint answers 404 until it does. The endpoints are still published.
		return nil, true, nil
	default:
		return nil, false, fmt.Errorf("%w: %w", errCertificateDownload, err)
	}
}

// describeSecret says what the Secret actually ended up holding. It judges on
// what the Secret holds, not on extractErr alone: a pass that downloaded
// nothing must not report success for having attempted nothing.
func describeSecret(
	cluster *v1alpha1.CloudCluster,
	observed *cloudapi.Cluster,
	name string,
	storedCert []byte,
	extractErr error,
	certPending bool,
) secretState {
	if extractErr != nil {
		return secretState{v1alpha1.ReasonUnexpectedResponse, sentence(fmt.Sprintf(
			"The cluster is Active and Secret %q holds its URLs and the raw certificate bundle, but the bundle "+
				"could not be unpacked: %v", name, extractErr)) + " This usually means the RavenDB Cloud API changed " +
			"and this operator needs upgrading."}
	}
	if !cluster.ConnectionSecretIncludesCertificate() {
		return secretState{v1alpha1.ReasonRunning, fmt.Sprintf(
			"The cluster is Active with %d node(s), and Secret %q holds its URLs. includeCertificate is false, so "+
				"the client certificate is not in it.", observed.NodeCount(), name)}
	}
	if len(storedCert) == 0 && certPending {
		return secretState{v1alpha1.ReasonWaitingForCertificate, fmt.Sprintf(
			"The cluster is Active and Secret %q holds its URLs. RavenDB Cloud has not issued the client "+
				"certificate yet; it is added on a later poll.", name)}
	}
	if len(storedCert) == 0 {
		return secretState{v1alpha1.ReasonUnexpectedResponse, fmt.Sprintf(
			"The cluster is Active and Secret %q holds its URLs, but no %s.", name, secretKeyTLSCert)}
	}
	return secretState{v1alpha1.ReasonRunning, fmt.Sprintf(
		"The cluster is Active with %d node(s), and Secret %q holds its URLs and client certificate.",
		observed.NodeCount(), name)}
}

// certificateNeeded reports whether the certificate has to be downloaded.
func certificateNeeded(existing *corev1.Secret) bool {
	if existing == nil {
		return true
	}
	if stored := existing.Data[secretKeyTLSCert]; len(stored) > 0 {
		return expiringSoon(stored)
	}
	return !bundleNotUnpacked(existing.Data)
}

// certificateRenewWindow is how close to expiry the published certificate may
// get before the operator fetches its replacement.
//
// RavenDB Cloud issues client certificates for five years and regenerates them
// thirty days before expiry, so with three weeks left the replacement has
// existed for over a week.
const certificateRenewWindow = 21 * 24 * time.Hour

// expiringSoon reports whether a stored certificate is close enough to expiry
// to be worth replacing.
//
// An unparseable certificate answers false on purpose: re-downloading on every
// poll would turn one bad Secret into a permanent request loop.
func expiringSoon(pemBytes []byte) bool {
	cert := parseCertificate(pemBytes)
	return cert != nil && time.Now().Add(certificateRenewWindow).After(cert.NotAfter)
}

// certificateExpiry reads NotAfter off a stored certificate, for status.
func certificateExpiry(pemBytes []byte) *metav1.Time {
	cert := parseCertificate(pemBytes)
	if cert == nil {
		return nil
	}
	t := metav1.NewTime(cert.NotAfter)
	return &t
}

// parseCertificate decodes the first PEM block as a certificate, or returns nil.
func parseCertificate(pemBytes []byte) *x509.Certificate {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}
