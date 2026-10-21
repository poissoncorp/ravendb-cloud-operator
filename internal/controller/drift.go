package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// driftPass is what one pass did about the differences between the spec and
// the live cluster.
type driftPass struct {
	// applied is the change carried through, at most one per pass.
	applied *shapeDifference
	// failed is the change being sent when the pass returned an error.
	failed *shapeDifference
	// refused is the first change RavenDB Cloud refused for good this pass,
	// and refusal what it said. The changes after it were still tried.
	refused *shapeDifference
	refusal error
	// deferred says, one sentence each, which changes RavenDB Cloud will take
	// later and what it said about when.
	deferred []string
	// drift is every other difference still outstanding, when nothing was
	// applied.
	drift []string
}

// reconcileDrift applies at most one shape change per pass, because the cloud
// API serialises them anyway and a second one stacked on the first is dropped.
func (r *CloudClusterReconciler) reconcileDrift(
	ctx context.Context,
	cluster *v1alpha1.CloudCluster,
	observed *cloudapi.Cluster,
	cloud cloudapi.Client,
) (driftPass, error) {
	differences := shapeDifferences(&cluster.Spec, observed, cloud)
	if cluster.MayMutate() {
		return r.applyFirst(ctx, cluster, observed, differences)
	}
	var pass driftPass
	for _, diff := range differences {
		pass.drift = append(pass.drift, diff.report)
	}
	if len(pass.drift) > 0 {
		pass.drift = append(pass.drift, whyNotApplied(cluster))
	}
	return pass, nil
}

// applyFirst carries through the first difference RavenDB Cloud takes now. One
// it postpones or refuses changed nothing, so the next is tried: a node count
// should not wait behind a storage window that can be twelve hours long, or
// behind an instance type RavenDB Cloud will never take. A busy cluster ends
// the pass, since every other change would be refused the same way.
func (r *CloudClusterReconciler) applyFirst(
	ctx context.Context, cluster *v1alpha1.CloudCluster, observed *cloudapi.Cluster, differences []shapeDifference,
) (driftPass, error) {
	var pass driftPass
	for i := range differences {
		diff := &differences[i]
		if diff.apply == nil {
			pass.drift = append(pass.drift, diff.report)
			continue
		}
		err := diff.apply(ctx)
		switch {
		case cloudapi.IsTooSoon(err):
			pass.deferred = append(pass.deferred,
				sentence(fmt.Sprintf("RavenDB Cloud will take a change later (%s): %s", lowerFirst(diff.event), cloudSaid(err)))+
					" The operator asks again at each poll.")
		case cloudapi.IsBusy(err):
			pass.deferred = append(pass.deferred, sentence(fmt.Sprintf(
				"The cluster is busy with another change or a restart, so RavenDB Cloud will take this one once it is Active again (%s)",
				lowerFirst(diff.event)))+" The operator asks again at each poll.")
			return pass, nil
		case err != nil && !cloudapi.IsRetryable(err) && !cloudapi.IsUnauthorized(err):
			if pass.refused == nil {
				pass.refused, pass.refusal = diff, err
			}
		case err != nil:
			pass.failed = diff
			return pass, err
		default:
			emit(r.Recorder, cluster, corev1.EventTypeNormal, v1alpha1.EventApplying, "%s", sentence(diff.event))
			log.FromContext(ctx).Info("applying a change", "productID", observed.ID, "change", diff.event)
			pass.applied = diff
			return pass, nil
		}
	}
	return pass, nil
}

// whyNotApplied says why a change was not applied, and how to have it applied.
func whyNotApplied(cluster *v1alpha1.CloudCluster) string {
	if cluster.Spec.ManagementPolicy == "" {
		return "managementPolicy is Observe, the default for a cluster this object " +
			"found already running, so no changes were applied; set spec.managementPolicy to Full to apply them"
	}
	return fmt.Sprintf("managementPolicy is %s, so no changes were applied", cluster.EffectiveManagementPolicy())
}

// shapeDifference is one divergence between the desired shape and the live
// cluster. A nil apply means RavenDB Cloud cannot take this change, so it is
// reported and nothing more; ignoring it would let a manifest claim something
// untrue indefinitely.
type shapeDifference struct {
	// report is what the Synced condition says about this difference.
	report string
	// event is what the object records when apply carries it through. It is
	// empty exactly when apply is nil.
	event string
	// apply carries the difference through to RavenDB Cloud, wrapping any
	// failure with what it was trying to do.
	apply func(ctx context.Context) error
}

// shapeDifferences lists every divergence between spec and the live cluster, in
// the order the mutating path acts on them. Reporting and applying both read
// this one list, so no field can be applied without being reportable.
func shapeDifferences(spec *v1alpha1.CloudClusterSpec, observed *cloudapi.Cluster, cloud cloudapi.Client) []shapeDifference {
	var out []shapeDifference

	// Binding refuses a cluster that differs in either. These are for a
	// manifest that pins one later, a value first inherited from the account.
	if spec.Provider != "" && v1alpha1.Provider(observed.Provider) != spec.Provider {
		out = append(out, shapeDifference{report: fmt.Sprintf(
			"provider differs (desired %s, actual %s); a cluster cannot move between providers", spec.Provider, observed.Provider)})
	}
	if spec.Region != "" && observed.Region != spec.Region {
		out = append(out, shapeDifference{report: fmt.Sprintf(
			"region differs (desired %s, actual %s); a cluster cannot move between regions", spec.Region, observed.Region)})
	}

	desiredIPs := spec.Networking.AllowedIPs
	if len(desiredIPs) > 0 &&
		!slices.Equal(sortedCopy(canonicalCIDRs(desiredIPs)), sortedCopy(canonicalCIDRs(observed.AllowedIPs))) {
		out = append(out, shapeDifference{report: fmt.Sprintf(
			"allowedIPs differ (desired %v, actual %v); the RavenDB Cloud API cannot change them after creation, adjust them in the portal",
			desiredIPs, observed.AllowedIPs)})
	}

	// An empty spec.releaseChannel is whatever the account defaulted to at
	// creation, so only an explicit value can differ.
	if spec.ReleaseChannel != "" && spec.ReleaseChannel != observed.ReleaseChannel {
		out = append(out, shapeDifference{report: fmt.Sprintf(
			"releaseChannel differs (desired %s, actual %s); the RavenDB Cloud API has no endpoint to change it after creation, adjust it in the portal",
			spec.ReleaseChannel, observed.ReleaseChannel)})
	}

	if desired := spec.InstanceType; desired != "" && desired != observed.InstanceType {
		out = append(out, shapeDifference{
			report: fmt.Sprintf("instanceType differs (desired %s, actual %s)", desired, observed.InstanceType),
			event:  fmt.Sprintf("Changing instance type from %s to %s", observed.InstanceType, desired),
			apply: func(ctx context.Context) error {
				return wrapChange(cloud.ChangeInstanceType(ctx, observed.ID, desired), "changing instance type to %s", desired)
			},
		})
	}

	out = append(out, storageDifference(spec, observed, cloud)...)
	return append(out, nodeDifference(spec, observed, cloud)...)
}

// nodeDifference compares the desired node count with the live one, and moves
// it by one node per pass.
func nodeDifference(spec *v1alpha1.CloudClusterSpec, observed *cloudapi.Cluster, cloud cloudapi.Client) []shapeDifference {
	if spec.Nodes == nil {
		return nil
	}
	desired, actual := *spec.Nodes, observed.NodeCount()
	report := fmt.Sprintf("nodes differ (desired %d, actual %d)", desired, actual)
	if desired != actual && slices.Contains(withoutAdditionalNodes, observed.InstanceType) {
		return []shapeDifference{{report: report + fmt.Sprintf(
			"; RavenDB Cloud sells instance type %s without additional nodes, so its node count cannot change", observed.InstanceType)}}
	}
	switch {
	case desired > actual:
		return []shapeDifference{{
			report: report,
			event:  fmt.Sprintf("Adding a node (%d -> %d)", actual, actual+1),
			apply: func(ctx context.Context) error {
				return wrapChange(cloud.AddNode(ctx, observed.ID), "adding a node")
			},
		}}
	case desired < actual:
		// The highest tag, so shrinking does not depend on the order the API
		// returned. desired is at least one, so there is always a tag here.
		tags := sortedCopy(observed.NodeTags)
		victim := tags[len(tags)-1]
		return []shapeDifference{{
			report: report,
			event:  fmt.Sprintf("Removing node %s (%d -> %d)", victim, actual, actual-1),
			apply: func(ctx context.Context) error {
				return wrapChange(cloud.RemoveNode(ctx, observed.ID, victim), "removing node %s", victim)
			},
		}}
	}
	return nil
}

// storageDifference compares the disk attributes this manifest sets with the
// live disk. An attribute it leaves out is not compared, so a manifest that
// names only a size never touches the disk class or its IOPS. The storage
// endpoint takes size, type, iops and throughput in one request, so whatever
// can be applied goes out as a single change.
func storageDifference(spec *v1alpha1.CloudClusterSpec, observed *cloudapi.Cluster, cloud cloudapi.Client) []shapeDifference {
	desired, current := spec.Storage, observed.Hardware.Storage
	if why := unreachableSize(desired.SizeGB, current); why != "" {
		return []shapeDifference{{report: why}}
	}

	// reports name the difference for the Synced condition, moves name it for
	// the event, gathered together so the two describe the same attributes.
	var reports, moves []string
	if desired.SizeGB > current.SizeGiB {
		reports = append(reports, fmt.Sprintf("storage size differs (desired %d GB, actual %d GB)", desired.SizeGB, current.SizeGiB))
		moves = append(moves, fmt.Sprintf("from %d GB to %d GB", current.SizeGiB, desired.SizeGB))
	}
	if desired.Type != "" && cloudapi.StorageType(desired.Type) != current.Type {
		reports = append(reports, fmt.Sprintf("storage type differs (desired %s, actual %s)", desired.Type, current.Type))
		moves = append(moves, fmt.Sprintf("type from %s to %s", current.Type, desired.Type))
	}
	if desired.IOPS != nil && !int32PtrEqual(desired.IOPS, current.IOPS) {
		reports = append(reports, fmt.Sprintf("storage iops differs (desired %d, actual %s)", *desired.IOPS, int32PtrText(current.IOPS)))
		moves = append(moves, fmt.Sprintf("iops from %s to %d", int32PtrText(current.IOPS), *desired.IOPS))
	}
	observedThroughput := throughputFromAPI(current.ThroughputMBps)
	throughputDiffers := desired.ThroughputMBps != nil && !int32PtrEqual(desired.ThroughputMBps, observedThroughput)

	if len(moves) == 0 {
		// RavenDB Cloud judges whether a storage request moves anything from
		// size, type and iops alone, and refuses a throughput-only one as a no-op.
		if throughputDiffers {
			return []shapeDifference{{report: fmt.Sprintf(
				"storage throughputMBps differs (desired %d, actual %s); the RavenDB Cloud storage endpoint refuses a request that moves nothing else, so change it in the portal or alongside another storage edit",
				*desired.ThroughputMBps, int32PtrText(observedThroughput))}}
		}
		return nil
	}

	if throughputDiffers {
		reports = append(reports, fmt.Sprintf("storage throughputMBps differs (desired %d, actual %s)",
			*desired.ThroughputMBps, int32PtrText(observedThroughput)))
		moves = append(moves, fmt.Sprintf("throughput from %s to %d MB/s",
			int32PtrText(observedThroughput), *desired.ThroughputMBps))
	}

	// "Resizing storage from 500 GB to 500 GB" for an iops-only change would
	// read as a no-op bug.
	verb := "Changing storage"
	if desired.SizeGB > current.SizeGiB {
		verb = "Resizing storage"
	}
	change := storageChange(desired, current)
	return []shapeDifference{{
		report: strings.Join(reports, "; "),
		event:  verb + " " + strings.Join(moves, ", "),
		apply: func(ctx context.Context) error {
			return wrapChange(cloud.ChangeStorage(ctx, observed.ID, change), "changing storage")
		},
	}}
}

// unreachableSize says why a size the manifest sets cannot be sent, or returns
// "" when it can. Cloud disks do not shrink, and a live disk may move to a
// narrower list of sizes than the create-time menu, reported per product.
func unreachableSize(desired int32, current cloudapi.Storage) string {
	switch {
	case desired == 0 || desired == current.SizeGiB:
		return ""
	case desired < current.SizeGiB:
		return fmt.Sprintf("storage size %d GB is smaller than the current %d GB; cloud disks cannot shrink",
			desired, current.SizeGiB)
	case len(current.ResizableTo) > 0 && !slices.Contains(current.ResizableTo, desired):
		return fmt.Sprintf("storage size %d GB is not one this cluster can move to from %d GB; RavenDB Cloud offers %s",
			desired, current.SizeGiB, joinInt32s(current.ResizableTo))
	}
	return ""
}

// storageChange is the request for the disk the manifest describes, with every
// attribute it leaves out sent as the disk has it now. The request replaces the
// whole disk spec, and RavenDB Cloud requires IOPS on a disk class with an IOPS
// range even for a size-only change, and on Azure IOPS and throughput together
// or not at all. Both belong to a disk class, so a class change that leaves
// them out asks for the new class's defaults rather than carrying the old
// class's values over.
func storageChange(desired v1alpha1.StorageSpec, current cloudapi.Storage) cloudapi.StorageChange {
	change := cloudapi.StorageChange{
		SizeGiB:        cmp.Or(desired.SizeGB, current.SizeGiB),
		Type:           cmp.Or(cloudapi.StorageType(desired.Type), current.Type),
		IOPS:           desired.IOPS,
		ThroughputMBps: throughputToAPI(desired.ThroughputMBps),
	}
	if change.Type == current.Type {
		change.IOPS = cmp.Or(change.IOPS, current.IOPS)
		change.ThroughputMBps = cmp.Or(change.ThroughputMBps, current.ThroughputMBps)
	}
	return change
}

// wrapChange prefixes a failed change with what it was doing, and passes a nil
// error through.
func wrapChange(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err)
}

// throughputToAPI converts the CRD's integer MB/s to the double the cloud API
// wants. The CRD uses an integer because Kubernetes API conventions steer away
// from floating point in spec fields.
func throughputToAPI(v *int32) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

// throughputFromAPI truncates towards zero. Status and the drift comparison both
// read the value through here, so an observed 125.0 matches a desired 125.
func throughputFromAPI(v *float64) *int32 {
	if v == nil {
		return nil
	}
	i := int32(*v)
	return &i
}

// int32PtrEqual compares two optional numbers, where nil means unset. Two unset
// values are equal; one set and one unset are not.
func int32PtrEqual(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// int32PtrText renders an optional number for a message.
func int32PtrText(v *int32) string {
	if v == nil {
		return "not reported"
	}
	return fmt.Sprintf("%d", *v)
}
