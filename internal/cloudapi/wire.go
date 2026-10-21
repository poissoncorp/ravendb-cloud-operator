package cloudapi

// Wire types mirror the RavenDB Cloud OpenAPI document
// (https://api.cloud.ravendb.net/api/v1/swagger.json, vendored in
// .reference/swagger.json). They are separate from the domain types so that
// when the cloud API changes, the blast radius is this file, never the CRDs.

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

type wireAccountInfo struct {
	DomainName string `json:"domainName"`
}

type wireRegions struct {
	Names []string `json:"names"`
}

type wireStorageTypeInfo struct {
	Name string `json:"name"`
	// Null when the class does not let you choose the IOPS.
	IOPSRange *wireIOPSRange `json:"iopsRange"`
}

type wireIOPSRange struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}

type wireInstanceTypeParameters struct {
	VirtualCPUs           int32                 `json:"virtualCpus"`
	RAM                   float64               `json:"ram"`
	AvailableDiskSizes    []int32               `json:"availableDiskSizes"`
	NumberOfNodes         int32                 `json:"numberOfNodes"`
	AvailableStorageTypes []wireStorageTypeInfo `json:"availableStorageTypes"`
}

type wireInstanceTypeItem struct {
	Name       string                     `json:"name"`
	Tier       string                     `json:"tier"`
	Parameters wireInstanceTypeParameters `json:"parameters"`
}

type wireInstanceTypes struct {
	InstanceTypes []wireInstanceTypeItem `json:"instanceTypes"`
}

type wireReleaseChannelItem struct {
	Name         string `json:"name"`
	RavenVersion string `json:"ravenVersion"`
	BuildType    string `json:"buildType"`
}

type wireReleaseChannels struct {
	DefaultReleaseChannel string                   `json:"defaultReleaseChannel"`
	ReleaseChannels       []wireReleaseChannelItem `json:"releaseChannels"`
}

type wireProductListItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type wireProductList struct {
	Items []wireProductListItem `json:"items"`
}

type wireStorage struct {
	Size       int32    `json:"size"`
	IOPS       *int32   `json:"iops"`
	Throughput *float64 `json:"throughput"`
	Type       string   `json:"type"`
	// The sizes this product may resize to, not the create-time menu.
	AvailableDiskSizes []int32 `json:"availableDiskSizes"`
}

type wireHardwareInfo struct {
	CPUPriority string  `json:"cpuPriority"`
	CPUs        int32   `json:"cpus"`
	RAM         float64 `json:"ram"`
	// Storage is always the root disk. AdditionalStorage is the data disk and
	// is null unless the product has one.
	Storage           wireStorage `json:"storage"`
	AdditionalStorage wireStorage `json:"additionalStorage"`
}

type wireSecurity struct {
	AllowedIPs []string `json:"allowedIps"`
}

type wireProductDetails struct {
	ID             string           `json:"id"`
	DisplayName    string           `json:"displayName"`
	InstanceType   string           `json:"instanceType"`
	CloudProvider  string           `json:"cloudProvider"`
	Status         string           `json:"status"`
	Tier           string           `json:"tier"`
	Region         string           `json:"region"`
	DNS            []string         `json:"dns"`
	NodeTags       []string         `json:"nodeTags"`
	ReleaseChannel string           `json:"releaseChannel"`
	HardwareInfo   wireHardwareInfo `json:"hardwareInfo"`
	Security       wireSecurity     `json:"security"`
}

type wireProductCreateRequest struct {
	CloudProvider    string   `json:"cloudProvider"`
	InstanceTypeName string   `json:"instanceTypeName"`
	DisplayName      string   `json:"displayName"`
	ReleaseChannel   string   `json:"releaseChannel"`
	SubdomainName    string   `json:"subdomainName,omitempty"`
	Tier             string   `json:"tier"`
	Region           string   `json:"region"`
	DiskSize         int32    `json:"diskSize"`
	IOPS             *int32   `json:"iops,omitempty"`
	Throughput       *float64 `json:"throughput,omitempty"`
	StorageTypeName  string   `json:"storageTypeName"`
	AllowedIPs       []string `json:"allowedIps"`
	DeploymentType   string   `json:"deploymentType,omitempty"`
}

type wireProductCreated struct {
	ProductID string `json:"productId"`
}

type wireChangeStorageRequest struct {
	ProductID   string   `json:"productId"`
	StorageType string   `json:"storageType"`
	Size        int32    `json:"size"`
	IOPS        *int32   `json:"iops,omitempty"`
	Throughput  *float64 `json:"throughput,omitempty"`
}

type wireChangeInstanceTypeRequest struct {
	ProductID    string `json:"productId"`
	InstanceType string `json:"instanceType"`
}

type wireNodeRequest struct {
	ProductID string `json:"productId"`
	NodeTag   string `json:"nodeTag,omitempty"`
}

type wireTerminateRequest struct {
	HideInPortal bool `json:"hideInPortal"`
}

// --- enum translation ------------------------------------------------------
//
// The cloud API spells providers "Aws"/"Azure"/"Gcp". The CRD and the domain
// model use the spelling people write, "AWS"/"GCP".

func providerToWire(p Provider) string {
	switch p {
	case ProviderAWS:
		return "Aws"
	case ProviderAzure:
		return "Azure"
	case ProviderGCP:
		return "Gcp"
	}
	return string(p)
}

func providerFromWire(s string) Provider {
	switch s {
	case "Aws", "AWS", "aws":
		return ProviderAWS
	case "Azure", "azure":
		return ProviderAzure
	case "Gcp", "GCP", "gcp":
		return ProviderGCP
	}
	return Provider(s)
}

// userStorage picks the disk spec.storage describes: the data disk when the
// product has one, since that is the disk the user sizes, and the root disk
// otherwise. A root-only product sends additionalStorage as null, which leaves
// the zero value.
func (w wireHardwareInfo) userStorage() wireStorage {
	if w.AdditionalStorage.Size > 0 {
		return w.AdditionalStorage
	}
	return w.Storage
}

// nodeURLs turns the wire's dns list into URLs clients can use.
//
// RavenDB Cloud sends bare hostnames, "{nodeTag}.{subdomain}.{rootDomain}"
// with no scheme. A value that already carries a scheme is left alone, so an
// API that starts sending URLs does not get two.
func nodeURLs(dns []string) []string {
	if len(dns) == 0 {
		return nil
	}
	out := make([]string, 0, len(dns))
	for _, host := range dns {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if !strings.Contains(host, "://") {
			host = "https://" + host
		}
		out = append(out, host)
	}
	return out
}

func (w wireProductDetails) toDomain() *Cluster {
	storage := w.HardwareInfo.userStorage()
	return &Cluster{
		ID:             w.ID,
		DisplayName:    w.DisplayName,
		Provider:       providerFromWire(w.CloudProvider),
		Region:         w.Region,
		Tier:           Tier(w.Tier),
		InstanceType:   w.InstanceType,
		ReleaseChannel: w.ReleaseChannel,
		Status:         ClusterStatus(w.Status),
		URLs:           nodeURLs(w.DNS),
		NodeTags:       w.NodeTags,
		AllowedIPs:     w.Security.AllowedIPs,
		Hardware: Hardware{
			CPUPriority: w.HardwareInfo.CPUPriority,
			CPUs:        w.HardwareInfo.CPUs,
			RAMGiB:      w.HardwareInfo.RAM,
			Storage: Storage{
				SizeGiB:        storage.Size,
				ResizableTo:    storage.AvailableDiskSizes,
				Type:           StorageType(storage.Type),
				IOPS:           storage.IOPS,
				ThroughputMBps: storage.Throughput,
			},
		},
	}
}

func (w wireInstanceTypeItem) toDomain() InstanceType {
	offers := make([]StorageOffer, 0, len(w.Parameters.AvailableStorageTypes))
	for _, st := range w.Parameters.AvailableStorageTypes {
		offer := StorageOffer{Type: StorageType(st.Name)}
		if st.IOPSRange != nil {
			offer.IOPS = &IOPSRange{Min: st.IOPSRange.Min, Max: st.IOPSRange.Max}
		}
		offers = append(offers, offer)
	}
	return InstanceType{
		Name:          w.Name,
		Tier:          Tier(w.Tier),
		VirtualCPUs:   w.Parameters.VirtualCPUs,
		RAMGiB:        w.Parameters.RAM,
		NumberOfNodes: w.Parameters.NumberOfNodes,
		DiskSizesGiB:  w.Parameters.AvailableDiskSizes,
		StorageOffers: offers,
	}
}

// --- refusals ---------------------------------------------------------------

// generalErrorKey is where RavenDB Cloud's public API puts a problem with a
// request as a whole rather than with one field of it.
const generalErrorKey = "generalError"

// problemsFromWire reads what RavenDB Cloud said was wrong out of a refusal.
//
// A refusal comes in one of two shapes. A validator's verdict is ASP.NET's
// SerializableError, a flat object from field name to a list of messages, such
// as {"subdomainName":["Domain name is too long."]}. A body the framework
// refused before any validator ran is ValidationProblemDetails, the same map
// nested under "errors" beside a title and a status. Anything else yields
// nothing, and the caller falls back to the HTTP status.
func problemsFromWire(body []byte) []Problem {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return nil
	}
	if nested, ok := top["errors"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(nested, &inner) == nil {
			top = inner
		}
	}

	// A stable order, so a message built from these does not reshuffle
	// between reconciles.
	fields := slices.SortedFunc(maps.Keys(top), generalFirst)

	var out []Problem
	for _, field := range fields {
		var messages []string
		if json.Unmarshal(top[field], &messages) != nil {
			continue
		}
		name := field
		if isGeneral(field) {
			name = ""
		}
		for _, message := range messages {
			if message = forDisplay(message, maxDisplayRunes); message != "" {
				out = append(out, Problem{Field: name, Message: message})
			}
		}
	}
	return out
}

func generalFirst(a, b string) int {
	switch {
	case isGeneral(a) == isGeneral(b):
		return strings.Compare(a, b)
	case isGeneral(a):
		return -1
	default:
		return 1
	}
}

// isGeneral reports whether a refusal key means the request as a whole. The
// empty key is where ASP.NET puts a model error with no field, and "_error" is
// the name RavenDB Cloud uses for it outside the public API.
func isGeneral(field string) bool {
	return field == generalErrorKey || field == "" || field == "_error"
}
