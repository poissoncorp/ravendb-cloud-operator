// Package export writes CloudCluster manifests from clusters already running in
// RavenDB Cloud.
//
// It is how an existing cluster comes under the operator without being
// reshaped. A hand-written manifest that gets a field wrong would change the
// cluster to match it on the first pass; one written from the cluster itself
// matches it exactly, so applying it changes nothing until somebody edits it.
package export

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// Target is where the manifests are going: the CloudAccount they reference and
// the namespace they will live in.
type Target struct {
	// Account is the CloudAccount name every manifest's spec.accountRef names.
	Account string
	// Namespace goes into metadata.namespace. Empty leaves it out, so the
	// manifests land wherever kubectl apply is pointed.
	Namespace string
}

// Named finds clusters by their display names and writes one manifest each.
// A name that matches no running cluster is an error, and nothing is written.
func Named(ctx context.Context, cloud cloudapi.Client, to Target, names []string) ([]byte, error) {
	clusters := make([]*cloudapi.Cluster, 0, len(names))
	for _, name := range names {
		ref, err := cloud.FindClusterByName(ctx, name)
		switch {
		case cloudapi.IsNotFound(err):
			return nil, fmt.Errorf("no RavenDB Cloud cluster named %q in this account, other than terminated ones", name)
		case err != nil:
			return nil, fmt.Errorf("finding RavenDB Cloud cluster %q: %s", name, cloudapi.Describe(err, nil))
		}
		cluster, err := cloud.GetCluster(ctx, ref.ID)
		if err != nil {
			return nil, fmt.Errorf("reading RavenDB Cloud cluster %q (%s): %s", name, ref.ID, cloudapi.Describe(err, nil))
		}
		clusters = append(clusters, cluster)
	}
	return render(to, clusters)
}

// All writes a manifest for every cluster in the account that has not been
// terminated.
func All(ctx context.Context, cloud cloudapi.Client, to Target) ([]byte, error) {
	refs, err := cloud.ListClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing RavenDB Cloud clusters: %s", cloudapi.Describe(err, nil))
	}
	var clusters []*cloudapi.Cluster
	for _, ref := range refs {
		cluster, err := cloud.GetCluster(ctx, ref.ID)
		if err != nil {
			return nil, fmt.Errorf("reading RavenDB Cloud cluster %q (%s): %s", ref.Name, ref.ID, cloudapi.Describe(err, nil))
		}
		if terminated(cluster) {
			continue
		}
		clusters = append(clusters, cluster)
	}
	return render(to, clusters)
}

// terminated reports a cluster RavenDB Cloud has terminated or is terminating.
// It keeps its record and its name, but a manifest for it would bind an object
// to something that no longer runs.
func terminated(cluster *cloudapi.Cluster) bool {
	return cluster.Status == cloudapi.StatusTerminated || cluster.Status == cloudapi.StatusTerminating
}

// render writes one CloudCluster per cluster, as YAML documents separated the
// way kubectl apply -f reads them, each headed by a comment saying where it
// came from.
func render(to Target, clusters []*cloudapi.Cluster) ([]byte, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var out bytes.Buffer
	for i, cluster := range clusters {
		doc, err := yaml.Marshal(manifestFor(to, cluster))
		if err != nil {
			return nil, fmt.Errorf("rendering the manifest for %q: %w", cluster.DisplayName, err)
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		fmt.Fprintf(&out, "# Written from RavenDB Cloud cluster %q (%s) on %s.\n", cluster.DisplayName, cluster.ID, now)
		out.Write(doc)
	}
	return out.Bytes(), nil
}

// manifest is a CloudCluster with only what an apply needs. The CloudCluster
// type itself would render an empty status and a null creationTimestamp, which
// are noise in a file somebody commits.
type manifest struct {
	APIVersion string                    `json:"apiVersion"`
	Kind       string                    `json:"kind"`
	Metadata   metadata                  `json:"metadata"`
	Spec       v1alpha1.CloudClusterSpec `json:"spec"`
}

type metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// manifestFor describes a cluster exactly as it runs, with managementPolicy
// Full and creationMode TakeOverOnly: since nothing differs, nothing changes
// when it is applied, and from then on the manifest is how the cluster is
// changed.
//
// subdomain is left out. It is only sent when the operator creates a cluster,
// and this one exists.
func manifestFor(to Target, cluster *cloudapi.Cluster) manifest {
	nodes := cluster.NodeCount()
	disk := cluster.Hardware.Storage
	spec := v1alpha1.CloudClusterSpec{
		AccountRef:     v1alpha1.LocalObjectReference{Name: to.Account},
		DisplayName:    cluster.DisplayName,
		Tier:           v1alpha1.Tier(cluster.Tier),
		Provider:       v1alpha1.Provider(cluster.Provider),
		Region:         cluster.Region,
		InstanceType:   cluster.InstanceType,
		ReleaseChannel: cluster.ReleaseChannel,
		Nodes:          &nodes,
		Storage: v1alpha1.StorageSpec{
			SizeGB:         disk.SizeGiB,
			Type:           v1alpha1.StorageType(disk.Type),
			IOPS:           disk.IOPS,
			ThroughputMBps: wholeMBps(disk.ThroughputMBps),
		},
		Networking:       v1alpha1.NetworkingSpec{AllowedIPs: cluster.AllowedIPs},
		ManagementPolicy: v1alpha1.ManagementFull,
		// The cluster exists, so applying this again must never create one in
		// its place.
		CreationMode: v1alpha1.CreationTakeOverOnly,
	}
	return manifest{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "CloudCluster",
		Metadata:   metadata{Name: objectName(cluster.DisplayName), Namespace: to.Namespace},
		Spec:       spec,
	}
}

// wholeMBps is the throughput as the CRD takes it, whole MB/s.
func wholeMBps(v *float64) *int32 {
	if v == nil {
		return nil
	}
	whole := int32(*v)
	return &whole
}

// objectName turns a display name into a Kubernetes object name: lower case,
// with anything a DNS-1123 subdomain does not allow turned into a dash. The
// display name itself is written out in spec, so the two need not match.
func objectName(displayName string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '-'
		}
	}, strings.ToLower(displayName))
	name = strings.Trim(name, "-.")
	// A label value, not the longer DNS subdomain: the connection Secret is
	// labelled with it, and the CRD refuses a longer name.
	if len(name) > validation.LabelValueMaxLength {
		name = strings.TrimRight(name[:validation.LabelValueMaxLength], "-.")
	}
	if name == "" {
		name = "cloudcluster"
	}
	return name
}
