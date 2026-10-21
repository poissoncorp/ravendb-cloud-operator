package cloudapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
)

// DefaultEndpoint is the public RavenDB Cloud control plane.
const DefaultEndpoint = "https://api.cloud.ravendb.net"

// EndpointEnvVar names the environment variable the operator and the export
// tool read at start to point every client in the process at another control
// plane, such as internal/cloudsim. There is one implementation of Client, so
// this is how a run points it elsewhere.
const EndpointEnvVar = "RAVENDB_CLOUD_ENDPOINT"

// DefaultRateLimit paces outbound calls.
var DefaultRateLimit = rate.Every(1100 * time.Millisecond)

// maxResponseBytes caps any single response so a misbehaving endpoint cannot
// balloon the operator's memory. Certificate bundles are kilobytes.
const maxResponseBytes = 8 << 20

// maxDisplayRunes bounds what of a response reaches a person.
const maxDisplayRunes = 512

const (
	defaultMaxRetries     = 3
	defaultRetryBaseDelay = time.Second
	defaultUserAgent      = "ravendb-cloud-operator"
)

// attemptTimeout bounds one request, so a control plane that accepts the
// connection and never answers costs one attempt rather than a worker.
const attemptTimeout = 30 * time.Second

// sharedHTTPClient is behind every REST client. The API key travels in a
// header, never in transport state, so one connection pool serves every
// CloudAccount.
var sharedHTTPClient = &http.Client{Timeout: attemptTimeout}

// Options configures a REST client.
type Options struct {
	// Endpoint is the control plane URL. Empty means DefaultEndpoint.
	Endpoint string
	// APIKey is sent as the X-Api-Key header. Required.
	APIKey string
	// RateLimit caps outbound requests. Zero means DefaultRateLimit. Ignored
	// when Limiter is set.
	RateLimit rate.Limit
	// Limiter, when set, is shared with every other client built from it, so
	// every account in the process draws on one pace.
	Limiter *rate.Limiter
	// MaxRetries bounds retries of retryable failures. Zero means 3; negative
	// disables retrying. One attempt is always made.
	MaxRetries int
	// RetryBaseDelay is the first backoff step. Zero means 1s.
	RetryBaseDelay time.Duration
	// UserAgent identifies the operator to the control plane.
	UserAgent string
}

type restClient struct {
	endpoint   string
	apiKey     string
	limiter    *rate.Limiter
	maxRetries int
	baseDelay  time.Duration
	userAgent  string
}

// NewRESTClient returns a Client backed by the RavenDB Cloud HTTP API.
func NewRESTClient(opts Options) (Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("cloudapi: API key is required")
	}
	endpoint := cmp.Or(strings.TrimSpace(opts.Endpoint), DefaultEndpoint)
	if err := checkEndpoint(endpoint); err != nil {
		return nil, err
	}
	limiter := opts.Limiter
	if limiter == nil {
		limiter = rate.NewLimiter(cmp.Or(opts.RateLimit, DefaultRateLimit), 1)
	}
	return &restClient{
		endpoint:   strings.TrimRight(endpoint, "/"),
		apiKey:     opts.APIKey,
		limiter:    limiter,
		maxRetries: cmp.Or(opts.MaxRetries, defaultMaxRetries),
		baseDelay:  cmp.Or(opts.RetryBaseDelay, defaultRetryBaseDelay),
		userAgent:  cmp.Or(opts.UserAgent, defaultUserAgent),
	}, nil
}

// checkEndpoint insists on an absolute http or https URL. url.Parse alone
// accepts "api.cloud.ravendb.net" as a relative reference, which would fail
// only at the first request, as a transport error that IsRetryable retries on
// every call of every reconcile.
func checkEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("cloudapi: invalid endpoint %q: %w", endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("cloudapi: endpoint %q must be an absolute http or https URL", endpoint)
	}
	return nil
}

// do issues one request, retrying retryable failures under the rate limiter,
// and decodes the JSON answer into out when out is non-nil. The raw body is
// returned for the endpoint that does not answer JSON.
func (c *restClient) do(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	// From the context rather than a field: the controller's logger there
	// already names the object, and a test that passes none gets a discard.
	log := logr.FromContextOrDiscard(ctx)

	payload, err := encodeBody(path, body)
	if err != nil {
		return nil, err
	}

	attempts := max(c.maxRetries+1, 1)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			retries.WithLabelValues(method).Inc()
			if err := c.backOff(ctx, attempt-1); err != nil {
				return nil, &notSentError{Method: method, Path: path, Err: err}
			}
		}
		if err := c.waitForTurn(ctx, log, method, path); err != nil {
			return nil, &notSentError{Method: method, Path: path, Err: err}
		}

		start := time.Now()
		raw, err := c.attempt(ctx, method, path, payload)
		took := time.Since(start).Round(time.Millisecond)

		if err == nil {
			log.V(1).Info("cloud call",
				"method", method, "path", path, "took", took, "attempt", attempt)
			return raw, decode(log, method, path, raw, out)
		}
		lastErr = err

		status := statusOf(err)
		if !IsRetryable(err) || ctx.Err() != nil {
			// V(1), not Error: a 404 or a rejected spec is the cloud answering
			// correctly, and the controller reports it as a condition.
			log.V(1).Info("cloud refused the call and will not be retried",
				"method", method, "path", path, "status", status, "took", took,
				"attempt", attempt, "reason", err.Error())
			return nil, err
		}
		if method != http.MethodGet && MayHaveApplied(err) {
			log.V(1).Info("cloud call failed after it may have taken effect, so it is not sent again",
				"method", method, "path", path, "status", status, "took", took,
				"attempt", attempt, "reason", Describe(err, nil))
			return nil, err
		}
		// The reason as well as the status: a call that never got an answer
		// has status 0, which says nothing.
		log.V(1).Info("cloud call failed, retrying",
			"method", method, "path", path, "status", status, "took", took,
			"attempt", attempt, "of", attempts, "reason", Describe(err, nil))
	}
	return nil, fmt.Errorf("cloudapi: %s %s failed after %d attempt(s): %w", method, path, attempts, lastErr)
}

func encodeBody(path string, body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cloudapi: encoding request for %s: %w", path, err)
	}
	return payload, nil
}

// backOff waits before a retry, doubling each time. It comes on top of the
// limiter, so a 429 does not become a hot loop against an endpoint that is
// already complaining.
func (c *restClient) backOff(ctx context.Context, retry int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.baseDelay << (retry - 1)):
		return nil
	}
}

// waitForTurn blocks until the shared pace allows a call. Time queued here
// looks like the operator doing nothing, so a wait long enough to matter is
// logged, at V(1) because several workers share the pace.
func (c *restClient) waitForTurn(ctx context.Context, log logr.Logger, method, path string) error {
	start := time.Now()
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	waited := time.Since(start)
	queuedSeconds.Observe(waited.Seconds())
	if waited > time.Second {
		log.V(1).Info("waited on the rate limiter before calling the cloud",
			"method", method, "path", path, "waited", waited.Round(time.Millisecond))
	}
	return nil
}

func decode(log logr.Logger, method, path string, raw []byte, out any) error {
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		log.Error(err, "the cloud sent a body this client cannot decode",
			"method", method, "path", path, "bytes", len(raw))
		return &DecodeError{Path: path, Err: err}
	}
	return nil
}

func (c *restClient) attempt(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	req, err := c.newRequest(ctx, method, path, payload)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		observeAttempt(method, 0, time.Since(start))
		return nil, fmt.Errorf("cloudapi: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	observeAttempt(method, resp.StatusCode, time.Since(start))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Body:       forDisplay(string(raw), maxDisplayRunes),
			// From the whole body: the display copy is cut, and cut JSON does
			// not parse.
			Problems: problemsFromWire(raw),
		}
	}
	if readErr != nil {
		return nil, fmt.Errorf("cloudapi: reading response from %s: %w", path, readErr)
	}
	return raw, nil
}

func (c *restClient) newRequest(ctx context.Context, method, path string, payload []byte) (*http.Request, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// forDisplay makes a response body safe to put in front of a person reading
// kubectl describe in a terminal. The body is not necessarily from RavenDB
// Cloud: a proxy in between answers too.
//
// It drops control characters, which a terminal would act on. It replaces
// invalid UTF-8, and counts runes rather than bytes so its own cut makes none.
// And it bounds the length: the API server caps a condition message at 32 KiB,
// and a longer one fails the status write and wedges the object.
func forDisplay(s string, maxRunes int) string {
	var b strings.Builder
	runes := 0
	for _, r := range strings.TrimSpace(s) {
		if runes == maxRunes {
			b.WriteString("...")
			break
		}
		switch {
		case r == utf8.RuneError:
			// range yields RuneError for each invalid byte.
			b.WriteRune('�')
		case r == '\n' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// Dropped, not replaced: a placeholder per byte of an escape
			// sequence would bury the actual message.
		default:
			b.WriteRune(r)
		}
		runes++
	}
	return strings.TrimSpace(b.String())
}

// --- Metadata ---------------------------------------------------------------

func (c *restClient) GetAccountInfo(ctx context.Context) (*AccountInfo, error) {
	var w wireAccountInfo
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/account/info", nil, &w); err != nil {
		return nil, err
	}
	return &AccountInfo{DomainName: w.DomainName}, nil
}

func (c *restClient) ListRegions(ctx context.Context, provider Provider) ([]string, error) {
	var w wireRegions
	path := "/api/v1/metadata/regions/" + url.PathEscape(providerToWire(provider))
	if _, err := c.do(ctx, http.MethodGet, path, nil, &w); err != nil {
		return nil, err
	}
	return w.Names, nil
}

func (c *restClient) ListInstanceTypes(ctx context.Context, provider Provider, region string) ([]InstanceType, error) {
	var w wireInstanceTypes
	path := "/api/v1/metadata/instance-types/" + url.PathEscape(providerToWire(provider)) + "/" + url.PathEscape(region)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &w); err != nil {
		return nil, err
	}
	out := make([]InstanceType, 0, len(w.InstanceTypes))
	for _, it := range w.InstanceTypes {
		out = append(out, it.toDomain())
	}
	return out, nil
}

func (c *restClient) ListReleaseChannels(ctx context.Context) (*ReleaseChannels, error) {
	var w wireReleaseChannels
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/metadata/release-channels", nil, &w); err != nil {
		return nil, err
	}
	channels := make([]ReleaseChannel, 0, len(w.ReleaseChannels))
	for _, ch := range w.ReleaseChannels {
		// Field by field, not a conversion: the wire type must stay free to follow the API.
		channels = append(channels, ReleaseChannel{Name: ch.Name, RavenVersion: ch.RavenVersion, BuildType: ch.BuildType})
	}
	return &ReleaseChannels{Default: w.DefaultReleaseChannel, Channels: channels}, nil
}

// --- Lifecycle -------------------------------------------------------------

func (c *restClient) ListClusters(ctx context.Context) ([]ClusterRef, error) {
	var w wireProductList
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/products/list", nil, &w); err != nil {
		return nil, err
	}
	out := make([]ClusterRef, 0, len(w.Items))
	for _, it := range w.Items {
		// Field by field, as in ListReleaseChannels.
		out = append(out, ClusterRef{ID: it.ID, Name: it.Name})
	}
	return out, nil
}

func (c *restClient) FindClusterByName(ctx context.Context, displayName string) (*ClusterRef, error) {
	refs, err := c.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	live, err := c.dropTerminated(ctx, named(refs, displayName))
	if err != nil {
		return nil, err
	}
	return onlyOne(live, displayName)
}

// named keeps the clusters called displayName, ignoring case the way RavenDB
// Cloud does when it decides a name is taken: the check is a RavenDB query,
// and a query compares strings case-insensitively. An exact match would call "orders" free while
// "Orders" exists, and the create that followed would be refused.
func named(refs []ClusterRef, displayName string) []ClusterRef {
	var out []ClusterRef
	for _, r := range refs {
		if strings.EqualFold(r.Name, displayName) {
			out = append(out, r)
		}
	}
	return out
}

// dropTerminated removes clusters that RavenDB Cloud has already destroyed.
//
// A terminated product keeps its record and its display name, and the list
// endpoint filters only on hideInPortal, never on status, so without this a
// lookup could bind to a dead cluster. The list carries only id and name, so
// this costs a details call per match, which is why it runs on the name
// matches and not on every cluster in the account.
func (c *restClient) dropTerminated(ctx context.Context, refs []ClusterRef) ([]ClusterRef, error) {
	live := make([]ClusterRef, 0, len(refs))
	for _, r := range refs {
		cluster, err := c.GetCluster(ctx, r.ID)
		if IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if cluster.Status != StatusTerminated {
			live = append(live, r)
		}
	}
	return live, nil
}

// onlyOne answers the single cluster in refs: ErrNotFound for none, and
// ErrAmbiguous for more than one.
func onlyOne(refs []ClusterRef, displayName string) (*ClusterRef, error) {
	switch len(refs) {
	case 0:
		return nil, fmt.Errorf("cloudapi: no cluster named %q: %w", displayName, ErrNotFound)
	case 1:
		return &refs[0], nil
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	return nil, &AmbiguousNameError{Name: displayName, IDs: ids}
}

func (c *restClient) GetCluster(ctx context.Context, id string) (*Cluster, error) {
	if id == "" {
		return nil, fmt.Errorf("cloudapi: empty cluster id: %w", ErrNotFound)
	}
	var w wireProductDetails
	path := "/api/v1/products/details/" + url.PathEscape(id)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &w); err != nil {
		return nil, err
	}
	return w.toDomain(), nil
}

func (c *restClient) CreateCluster(ctx context.Context, req CreateClusterRequest) (string, error) {
	if err := ValidateCreate(req); err != nil {
		return "", err
	}
	body := wireProductCreateRequest{
		CloudProvider:    providerToWire(req.Provider),
		InstanceTypeName: req.InstanceType,
		DisplayName:      req.DisplayName,
		ReleaseChannel:   req.ReleaseChannel,
		SubdomainName:    req.SubdomainName,
		Tier:             string(req.Tier),
		Region:           req.Region,
		DiskSize:         req.Storage.SizeGiB,
		IOPS:             req.Storage.IOPS,
		Throughput:       req.Storage.ThroughputMBps,
		StorageTypeName:  string(req.Storage.Type),
		AllowedIPs:       req.AllowedIPs,
		DeploymentType:   string(req.DeploymentType),
	}
	var created wireProductCreated
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/products/create", body, &created); err != nil {
		return "", err
	}
	if created.ProductID != "" {
		return created.ProductID, nil
	}
	// The API answers 202 Accepted with the new id. Should a deployment ever
	// send none, find the cluster by name, so it is not orphaned unrecorded.
	ref, err := c.FindClusterByName(ctx, req.DisplayName)
	if err != nil {
		return "", fmt.Errorf("cloudapi: create returned no product id and lookup by name failed: %w", err)
	}
	return ref.ID, nil
}

// ValidateCreate rejects requests RavenDB Cloud is certain to refuse, so the
// failure lands on the CloudCluster with a readable message instead of a 400.
//
// It repeats rules the CRD already enforces, as defence in depth for a caller
// that did not come through the CRD, so do not remove them because CEL covers
// them. Do not grow it either: it sees only the request, and a rule needing
// regions, instance types or account state belongs in internal/controller.
func ValidateCreate(req CreateClusterRequest) error {
	switch {
	case req.DisplayName == "":
		return errors.New("cloudapi: displayName is required")
	case req.Region == "":
		return errors.New("cloudapi: region is required")
	case req.InstanceType == "":
		return errors.New("cloudapi: instanceType is required")
	case req.Tier == "":
		return errors.New("cloudapi: tier is required")
	case req.ReleaseChannel == "":
		return errors.New("cloudapi: releaseChannel is required")
	case req.Storage.SizeGiB <= 0:
		return errors.New("cloudapi: storage size must be greater than zero")
	case len(req.AllowedIPs) == 0:
		// Required on the wire, and an empty list is a cluster nothing can reach.
		return errors.New("cloudapi: at least one allowed IP range is required")
	case req.SubdomainName == "":
		// Nullable in the OpenAPI document, but RavenDB Cloud feeds it to a
		// domain-name validator that rejects an empty value.
		return errors.New("cloudapi: subdomainName is required")
	case !slices.Contains([]Provider{ProviderAWS, ProviderAzure, ProviderGCP}, req.Provider):
		// providerToWire passes an unknown value through, and cloudProvider
		// is an enum on the wire.
		return errors.New("cloudapi: provider must be AWS, Azure or GCP")
	case !slices.Contains([]StorageType{StorageSsdStandard, StorageSsdPremium}, req.Storage.Type):
		// storageTypeName is an enum on the wire too.
		return errors.New("cloudapi: storage type must be SsdStandard or SsdPremium")
	}
	return nil
}

func (c *restClient) TerminateCluster(ctx context.Context, id string) error {
	path := "/api/v1/products/terminate/" + url.PathEscape(id)
	// A terminated product keeps its display name until it is hidden, and
	// RavenDB Cloud hides it once termination completes when asked here. So a
	// manifest deleted with Terminate and applied again gets its name back
	// instead of a refused create.
	_, err := c.do(ctx, http.MethodPost, path, wireTerminateRequest{HideInPortal: true}, nil)
	if IsNotFound(err) || statusOf(err) == http.StatusGone {
		return nil
	}
	return err
}

func (c *restClient) ChangeInstanceType(ctx context.Context, id, instanceType string) error {
	body := wireChangeInstanceTypeRequest{ProductID: id, InstanceType: instanceType}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/products/instance-type", body, nil)
	return busyRefusal(err)
}

func (c *restClient) ChangeStorage(ctx context.Context, id string, change StorageChange) error {
	body := wireChangeStorageRequest{
		ProductID:   id,
		StorageType: string(change.Type),
		Size:        change.SizeGiB,
		IOPS:        change.IOPS,
		Throughput:  change.ThroughputMBps,
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/products/storage", body, nil)
	return busyRefusal(storageRefusal(err))
}

// storageChangeRateText is from the refusal RavenDB Cloud gives a storage
// change that comes too soon after the previous one: "You've reached the
// maximum storage modification rate. Wait at least 6 hours between each
// storage change. The next storage change will be available in ...".
const storageChangeRateText = "maximum storage modification rate"

// storageRefusal marks the one storage refusal that lifts on its own.
//
// RavenDB Cloud allows one storage change per cluster every 6h05m on AWS and
// 12h05m on Azure SsdPremium, counted from when the previous one finished. A
// change inside that window is a 400, and its text is all that tells it apart
// from a refusal that never lifts. It is matched here because
// the controller must never read a message. If RavenDB Cloud rewords it, the
// refusal degrades to an ordinary terminal one.
func storageRefusal(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return err
	}
	for _, problem := range apiErr.Problems {
		if strings.Contains(problem.Message, storageChangeRateText) {
			return fmt.Errorf("%w: %w", ErrTooSoon, err)
		}
	}
	return err
}

func (c *restClient) AddNode(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/products/additional-node/add", wireNodeRequest{ProductID: id}, nil)
	return busyRefusal(err)
}

func (c *restClient) RemoveNode(ctx context.Context, id, nodeTag string) error {
	body := wireNodeRequest{ProductID: id, NodeTag: nodeTag}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/products/additional-node/remove", body, nil)
	return busyRefusal(err)
}

func (c *restClient) RestartNode(ctx context.Context, id, nodeTag string) error {
	body := wireNodeRequest{ProductID: id, NodeTag: nodeTag}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/products/restart-node", body, nil)
	return busyRefusal(err)
}

// busyTexts are the refusals RavenDB Cloud gives a change on a cluster that is
// not Active: "Product must be Active" for a node added or removed, "the
// product is not currently active (status: ...)" for a restart, and "The
// current product status does not allow for storage modifications". Matched
// here because the controller must never read a message. If RavenDB Cloud
// rewords one, that refusal degrades to an ordinary terminal one.
var busyTexts = []string{"must be active", "not currently active", "status does not allow"}

// busyRefusal marks a refusal that lifts once the cluster is Active again.
func busyRefusal(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return err
	}
	for _, problem := range apiErr.Problems {
		message := strings.ToLower(problem.Message)
		for _, text := range busyTexts {
			if strings.Contains(message, text) {
				return fmt.Errorf("%w: %w", ErrBusy, err)
			}
		}
	}
	return err
}

// --- Connectivity ----------------------------------------------------------

func (c *restClient) GetClientCertificate(ctx context.Context, id string) (*ClientCertificate, error) {
	path := "/api/v1/products/security/certificate/" + url.PathEscape(id)
	// application/octet-stream, so out is nil and the body is taken verbatim.
	// The filename the server suggests decides nothing: the archive is read by
	// looking inside it.
	raw, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("cloudapi: empty certificate payload for cluster %s", id)
	}
	return &ClientCertificate{Data: raw, RetrievedAt: time.Now()}, nil
}
