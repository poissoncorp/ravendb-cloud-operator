package cloudsim

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Cluster is one simulated RavenDB Cloud product.
type Cluster struct {
	ID             string
	DisplayName    string
	SubdomainName  string
	Provider       string
	Region         string
	Tier           string
	InstanceType   string
	ReleaseChannel string
	Status         string
	NodeTags       []string
	AllowedIps     []string
	DiskSizeGiB    int32
	StorageType    string
	Iops           *int32
	Throughput     *float64

	// HiddenInPortal is set by a terminate that asked for it, and is the only
	// thing that takes a product out of the list: prod filters the list on it
	// alone, never on status.
	HiddenInPortal bool

	// storageChangedAt starts RavenDB Cloud's storage change window.
	storageChangedAt time.Time

	// settleAt is when a transitional status ends. Real provisioning takes
	// minutes; the simulator compresses that but keeps the transition, because
	// a cluster that is Active at once would hide every polling path the
	// operator has.
	settleAt time.Time
}

// State is the simulated world.
type State struct {
	mu sync.Mutex

	provisionDuration time.Duration
	changeDuration    time.Duration

	clusters map[string]*Cluster
	nextID   int

	// productQuota is how many paid products the account may hold; zero means
	// no limit.
	productQuota int

	requestLog []RequestEntry

	// failNext maps "METHOD /path" or an operation name to a status to answer
	// once.
	failNext map[string]int

	// offset is added to the wall clock, so a test can step time forward
	// instead of sleeping.
	offset time.Duration
}

func newState(provisionDuration, changeDuration time.Duration) *State {
	return &State{
		provisionDuration: provisionDuration,
		changeDuration:    changeDuration,
		clusters:          map[string]*Cluster{},
		failNext:          map[string]int{},
	}
}

// RequestEntry is one recorded API call.
type RequestEntry struct {
	At          time.Time `json:"at"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Status      int       `json:"status"`
	UserAgent   string    `json:"userAgent"`
	SincePrevMs int64     `json:"sincePrevMs"`
	// Product is the display name of the cluster the call concerns, empty for
	// account and metadata calls. It lets labs share one simulator and still
	// measure only their own traffic.
	Product string `json:"product,omitempty"`
}

// now is the wall clock plus whatever a test has advanced it by. The caller
// holds mu.
func (s *State) now() time.Time { return time.Now().Add(s.offset) }

// Advance moves the clock forward, settling every transition the step passes,
// so a test can give provisioning a realistic duration and jump over it.
func (s *State) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset += d
	s.settle()
}

// SetProvisionDuration changes how long a create stays in Creating.
func (s *State) SetProvisionDuration(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provisionDuration = d
}

// SetChangeDuration changes how long a change stays in DeployingChanges.
func (s *State) SetChangeDuration(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changeDuration = d
}

// settle ends every transition whose deadline has passed. It runs on every
// read, so time drives the state machine without a background loop. The
// caller holds mu.
func (s *State) settle() {
	now := s.now()
	for _, c := range s.clusters {
		if c.settleAt.IsZero() || now.Before(c.settleAt) {
			continue
		}
		switch c.Status {
		case statusCreating, statusDeployingChanges:
			c.Status = statusActive
		case statusTerminating:
			// The record survives, display name and all: prod sets the status
			// and keeps the product.
			c.Status = statusTerminated
		}
		c.settleAt = time.Time{}
	}
}

// find settles the world and returns the live cluster. The caller holds mu.
func (s *State) find(id string) (*Cluster, error) {
	s.settle()
	c, ok := s.clusters[id]
	if !ok {
		return nil, errNotFound
	}
	return c, nil
}

func (s *State) newID() string {
	s.nextID++
	return fmt.Sprintf("%d-A", s.nextID)
}

const (
	statusCreating         = "Creating"
	statusActive           = "Active"
	statusDeployingChanges = "DeployingChanges"
	statusTerminating      = "Terminating"
	statusTerminated       = "Terminated"
)

const (
	tierFree        = "Free"
	tierDevelopment = "Development"
	tierProduction  = "Production"
)

var (
	errNotFound          = errors.New("not found")
	errAlreadyTerminated = errors.New("product already terminated")
)

// fieldError is a refusal about one request field, which RavenDB Cloud sends
// under that field's name rather than under generalError.
type fieldError struct {
	field, message string
}

func (e fieldError) Error() string { return e.field + ": " + e.message }

// Create provisions a cluster and returns its id, or refuses the request the
// way RavenDB Cloud would.
func (s *State) Create(req productCreateRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settle()

	// Before the request itself is looked at, and whatever tier it asks for:
	// RavenDB Cloud checks the quota first.
	if s.productQuota > 0 && s.livePaidProducts() >= s.productQuota {
		return "", quotaError{quota: s.productQuota}
	}
	if err := s.checkCreate(req); err != nil {
		return "", err
	}
	spec, _ := findInstanceType(req.CloudProvider, req.Region, req.InstanceTypeName) // checkCreate refused an unknown one

	id := s.newID()
	s.clusters[id] = &Cluster{
		ID:             id,
		DisplayName:    req.DisplayName,
		SubdomainName:  req.SubdomainName,
		Provider:       req.CloudProvider,
		Region:         req.Region,
		Tier:           req.Tier,
		InstanceType:   req.InstanceTypeName,
		ReleaseChannel: req.ReleaseChannel,
		Status:         statusCreating,
		NodeTags:       nodeTags(spec.Parameters.NumberOfNodes),
		AllowedIps:     req.AllowedIps,
		DiskSizeGiB:    req.DiskSize,
		StorageType:    req.StorageTypeName,
		Iops:           req.Iops,
		Throughput:     req.Throughput,
		settleAt:       s.now().Add(s.provisionDuration),
	}
	return id, nil
}

// checkCreate applies the rules RavenDB Cloud applies to a create. The caller
// holds mu.
func (s *State) checkCreate(req productCreateRequest) error {
	// The free product is a portal-only offer, and the public API refuses it
	// before checking anything else. Seed is not gated: a Free
	// product created in the portal is legitimate.
	if req.Tier == tierFree {
		return errors.New("creating Free product via API is not allowed")
	}
	if req.DisplayName == "" {
		return errors.New("displayName is required")
	}
	if len(req.AllowedIps) == 0 {
		return errors.New("allowedIps must contain at least one entry")
	}
	if err := checkAllowedIps(req.CloudProvider, req.AllowedIps); err != nil {
		return err
	}
	if s.displayNameTaken(req.DisplayName) {
		return fieldError{field: "displayName", message: "Provided Display Name is already taken."}
	}
	if err := validateSubdomain(req.SubdomainName); err != nil {
		return err
	}
	if s.subdomainTaken(req.SubdomainName) {
		return errors.New("the selected subdomain name is taken")
	}
	spec, err := findInstanceTypeForTier(req.CloudProvider, req.Region, req.InstanceTypeName, req.Tier)
	if err != nil {
		return err
	}
	if !slices.Contains(spec.Parameters.AvailableDiskSizes, req.DiskSize) {
		return fmt.Errorf("disk size %d is not offered for %s; available: %v",
			req.DiskSize, req.InstanceTypeName, spec.Parameters.AvailableDiskSizes)
	}
	return validateIops(spec.Parameters.AvailableStorageTypes, req.StorageTypeName, req.Iops)
}

// displayNameTaken looks at every product not hidden in the portal, terminated
// ones included, and ignores case the way a RavenDB query does.
func (s *State) displayNameTaken(name string) bool {
	for _, c := range s.clusters {
		if !c.HiddenInPortal && strings.EqualFold(c.DisplayName, name) {
			return true
		}
	}
	return false
}

// checkAllowedIps refuses what RavenDB Cloud refuses in a list of allowed IPs:
// an entry that is not CIDR, two that are one network once normalised, and on
// Azure two that overlap.
func checkAllowedIps(provider string, ips []string) error {
	networks := make([]*net.IPNet, 0, len(ips))
	for _, ip := range ips {
		_, network, err := net.ParseCIDR(ip)
		if err != nil {
			return fieldError{field: "allowedIps", message: fmt.Sprintf("Invalid CIDR IP block '%s' provided.", ip)}
		}
		networks = append(networks, network)
	}
	for i, a := range networks {
		for _, b := range networks[i+1:] {
			if a.String() == b.String() {
				return fieldError{field: "allowedIps", message: "The list of allowed IPs cannot contain duplicate entries."}
			}
			if provider == "Azure" && (a.Contains(b.IP) || b.Contains(a.IP)) {
				return fieldError{field: "allowedIps", message: "In Azure, specifying overlapping IP ranges is not allowed. " +
					"Tip: 0.0.0.0/0 cannot be specified along with other IP ranges."}
			}
		}
	}
	return nil
}

// SetProductQuota limits how many paid products the account may hold, as
// RavenDB Cloud does, three by default there. Zero means no limit.
func (s *State) SetProductQuota(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.productQuota = n
}

// livePaidProducts counts what the quota counts: every product not Terminated
// and not Free, Terminating ones included. Call it with mu held.
func (s *State) livePaidProducts() int {
	n := 0
	for _, c := range s.clusters {
		if c.Status != statusTerminated && c.Tier != tierFree {
			n++
		}
	}
	return n
}

// quotaError is the refusal of a create once the account is at its quota.
type quotaError struct{ quota int }

func (e quotaError) Error() string {
	return fmt.Sprintf("You have reached the account API quota of %d products. "+
		"You can raise the quota by submitting a support ticket in the RavenDB Cloud Portal.", e.quota)
}

// subdomainTaken skips terminated products, which give their subdomain up:
// RavenDB Cloud counts a subdomain as taken by any product not Terminated.
func (s *State) subdomainTaken(name string) bool {
	for _, c := range s.clusters {
		if c.Status != statusTerminated && strings.EqualFold(c.SubdomainName, name) {
			return true
		}
	}
	return false
}

// SeedSpec describes a cluster to place in the world as if a human had created
// it in the portal. Everything but DisplayName has a default.
type SeedSpec struct {
	DisplayName string
	// SubdomainName defaults to DisplayName. Set it when two seeded clusters
	// share a display name, since their DNS labels cannot be shared.
	SubdomainName string
	Tier          string
	InstanceType  string
	Region        string
	Provider      string
}

func (spec SeedSpec) withDefaults() SeedSpec {
	if spec.SubdomainName == "" {
		spec.SubdomainName = spec.DisplayName
	}
	if spec.Tier == "" {
		spec.Tier = tierProduction
	}
	if spec.InstanceType == "" {
		spec.InstanceType = "PB10"
	}
	if spec.Region == "" {
		spec.Region = "eu-central-1"
	}
	if spec.Provider == "" {
		spec.Provider = "Aws"
	}
	return spec
}

// Seed adds an Active cluster the operator did not create, and returns the
// live record.
//
// It skips every rule Create enforces, on purpose: a seeded cluster stands for
// one that already exists, including a Free one, which the API refuses to
// create but the portal sells. Two seeded clusters may even share a subdomain,
// which sets up the ambiguity the operator has to refuse to guess at.
func (s *State) Seed(spec SeedSpec) *Cluster {
	spec = spec.withDefaults()

	s.mu.Lock()
	defer s.mu.Unlock()

	nodes := int32(1)
	if shape, err := findInstanceType(spec.Provider, spec.Region, spec.InstanceType); err == nil {
		nodes = shape.Parameters.NumberOfNodes
	}
	id := s.newID()
	cluster := &Cluster{
		ID:             id,
		DisplayName:    spec.DisplayName,
		SubdomainName:  spec.SubdomainName,
		Provider:       spec.Provider,
		Region:         spec.Region,
		Tier:           spec.Tier,
		InstanceType:   spec.InstanceType,
		ReleaseChannel: "Stable72",
		Status:         statusActive,
		NodeTags:       nodeTags(nodes),
		AllowedIps:     []string{"0.0.0.0/0"},
		DiskSizeGiB:    20, // on the default PB10's menu, so the disk can be resized like a real one
		StorageType:    "SsdStandard",
	}
	s.clusters[id] = cluster
	return cluster
}

// Get returns a snapshot of one cluster, terminated ones included.
func (s *State) Get(id string) (*Cluster, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.find(id)
	if err != nil {
		return nil, false
	}
	snapshot := *c
	return &snapshot, true
}

// List returns snapshots of every cluster not hidden in the portal, by id.
func (s *State) List() []*Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settle()
	out := make([]*Cluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		if c.HiddenInPortal {
			continue
		}
		snapshot := *c
		out = append(out, &snapshot)
	}
	slices.SortFunc(out, func(a, b *Cluster) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// mutate applies fn to an Active cluster and puts it into DeployingChanges.
// Every day-two endpoint is Active-only in prod, and without the check the
// status rewrite would bring a terminating cluster back.
func (s *State) mutate(id string, fn func(*Cluster) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.find(id)
	if err != nil {
		return err
	}
	if c.Status != statusActive {
		return fmt.Errorf("product must be Active, but is %s", c.Status)
	}
	if err := fn(c); err != nil {
		return err
	}
	s.deployChanges(c)
	return nil
}

// deployChanges puts a cluster into DeployingChanges. The caller holds mu.
func (s *State) deployChanges(c *Cluster) {
	c.Status = statusDeployingChanges
	c.settleAt = s.now().Add(s.changeDuration)
}

func (s *State) ChangeInstanceType(id, instanceType string) error {
	return s.mutate(id, func(c *Cluster) error {
		if _, err := findInstanceTypeForTier(c.Provider, c.Region, instanceType, c.Tier); err != nil {
			return err
		}
		c.InstanceType = instanceType
		return nil
	})
}

func (s *State) ChangeStorage(id string, req changeStorageRequest) error {
	return s.mutate(id, func(c *Cluster) error {
		// The window comes first, as in prod, where it is checked before any
		// field of the request.
		if err := s.storageChangeAllowed(c); err != nil {
			return err
		}
		if req.Size < c.DiskSizeGiB {
			return fmt.Errorf("disk size cannot shrink from %d to %d", c.DiskSizeGiB, req.Size)
		}
		spec, err := findInstanceType(c.Provider, c.Region, c.InstanceType)
		if err != nil {
			return err
		}
		// Prod allows the menu plus the size the disk already has, so a cluster
		// parked on an off-menu size can still change its storage type.
		if req.Size != c.DiskSizeGiB && !slices.Contains(spec.Parameters.AvailableDiskSizes, req.Size) {
			return fmt.Errorf("disk size %d is not offered for %s; available: %v",
				req.Size, c.InstanceType, spec.Parameters.AvailableDiskSizes)
		}
		available := storageTypeNames(spec.Parameters.AvailableStorageTypes)
		if !slices.Contains(available, req.StorageType) {
			return fmt.Errorf("storage type %q is not offered for %s; available: %v",
				req.StorageType, c.InstanceType, available)
		}
		c.DiskSizeGiB = req.Size
		c.StorageType = req.StorageType
		c.Iops = req.Iops
		c.Throughput = req.Throughput
		c.storageChangedAt = s.now()
		return nil
	})
}

// storageChangeWindow is how long RavenDB Cloud makes a cluster wait between
// storage changes: six hours on AWS and twelve on Azure premium disks, each
// with five minutes of margin, and no wait anywhere else.
func storageChangeWindow(c *Cluster) time.Duration {
	const margin = 5 * time.Minute
	switch {
	case c.Provider == "Aws":
		return 6*time.Hour + margin
	case c.Provider == "Azure" && c.StorageType == "SsdPremium":
		return 12*time.Hour + margin
	}
	return 0
}

// storageChangeAllowed refuses a storage change inside the window, in prod's
// words, since the operator recognises the refusal by its text. The window
// starts at the previous change, which is where prod starts counting too.
func (s *State) storageChangeAllowed(c *Cluster) error {
	window := storageChangeWindow(c)
	if window == 0 || c.storageChangedAt.IsZero() {
		return nil
	}
	next := c.storageChangedAt.Add(window)
	if !s.now().Before(next) {
		return nil
	}
	//lint:ignore ST1005 prod's message verbatim, which the operator matches on
	//nolint:staticcheck // RavenDB Cloud's own words, capital and full stop included.
	return fmt.Errorf("You've reached the maximum storage modification rate. "+
		"Wait at least %d hours between each storage change. "+
		"The next storage change will be available in %s.",
		int(window.Hours()), humanSpan(next.Sub(s.now())))
}

// humanSpan renders a wait the way prod's message does:
// "5 hours 58 minutes", or "58 minutes" under an hour.
func humanSpan(d time.Duration) string {
	hours, minutes := int(d.Hours()), int(d.Minutes())%60
	span := plural(minutes, "minute")
	if hours > 0 {
		span = plural(hours, "hour") + " " + span
	}
	return span
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// standardNodeTags are the nodes a Production cluster is born with. RavenDB
// Cloud calls everything beyond them an additional node, allows at most
// maxAdditionalNodes of them, and offers the feature on the Production tier
// only. Both numbers mirror RavenDB Cloud's own limits.
var standardNodeTags = []string{"A", "B", "C"}

const maxAdditionalNodes = 3

var errAdditionalNodesNeedProduction = errors.New("additional node is only available on production tier")

func (s *State) AddNode(id string) error {
	return s.mutate(id, func(c *Cluster) error {
		if c.Tier != tierProduction {
			return errAdditionalNodesNeedProduction
		}
		tag, ok := freeNodeTag(c.NodeTags)
		if !ok {
			return fmt.Errorf("cannot have more than %d additional nodes", maxAdditionalNodes)
		}
		c.NodeTags = append(c.NodeTags, tag)
		return nil
	})
}

func (s *State) RemoveNode(id, tag string) error {
	return s.mutate(id, func(c *Cluster) error {
		if c.Tier != tierProduction {
			return errAdditionalNodesNeedProduction
		}
		// The real service refuses to remove a standard node, which is also
		// the floor on cluster size.
		if isStandardNodeTag(tag) {
			return fmt.Errorf("removing standard cluster node (%s) is not allowed", tag)
		}
		if !slices.Contains(c.NodeTags, tag) {
			return fmt.Errorf("no node tagged %q", tag)
		}
		// Cloned, because the snapshots Get hands out share the backing array.
		c.NodeTags = slices.DeleteFunc(slices.Clone(c.NodeTags), func(t string) bool { return t == tag })
		return nil
	})
}

// RestartNode does not go through mutate because prod refuses a product that
// is not Active in its own words.
func (s *State) RestartNode(id, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.find(id)
	if err != nil {
		return err
	}
	if c.Status != statusActive {
		return fmt.Errorf("the node cannot be rebooted because the product is not currently active (status: %s)", c.Status)
	}
	if !slices.Contains(c.NodeTags, tag) {
		return fmt.Errorf("no node tagged %q", tag)
	}
	// A restart deploys changes like any other, as in prod.
	s.deployChanges(c)
	return nil
}

// Terminate starts a termination. The record survives it as Terminated;
// hideInPortal only takes the product out of the list.
func (s *State) Terminate(id string, hideInPortal bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.find(id)
	if err != nil {
		return err
	}
	// Refused rather than restarted, or a repeated terminate would push the
	// deadline out and keep the cluster alive indefinitely.
	if c.Status == statusTerminating || c.Status == statusTerminated {
		return errAlreadyTerminated
	}
	c.Status = statusTerminating
	c.HiddenInPortal = hideInPortal
	c.settleAt = s.now().Add(s.changeDuration)
	return nil
}

// --- lab controls ----------------------------------------------------------

// Vanish removes a cluster without any terminating state, standing in for
// somebody deleting it in the portal behind the operator's back.
func (s *State) Vanish(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.clusters[id]
	delete(s.clusters, id)
	return ok
}

// Patch edits a cluster in place with no transition, standing in for an
// out-of-band change made in the portal. Zero arguments leave a field alone.
func (s *State) Patch(id string, instanceType string, diskSize int32, allowedIps []string, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return false
	}
	if instanceType != "" {
		c.InstanceType = instanceType
	}
	if diskSize > 0 {
		c.DiskSizeGiB = diskSize
	}
	if allowedIps != nil {
		c.AllowedIps = allowedIps
	}
	if status != "" {
		c.Status = status
		c.settleAt = time.Time{}
	}
	return true
}

// FailNext arms a one-shot failure for the next call to an operation.
func (s *State) FailNext(key string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[key] = status
}

// takeFailure consumes the failure armed for operation, if there is one.
func (s *State) takeFailure(operation string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.failNext[operation]
	delete(s.failNext, operation)
	return status, ok
}

// requestLogLimit bounds the request log; a lab only reads the recent tail.
const requestLogLimit = 2000

func (s *State) recordRequest(method, path, agent, product string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var since int64
	if n := len(s.requestLog); n > 0 {
		since = now.Sub(s.requestLog[n-1].At).Milliseconds()
	}
	s.requestLog = append(s.requestLog, RequestEntry{
		At: now, Method: method, Path: path, Status: status, UserAgent: agent,
		SincePrevMs: since, Product: product,
	})
	if len(s.requestLog) > requestLogLimit {
		s.requestLog = s.requestLog[len(s.requestLog)-requestLogLimit:]
	}
}

func (s *State) Requests() []RequestEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RequestEntry(nil), s.requestLog...)
}

// ResetRequestsFor drops the entries of the clusters whose display names start
// with prefix, so a lab can zero its own measurements and leave everybody
// else's. An empty prefix clears everything.
func (s *State) ResetRequestsFor(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestLog = slices.DeleteFunc(s.requestLog, func(e RequestEntry) bool {
		return strings.HasPrefix(e.Product, prefix)
	})
}

// ProductName maps a cluster id to its display name, for attributing a request
// to the lab that caused it.
func (s *State) ProductName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clusters[id]; ok {
		return c.DisplayName
	}
	return ""
}

// --- helpers ---------------------------------------------------------------

func nodeTags(n int32) []string {
	tags := make([]string, 0, n)
	for i := range n {
		tags = append(tags, string(rune('A'+i)))
	}
	return tags
}

// freeNodeTag returns the lowest node tag not in use, so a removed node's
// letter comes back the way it does on a real cluster. The letters run out
// exactly when the cluster is at its maximum size.
func freeNodeTag(tags []string) (string, bool) {
	for i := range len(standardNodeTags) + maxAdditionalNodes {
		if tag := string(rune('A' + i)); !slices.Contains(tags, tag) {
			return tag, true
		}
	}
	return "", false
}

func isStandardNodeTag(tag string) bool {
	return slices.ContainsFunc(standardNodeTags, func(t string) bool { return strings.EqualFold(t, tag) })
}

func storageTypeNames(types []storageTypeInfo) []string {
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, t.Name)
	}
	return names
}

// dns is the per-node hostname list the details endpoint reports: bare
// hostnames with no scheme, as RavenDB Cloud sends. Each is
// "{nodeTag}.{host}", for example "a.clustername.accountname.ravendb.cloud".
func (c *Cluster) dns(domain string) []string {
	hosts := make([]string, 0, len(c.NodeTags))
	for _, tag := range c.NodeTags {
		hosts = append(hosts, strings.ToLower(tag)+"."+c.host(domain))
	}
	return hosts
}

// host is the cluster's part of every hostname, which RavenDB Cloud derives
// from the tier and lowercases: Free clusters share "free.{account}", a
// Development one is "{subdomain}-dev.{account}", a Production one
// "{subdomain}.{account}".
func (c *Cluster) host(domain string) string {
	switch c.Tier {
	case tierFree:
		return strings.ToLower("free." + domain)
	case tierDevelopment:
		return strings.ToLower(c.SubdomainName + "-dev." + domain)
	}
	return strings.ToLower(c.SubdomainName + "." + domain)
}

// maxSubdomainLength mirrors the subdomain length limit RavenDB Cloud
// enforces.
const maxSubdomainLength = 13

// validateSubdomain applies RavenDB Cloud's rules for a cluster DNS label,
// which it enforces even though the OpenAPI document marks subdomainName
// nullable.
func validateSubdomain(name string) error {
	if name == "" {
		return errors.New("domain name cannot be empty")
	}
	if len(name) > maxSubdomainLength {
		return fmt.Errorf("domain name is too long: %d characters, the maximum is %d", len(name), maxSubdomainLength)
	}
	if strings.ContainsFunc(name, func(r rune) bool { return !isSubdomainRune(r) }) {
		return errors.New("the domain can contain only alphanumeric characters and '-'")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return errors.New("the domain cannot start or end with '-'")
	}
	return nil
}

func isSubdomainRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}

// validateIops mirrors RavenDB Cloud's IOPS rule: a disk class whose IOPS
// range the metadata endpoint publishes needs a value inside it, and a class
// with no range refuses one.
func validateIops(offered []storageTypeInfo, storageTypeName string, requested *int32) error {
	i := slices.IndexFunc(offered, func(t storageTypeInfo) bool { return t.Name == storageTypeName })
	if i < 0 {
		return nil
	}
	limits := offered[i].IopsRange
	switch {
	case limits == nil && requested != nil:
		return fmt.Errorf("it is not possible to set IOPS for %s storage type", storageTypeName)
	case limits != nil && requested == nil:
		return fmt.Errorf("the IOPS value is required for %s storage type", storageTypeName)
	case limits != nil && (*requested < limits.Min || *requested > limits.Max):
		return fmt.Errorf("the new IOPS value must be between %d and %d", limits.Min, limits.Max)
	}
	return nil
}
