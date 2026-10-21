package cloudsim

// Wire shapes for the RavenDB Cloud public API, as described by
// https://api.cloud.ravendb.net/api/v1/swagger.json (openapi 3.0.4, v1).
//
// Declared here rather than imported from internal/cloudapi on purpose: an
// independent reading of the same specification means a mistake in the
// operator's wire mapping cannot cancel out against a simulator that shares it.

type accountInfoResponse struct {
	DomainName string `json:"domainName"`
}

type regionsResponse struct {
	Names []string `json:"names"`
}

type iopsRange struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}

// storageTypeInfo leaves iopsRange out when there is none, as RavenDB Cloud
// does; it never sends a null there.
type storageTypeInfo struct {
	Name      string     `json:"name"`
	IopsRange *iopsRange `json:"iopsRange,omitempty"`
}

type instanceTypeParameters struct {
	// CPUPriority is not in the metadata response, only in details. Free, Dev*
	// and PB* are all Basic; only the P* family, which this catalogue does not
	// offer, is Standard.
	CPUPriority           string            `json:"-"`
	VirtualCPUs           int32             `json:"virtualCpus"`
	RAM                   float64           `json:"ram"`
	AvailableDiskSizes    []int32           `json:"availableDiskSizes"`
	NumberOfNodes         int32             `json:"numberOfNodes"`
	AvailableStorageTypes []storageTypeInfo `json:"availableStorageTypes"`
}

type instanceTypeItem struct {
	Name       string                 `json:"name"`
	Tier       string                 `json:"tier"`
	Parameters instanceTypeParameters `json:"parameters"`
}

type instanceTypesResponse struct {
	InstanceTypes []instanceTypeItem `json:"instanceTypes"`
}

type releaseChannelItem struct {
	Name         string `json:"name"`
	RavenVersion string `json:"ravenVersion"`
	BuildType    string `json:"buildType"`
}

type releaseChannelsResponse struct {
	DefaultReleaseChannel string               `json:"defaultReleaseChannel"`
	ReleaseChannels       []releaseChannelItem `json:"releaseChannels"`
}

type productListItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type productListResponse struct {
	Items []productListItem `json:"items"`
}

type storageResponse struct {
	Size               int32    `json:"size"`
	Iops               *int32   `json:"iops"`
	Throughput         *float64 `json:"throughput"`
	Type               string   `json:"type"`
	AvailableDiskSizes []int32  `json:"availableDiskSizes"`
}

// hardwareInfoResponse has no additionalStorage: every simulated product is
// root-only, and RavenDB Cloud omits the field for those.
type hardwareInfoResponse struct {
	CPUPriority string          `json:"cpuPriority"`
	CPUs        int32           `json:"cpus"`
	RAM         float64         `json:"ram"`
	Storage     storageResponse `json:"storage"`
}

type securityResponse struct {
	AllowedIps []string `json:"allowedIps"`
}

type paymentPlanResponse struct {
	ContractTerm  string `json:"contractTerm"`
	PaymentOption string `json:"paymentOption"`
}

type productDetailsResponse struct {
	ID             string               `json:"id"`
	SubdomainName  string               `json:"subdomainName"`
	DisplayName    string               `json:"displayName"`
	InstanceType   string               `json:"instanceType"`
	CloudProvider  string               `json:"cloudProvider"`
	Status         string               `json:"status"`
	Tier           string               `json:"tier"`
	Region         string               `json:"region"`
	DNS            []string             `json:"dns"`
	NodeTags       []string             `json:"nodeTags"`
	ReleaseChannel string               `json:"releaseChannel"`
	PaymentPlan    paymentPlanResponse  `json:"paymentPlan"`
	HardwareInfo   hardwareInfoResponse `json:"hardwareInfo"`
	Security       securityResponse     `json:"security"`
}

type productCreateRequest struct {
	CloudProvider    string   `json:"cloudProvider"`
	InstanceTypeName string   `json:"instanceTypeName"`
	DisplayName      string   `json:"displayName"`
	ReleaseChannel   string   `json:"releaseChannel"`
	SubdomainName    string   `json:"subdomainName"`
	Tier             string   `json:"tier"`
	Region           string   `json:"region"`
	DiskSize         int32    `json:"diskSize"`
	Iops             *int32   `json:"iops"`
	Throughput       *float64 `json:"throughput"`
	StorageTypeName  string   `json:"storageTypeName"`
	AllowedIps       []string `json:"allowedIps"`
}

type productCreatedResponse struct {
	ProductID string `json:"productId"`
}

type changeStorageRequest struct {
	ProductID   string   `json:"productId"`
	StorageType string   `json:"storageType"`
	Size        int32    `json:"size"`
	Iops        *int32   `json:"iops"`
	Throughput  *float64 `json:"throughput"`
}

type changeInstanceTypeRequest struct {
	ProductID    string `json:"productId"`
	InstanceType string `json:"instanceType"`
}

type nodeRequest struct {
	ProductID string `json:"productId"`
	NodeTag   string `json:"nodeTag"`
}

type terminateRequest struct {
	HideInPortal bool `json:"hideInPortal"`
}
