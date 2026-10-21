// Package cloudsim is a stand-in for the RavenDB Cloud control plane, and the
// operator's only test double. The labs serve it over HTTP (labs/mockcloud) and
// the Go tests start it in process on an httptest server. Either way the
// operator reaches it with the real REST client over real HTTP, so there is no
// second implementation of cloudapi.Client that could drift from the first.
//
// The wire structs in api.go are a separate reading of the API from the
// operator's own in internal/cloudapi/wire.go. Sharing one set would make a
// shared misreading invisible; keeping two is what lets contract_test.go catch
// it.
//
// On top of the API it serves a /_lab control surface for what a control plane
// cannot be asked to do on purpose: delete a cluster behind the operator's
// back, or edit one in the portal. Go tests reach the same levers through Sim
// and State, and can also fail the next call to an operation.
package cloudsim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Options configures a simulator. Zero durations and no throttle are what unit
// tests want; the labs set all of them.
type Options struct {
	// APIKey is what callers must present in X-Api-Key.
	APIKey string
	// Domain is the account DNS domain.
	Domain string
	// ProvisionDuration is how long a create stays in Creating, and
	// ChangeDuration how long a change stays in DeployingChanges. Zero means
	// the transition is over by the next read.
	ProvisionDuration time.Duration
	ChangeDuration    time.Duration
	// Throttle answers 429 to a call that comes too soon after the previous
	// one to the same route.
	Throttle bool
	// ProductQuota is how many paid products the account may hold before a
	// create is refused with 412. Zero means no limit, which is what the labs
	// need: they hold more than RavenDB Cloud's default of three at once.
	ProductQuota int
	// CertPassword protects the .pfx in the certificate bundle.
	CertPassword string
	// Logf receives a line per state change, which the labs print next to the
	// operator's log. Tests leave it nil.
	Logf func(format string, args ...any)
}

func (o Options) withDefaults() Options {
	if o.APIKey == "" {
		o.APIKey = "lab-api-key"
	}
	if o.Domain == "" {
		o.Domain = "acme.ravendb.cloud"
	}
	if o.CertPassword == "" {
		o.CertPassword = "lab-pfx-password"
	}
	return o
}

// Sim is one simulated control plane: a world of clusters plus the HTTP
// surface that serves it.
type Sim struct {
	state        *State
	apiKey       string
	domain       string
	certPassword string
	throttle     bool
	log          func(format string, args ...any)

	mu       sync.Mutex
	lastCall map[string]time.Time
	calls    map[string][]Call
}

// New returns a simulator with an empty world. Seed it through State.
func New(opts Options) *Sim {
	opts = opts.withDefaults()
	state := newState(opts.ProvisionDuration, opts.ChangeDuration)
	state.productQuota = opts.ProductQuota
	return &Sim{
		state:        state,
		apiKey:       opts.APIKey,
		domain:       opts.Domain,
		certPassword: opts.CertPassword,
		throttle:     opts.Throttle,
		lastCall:     map[string]time.Time{},
		calls:        map[string][]Call{},
		log:          opts.Logf,
	}
}

func (s *Sim) logf(format string, args ...any) {
	if s.log != nil {
		s.log(format, args...)
	}
}

// Handler is the simulator as an http.Handler.
func (s *Sim) Handler() http.Handler { return s.routes() }

// State is the simulated world, for a test to seed, edit or empty behind the
// operator's back.
func (s *Sim) State() *State { return s.state }

// Call is one request the simulator answered. Body is the raw payload, so a
// test asserts on the bytes the operator sent rather than on this package's
// decoding of them.
type Call struct {
	Method string
	Path   string
	Body   []byte
	Status int
}

// Calls returns every request answered for one operation, in order. Operations
// are named after the cloudapi.Client methods they serve, so a test can count
// CreateCluster calls without knowing the route.
func (s *Sim) Calls(operation string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls[operation]...)
}

// CallCount is the number of requests answered for one operation.
func (s *Sim) CallCount(operation string) int { return len(s.Calls(operation)) }

// FailNext makes the next call to operation answer status without doing
// anything.
func (s *Sim) FailNext(operation string, status int) {
	s.state.FailNext(operation, status)
}

func (s *Sim) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/account/info", s.api("GetAccountInfo", s.accountInfo))
	mux.HandleFunc("GET /api/v1/metadata/regions/{provider}", s.api("ListRegions", s.regions))
	mux.HandleFunc("GET /api/v1/metadata/instance-types/{provider}/{region}", s.api("ListInstanceTypes", s.instanceTypes))
	mux.HandleFunc("GET /api/v1/metadata/release-channels", s.api("ListReleaseChannels", s.channels))
	mux.HandleFunc("GET /api/v1/products/list", s.api("ListClusters", s.listProducts))
	mux.HandleFunc("POST /api/v1/products/create", s.api("CreateCluster", s.createProduct))
	mux.HandleFunc("GET /api/v1/products/details/{id}", s.api("GetCluster", s.productDetails))
	mux.HandleFunc("GET /api/v1/products/security/certificate/{id}", s.api("GetClientCertificate", s.certificate))
	mux.HandleFunc("POST /api/v1/products/storage", s.api("ChangeStorage", decoded(s.changeStorage)))
	mux.HandleFunc("POST /api/v1/products/instance-type", s.api("ChangeInstanceType", decoded(s.changeInstanceType)))
	mux.HandleFunc("POST /api/v1/products/terminate/{id}", s.api("TerminateCluster", s.terminate))
	mux.HandleFunc("POST /api/v1/products/additional-node/add", s.api("AddNode", decoded(s.addNode)))
	mux.HandleFunc("POST /api/v1/products/restart-node", s.api("RestartNode", decoded(s.restartNode)))
	mux.HandleFunc("POST /api/v1/products/additional-node/remove", s.api("RemoveNode", decoded(s.removeNode)))

	// Unauthenticated on purpose: this is not part of the API being simulated.
	mux.HandleFunc("GET /_lab/clusters", s.labListClusters)
	mux.HandleFunc("POST /_lab/clusters", s.labSeedCluster)
	mux.HandleFunc("POST /_lab/clusters/{id}/vanish", s.labVanish)
	mux.HandleFunc("POST /_lab/clusters/{id}/patch", s.labPatch)
	mux.HandleFunc("GET /_lab/requests", s.labRequests)
	mux.HandleFunc("POST /_lab/requests/reset", s.labResetRequests)
	mux.HandleFunc("GET /_lab/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}

// endpoint serves one RavenDB Cloud route and returns the status it wrote, so
// api can record it.
type endpoint func(http.ResponseWriter, *http.Request) int

// api wraps an endpoint in what every real route shares, and records the call
// under operation, the cloudapi.Client method the route serves.
func (s *Sim) api(operation string, h endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := bufferBody(r)
		status := s.serve(w, r, operation, h)
		s.record(operation, r, body, status)
	}
}

func (s *Sim) serve(w http.ResponseWriter, r *http.Request, operation string, h endpoint) int {
	// Sent on every response, not only on a 429. The values are static: a lab
	// only needs to see that the headers exist.
	w.Header().Set("X-RateLimit-Limit", "1")
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", "1")

	// Bare, as prod answers a rejected key: a 401 with no body.
	if r.Header.Get("X-Api-Key") != s.apiKey {
		return writeStatus(w, http.StatusUnauthorized)
	}

	key := routeKey(r)
	// One bucket per verb and concrete path, and no Retry-After.
	if s.throttle && s.tooSoon(key) {
		return writeStatus(w, http.StatusTooManyRequests)
	}
	if status, ok := s.state.takeFailure(operation); ok {
		return writeInjectedFailure(w, status)
	}
	return h(w, r)
}

// maxRequestBody caps how much of a request body is recorded and decoded.
const maxRequestBody = 1 << 20

// bufferBody reads the request body so the recorded call can carry it, and
// puts it back for the endpoint to decode.
func bufferBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

func (s *Sim) record(operation string, r *http.Request, body []byte, status int) {
	s.state.recordRequest(r.Method, r.URL.Path, r.Header.Get("User-Agent"), s.productFor(r, body), status)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[operation] = append(s.calls[operation], Call{
		Method: r.Method, Path: r.URL.Path, Body: body, Status: status,
	})
}

// productFor names the cluster a call concerns, so the request log can be read
// per lab. Account and metadata calls belong to no cluster and stay
// unattributed.
func (s *Sim) productFor(r *http.Request, body []byte) string {
	if id := r.PathValue("id"); id != "" {
		return s.state.ProductName(id)
	}
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		ProductID   string `json:"productId"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if payload.ProductID != "" {
		return s.state.ProductName(payload.ProductID)
	}
	// A create has no id yet, so its own display name is the attribution.
	return payload.DisplayName
}

// routeKey is the concrete "METHOD /path" rather than the route pattern, so the
// throttle keeps one bucket per path.
func routeKey(r *http.Request) string {
	return r.Method + " " + r.URL.Path
}

func (s *Sim) tooSoon(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if last, ok := s.lastCall[key]; ok && now.Sub(last) < time.Second {
		return true
	}
	s.lastCall[key] = now
	return false
}

// --- RavenDB Cloud API endpoints -------------------------------------------

func (s *Sim) accountInfo(w http.ResponseWriter, _ *http.Request) int {
	return writeJSON(w, http.StatusOK, accountInfoResponse{DomainName: s.domain})
}

func (s *Sim) regions(w http.ResponseWriter, r *http.Request) int {
	provider := r.PathValue("provider")
	names, ok := regionsByProvider[provider]
	if !ok {
		return writeProblem(w, http.StatusBadRequest,
			fmt.Sprintf("unknown cloud provider %q; expected Aws, Azure or Gcp", provider))
	}
	return writeJSON(w, http.StatusOK, regionsResponse{Names: names})
}

func (s *Sim) instanceTypes(w http.ResponseWriter, r *http.Request) int {
	provider, region := r.PathValue("provider"), r.PathValue("region")
	if !regionExists(provider, region) {
		return writeProblem(w, http.StatusNotFound,
			fmt.Sprintf("region %q is not available for %s", region, provider))
	}
	return writeJSON(w, http.StatusOK, instanceTypesResponse{InstanceTypes: instanceTypes})
}

func (s *Sim) channels(w http.ResponseWriter, _ *http.Request) int {
	return writeJSON(w, http.StatusOK, releaseChannels)
}

func (s *Sim) listProducts(w http.ResponseWriter, _ *http.Request) int {
	clusters := s.state.List()
	items := make([]productListItem, 0, len(clusters))
	for _, c := range clusters {
		items = append(items, productListItem{ID: c.ID, Name: c.DisplayName})
	}
	return writeJSON(w, http.StatusOK, productListResponse{Items: items})
}

func (s *Sim) createProduct(w http.ResponseWriter, r *http.Request) int {
	var req productCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return writeProblem(w, http.StatusBadRequest, "malformed request body: "+err.Error())
	}
	id, err := s.state.Create(req)
	var quota quotaError
	switch {
	case errors.As(err, &quota):
		return writeProblem(w, http.StatusPreconditionFailed, err.Error())
	case err != nil:
		return writeRefusal(w, http.StatusBadRequest, err)
	}
	s.logf("created %s (%s, %s/%s, %s)", id, req.DisplayName, req.CloudProvider, req.Region, req.InstanceTypeName)
	return writeJSON(w, http.StatusAccepted, productCreatedResponse{ProductID: id})
}

func (s *Sim) productDetails(w http.ResponseWriter, r *http.Request) int {
	c, ok := s.state.Get(r.PathValue("id"))
	if !ok {
		return writeNoSuchProduct(w)
	}
	return writeJSON(w, http.StatusOK, s.details(c))
}

func (s *Sim) details(c *Cluster) productDetailsResponse {
	// An instance type patched in through /_lab may be off the catalogue, and
	// then reports zero hardware.
	spec, _ := findInstanceType(c.Provider, c.Region, c.InstanceType)
	return productDetailsResponse{
		ID:             c.ID,
		SubdomainName:  c.SubdomainName,
		DisplayName:    c.DisplayName,
		InstanceType:   c.InstanceType,
		CloudProvider:  c.Provider,
		Status:         c.Status,
		Tier:           c.Tier,
		Region:         c.Region,
		DNS:            c.dns(s.domain),
		NodeTags:       c.NodeTags,
		ReleaseChannel: c.ReleaseChannel,
		PaymentPlan:    paymentPlanResponse{ContractTerm: "OnDemand", PaymentOption: "Monthly"},
		HardwareInfo: hardwareInfoResponse{
			CPUPriority: spec.Parameters.CPUPriority,
			CPUs:        spec.Parameters.VirtualCPUs,
			RAM:         spec.Parameters.RAM,
			Storage: storageResponse{
				Size:               c.DiskSizeGiB,
				Iops:               c.Iops,
				Throughput:         c.Throughput,
				Type:               c.StorageType,
				AvailableDiskSizes: resizableTo(spec.Parameters.AvailableDiskSizes, c.DiskSizeGiB),
			},
		},
		Security: securityResponse{AllowedIps: c.AllowedIps},
	}
}

func (s *Sim) certificate(w http.ResponseWriter, r *http.Request) int {
	c, ok := s.state.Get(r.PathValue("id"))
	if !ok {
		return writeNoSuchProduct(w)
	}
	bundle, err := certificateZip(c.host(s.domain), s.certPassword)
	if err != nil {
		return writeProblem(w, http.StatusInternalServerError, err.Error())
	}
	filename := fmt.Sprintf("%s.%s.client.certificate.zip", c.SubdomainName, s.domain)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bundle)
	return http.StatusOK
}

func (s *Sim) changeStorage(w http.ResponseWriter, req changeStorageRequest) int {
	return s.answerChange(w, s.state.ChangeStorage(req.ProductID, req),
		fmt.Sprintf("storage of %s -> %d GiB %s", req.ProductID, req.Size, req.StorageType))
}

func (s *Sim) changeInstanceType(w http.ResponseWriter, req changeInstanceTypeRequest) int {
	return s.answerChange(w, s.state.ChangeInstanceType(req.ProductID, req.InstanceType),
		fmt.Sprintf("instance type of %s -> %s", req.ProductID, req.InstanceType))
}

func (s *Sim) addNode(w http.ResponseWriter, req nodeRequest) int {
	return s.answerChange(w, s.state.AddNode(req.ProductID), "add node to "+req.ProductID)
}

func (s *Sim) removeNode(w http.ResponseWriter, req nodeRequest) int {
	return s.answerChange(w, s.state.RemoveNode(req.ProductID, req.NodeTag),
		fmt.Sprintf("remove node %s from %s", req.NodeTag, req.ProductID))
}

func (s *Sim) restartNode(w http.ResponseWriter, req nodeRequest) int {
	return s.answerChange(w, s.state.RestartNode(req.ProductID, req.NodeTag),
		fmt.Sprintf("restart node %s of %s", req.NodeTag, req.ProductID))
}

func (s *Sim) terminate(w http.ResponseWriter, r *http.Request) int {
	id := r.PathValue("id")
	var req terminateRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // the body is optional
	switch err := s.state.Terminate(id, req.HideInPortal); {
	case errors.Is(err, errNotFound):
		return writeNoSuchProduct(w)
	case errors.Is(err, errAlreadyTerminated):
		// Gone, and forever: prod answers it for any product already
		// terminated, and the record never disappears. The operator's client treats Gone as success, which
		// is what makes a repeated delete idempotent.
		return writeProblem(w, http.StatusGone, err.Error())
	case err != nil:
		return writeProblem(w, http.StatusBadRequest, err.Error())
	}
	s.logf("terminating %s (hideInPortal=%v)", id, req.HideInPortal)
	return writeStatus(w, http.StatusAccepted)
}

// answerChange answers a day-two request the world has accepted or refused.
func (s *Sim) answerChange(w http.ResponseWriter, err error, what string) int {
	switch {
	case errors.Is(err, errNotFound):
		return writeNoSuchProduct(w)
	case err != nil:
		return writeProblem(w, http.StatusBadRequest, err.Error())
	}
	s.logf("accepted change: %s", what)
	return writeStatus(w, http.StatusAccepted)
}

// --- lab control surface ---------------------------------------------------

func (s *Sim) labListClusters(w http.ResponseWriter, r *http.Request) {
	// Scoped like the request log, so a lab counts its own clusters and not
	// whatever a concurrent lab is midway through creating.
	lab := r.URL.Query().Get("lab")
	clusters := s.state.List()
	out := make([]productDetailsResponse, 0, len(clusters))
	for _, c := range clusters {
		if lab != "" && !strings.HasPrefix(c.DisplayName, lab) {
			continue
		}
		out = append(out, s.details(c))
	}
	_ = writeJSON(w, http.StatusOK, out)
}

func (s *Sim) labSeedCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName  string `json:"displayName"`
		Tier         string `json:"tier"`
		InstanceType string `json:"instanceType"`
		Region       string `json:"region"`
		Provider     string `json:"provider"`
		Count        int    `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.DisplayName == "" {
		_ = writeProblem(w, http.StatusBadRequest, "displayName is required")
		return
	}
	spec := SeedSpec{
		DisplayName:  req.DisplayName,
		Tier:         req.Tier,
		InstanceType: req.InstanceType,
		Region:       req.Region,
		Provider:     req.Provider,
	}
	count := max(req.Count, 1)
	created := make([]string, 0, count)
	for range count {
		c := s.state.Seed(spec)
		created = append(created, c.ID)
		s.logf("lab: seeded %q as %s", req.DisplayName, c.ID)
	}
	_ = writeJSON(w, http.StatusOK, map[string]any{"ids": created})
}

func (s *Sim) labVanish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.state.Vanish(id) {
		_ = writeNoSuchProduct(w)
		return
	}
	s.logf("lab: %s vanished without terminating, as if deleted in the portal", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Sim) labPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstanceType string   `json:"instanceType"`
		DiskSizeGiB  int32    `json:"diskSizeGiB"`
		AllowedIps   []string `json:"allowedIps"`
		Status       string   `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		_ = writeProblem(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if !s.state.Patch(id, req.InstanceType, req.DiskSizeGiB, req.AllowedIps, req.Status) {
		_ = writeNoSuchProduct(w)
		return
	}
	s.logf("lab: patched %s out of band (%+v)", id, req)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Sim) labRequests(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	entries := s.state.Requests()
	// A lab keeps the calls that belong to no cluster too. Account and
	// metadata traffic is shared by the one CloudAccount, so hiding it would
	// make a lab that measures it read zero.
	if lab := query.Get("lab"); lab != "" {
		entries = slices.DeleteFunc(entries, func(e RequestEntry) bool {
			return e.Product != "" && !strings.HasPrefix(e.Product, lab)
		})
	}
	if path := query.Get("path"); path != "" {
		entries = slices.DeleteFunc(entries, func(e RequestEntry) bool {
			return !strings.Contains(e.Path, path)
		})
	}
	_ = writeJSON(w, http.StatusOK, entries)
}

func (s *Sim) labResetRequests(w http.ResponseWriter, r *http.Request) {
	s.state.ResetRequestsFor(r.URL.Query().Get("lab"))
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---------------------------------------------------------------

// decoded adapts a handler of one JSON request type into an endpoint, answering
// 400 for a body that does not decode.
func decoded[T any](h func(http.ResponseWriter, T) int) endpoint {
	return func(w http.ResponseWriter, r *http.Request) int {
		var req T
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return writeProblem(w, http.StatusBadRequest, err.Error())
		}
		return h(w, req)
	}
}

func writeStatus(w http.ResponseWriter, status int) int {
	w.WriteHeader(status)
	return status
}

// writeJSON drops the encode error: the bodies are this package's own structs,
// so Encode fails only on a client that has stopped listening.
func writeJSON(w http.ResponseWriter, status int, body any) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
	return status
}

// writeProblem answers a refusal of the request as a whole, in the shape
// RavenDB Cloud uses: ASP.NET's SerializableError, a field name mapped to a
// list of messages, under "generalError".
func writeProblem(w http.ResponseWriter, status int, message string) int {
	return writeJSON(w, status, map[string][]string{"generalError": {message}})
}

// writeRefusal answers err as a refusal, under its field when it names one.
func writeRefusal(w http.ResponseWriter, status int, err error) int {
	var field fieldError
	if errors.As(err, &field) {
		return writeJSON(w, status, map[string][]string{field.field: {field.message}})
	}
	return writeProblem(w, status, err.Error())
}

// writeNoSuchProduct answers as RavenDB Cloud does for an unknown product: a
// bare 404, so nothing here can lean on words production never sends.
func writeNoSuchProduct(w http.ResponseWriter) int {
	return writeStatus(w, http.StatusNotFound)
}

// writeInjectedFailure sends a body only where RavenDB Cloud sends one: its
// validators and its quota check answer in words (400, 412), while a rejected
// key, an unknown product and a throttled call do not. A body where prod has
// none would let a message pass here that reads as nothing in production.
func writeInjectedFailure(w http.ResponseWriter, status int) int {
	if status == http.StatusBadRequest || status == http.StatusPreconditionFailed {
		return writeProblem(w, status, "failure injected by a test")
	}
	return writeStatus(w, status)
}

// resizableTo is the per-product disk allow-list the details endpoint reports:
// the current size and up, not the whole create-time menu.
func resizableTo(menu []int32, current int32) []int32 {
	out := make([]int32, 0, len(menu))
	for _, size := range menu {
		if size >= current {
			out = append(out, size)
		}
	}
	return out
}
