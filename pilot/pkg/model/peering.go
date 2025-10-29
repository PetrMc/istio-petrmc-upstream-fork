// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package model

import (
	"istio.io/istio/pkg/workloadapi"
)

// SoloServiceScope represents the peering scope for a service
type SoloServiceScope string

const (
	// SoloServiceScopeGlobal indicates the service is visible across all segments
	SoloServiceScopeGlobal SoloServiceScope = "global"
	// SoloServiceScopeSegment indicates the service is only visible within its segment
	SoloServiceScopeSegment SoloServiceScope = "segment"
	// SoloServiceScopeGlobalOnly is a legacy value meaning global + takeover enabled
	SoloServiceScopeGlobalOnly SoloServiceScope = "global-only"
	// SoloServiceScopeCluster indicates the service is only visible within the local cluster
	SoloServiceScopeCluster SoloServiceScope = "cluster"
)

// IsGlobal returns true if the scope is global (including global-only)
func (s SoloServiceScope) IsGlobal() bool {
	return s == SoloServiceScopeGlobal || s == SoloServiceScopeGlobalOnly
}

// IsPeered returns true if the service should be peered/exported to other clusters
func (s SoloServiceScope) IsPeered() bool {
	return s == SoloServiceScopeGlobal || s == SoloServiceScopeSegment || s == SoloServiceScopeGlobalOnly
}

// ToProto converts a SoloServiceScope to the protobuf ServiceScope enum
func (s SoloServiceScope) ToProto() workloadapi.ServiceScope {
	switch s {
	case SoloServiceScopeGlobal:
		return workloadapi.ServiceScope_GLOBAL
	case SoloServiceScopeGlobalOnly:
		return workloadapi.ServiceScope_GLOBAL_ONLY //nolint:staticcheck // legacy value for backward compatibility
	case SoloServiceScopeSegment:
		return workloadapi.ServiceScope_SEGMENT
	default:
		// Local and any other unknown values default to GLOBAL
		// (LOCAL is filtered before conversion in practice)
		return workloadapi.ServiceScope_GLOBAL
	}
}

// SoloServiceScopeFromProto converts a protobuf ServiceScope to SoloServiceScope
func SoloServiceScopeFromProto(scope workloadapi.ServiceScope) SoloServiceScope {
	switch scope {
	case workloadapi.ServiceScope_GLOBAL:
		return SoloServiceScopeGlobal
	case workloadapi.ServiceScope_GLOBAL_ONLY: //nolint:staticcheck // legacy value for backward compatibility
		return SoloServiceScopeGlobalOnly
	case workloadapi.ServiceScope_SEGMENT:
		return SoloServiceScopeSegment
	default:
		return SoloServiceScopeCluster
	}
}

// IsServiceVisibleFromSegment determines if a service with the given scope
// is visible from the specified segment ID.
// - GLOBAL services are visible from all segments
// - GLOBAL_ONLY services are visible from all segments
// - SEGMENT services are only visible from their own segment (segmentID must match)
func IsServiceVisibleFromSegment(scope workloadapi.ServiceScope, serviceSegmentID, viewerSegmentID string) bool {
	switch scope {
	case workloadapi.ServiceScope_GLOBAL, workloadapi.ServiceScope_GLOBAL_ONLY: //nolint:staticcheck // legacy value for backward compatibility
		return true
	case workloadapi.ServiceScope_SEGMENT:
		return serviceSegmentID == viewerSegmentID
	default:
		// Unknown scopes are treated as local/not visible
		return false
	}
}
