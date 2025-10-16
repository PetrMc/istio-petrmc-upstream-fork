// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/workloadapi"
	soloapi "istio.io/istio/soloapi/v1alpha1"
)

const DefaultSegmentName = "default"

var DefaultSegment = soloapi.Segment{
	ObjectMeta: metav1.ObjectMeta{
		Name:      DefaultSegmentName,
		Namespace: PeeringNamespace,
	},
	Spec: soloapi.SegmentSpec{
		Domain: strings.TrimPrefix(DomainSuffix, "."), // default: "mesh.internal"
	},
}

// validatePeerSegment checks if a peer cluster's segment is valid against our local segments
func (c *NetworkWatcher) validatePeerSegment(clusterID cluster.ID, segmentInfo *workloadapi.Segment) bool {
	if segmentInfo == nil {
		// This shouldn't happen after our changes to network.go, but handle it gracefully
		log.Warnf("peer cluster %s has no segment info, treating as default segment", clusterID)
		return true // Accept as backward compatibility case
	}

	// Special case: "default" segment may not exist as a CRD locally
	localCopy := &DefaultSegment
	if segmentInfo.Name != DefaultSegmentName {
		localCopy = c.segments.Get(segmentInfo.Name, PeeringNamespace)
		if localCopy == nil {
			// We don't have this segment locally
			log.Errorf("peer cluster %s declared segment %s which we don't have locally", clusterID, segmentInfo.Name)
			return false
		}
	}

	// Check if the domains match
	if localCopy.Spec.Domain != segmentInfo.Domain {
		log.Errorf("peer cluster %s declared segment %s with domain %s, but our local segment has domain %s",
			clusterID, segmentInfo.Name, segmentInfo.Domain, localCopy.Spec.Domain)
		return false
	}

	return true
}
