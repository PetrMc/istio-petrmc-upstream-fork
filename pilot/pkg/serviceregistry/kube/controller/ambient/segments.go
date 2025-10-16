// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambient

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/platform/discovery/peering"
	soloapi "istio.io/istio/soloapi/v1alpha1"
)

func clusterSegment(
	systemNamespace string,
	Namespaces krt.Collection[*corev1.Namespace], // nolint: gocritic
	Segments krt.Collection[*soloapi.Segment], // nolint: gocritic
) krt.Singleton[*soloapi.Segment] {
	if !features.EnablePeering {
		return krt.NewStatic(ptr.Of(&peering.DefaultSegment), true)
	}

	return krt.NewSingleton(func(ctx krt.HandlerContext) **soloapi.Segment {
		ns := ptr.Flatten(krt.FetchOne(ctx, Namespaces, krt.FilterKey(systemNamespace)))
		if ns == nil {
			return ptr.Of(&peering.DefaultSegment)
		}
		segmentName := ns.Labels[peering.SegmentLabel]
		segment := krt.FetchOne(ctx, Segments, krt.FilterKey(types.NamespacedName{
			Name:      segmentName,
			Namespace: systemNamespace,
		}.String()))
		if segment == nil {
			return ptr.Of(&peering.DefaultSegment)
		}
		return ptr.Flatten(&segment)
	})
}

// localSegmentName safely checks the local segment name, returning "default" if not found
func localSegmentName(ctx krt.HandlerContext, clusterSegment krt.Singleton[*soloapi.Segment]) string {
	localSegment := ptr.Flatten(krt.FetchOne(ctx, clusterSegment.AsCollection()))
	if localSegment == nil {
		return peering.DefaultSegmentName
	}
	return ptr.NonEmptyOrDefault(localSegment.GetName(), peering.DefaultSegmentName)
}
