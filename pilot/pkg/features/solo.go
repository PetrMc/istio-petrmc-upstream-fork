// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package features

import (
	"strings"

	"istio.io/istio/pkg/env"
	"istio.io/istio/pkg/util/sets"
)

// EnableSidecarWaypointInterop enables Sidecar -> Service Waypoint
// See EnableIngressWaypointRouting for the waypoint feature
var EnableSidecarWaypointInterop = registerAmbient("ENABLE_WAYPOINT_INTEROP", false, false,
	"If true, sidecars will short-circuit all processing and connect directly to a waypoint if the destination service has a waypoint.")

// EnableAmbientEnvoyFilterUnlicensed enables EnvoyFilter in waypoints.
// This is done without a license check and usually should not be used
var EnableAmbientEnvoyFilterUnlicensed, EnableAmbientEnvoyFilterSet = env.Register("ENABLE_AMBIENT_ENVOYFILTER", true,
	"If true, ambient waypoints will support EnvoyFilter API.").Lookup()

var EnablePeering, EnablePeeringExplicitly = env.Register("ENABLE_PEERING_DISCOVERY", false,
	"If enabled, cross-cluster service discovery will be done via peering.").Lookup()

var DisableRemoteSecrets, _ = env.Register("DISABLE_LEGACY_MULTICLUSTER", false,
	"If enabled, cross-cluster service discovery can ONLY be done via peering. Remote secrets will be ignored.").Lookup()

var EnableEnvoyMultiNetworkHBONE = func() bool {
	v, f := env.Register("ENABLE_AMBIENT_ENVOY_MULTI_NETWORK", false,
		"If enabled, ambient multi-network mode will work for Envoy based data-planes (Sidecar, Gateway, Waypoint).").Lookup()
	if f {
		return v
	}
	// If unset, default to enabling when peering is enabled.
	// We explicitly do not check the license here; if they are not licensed we will just turn off the service discovery,
	// not break routing
	return EnablePeering
}()

var PermitCrossNamespaceResouceAccess = func() sets.Set[string] {
	v, f := env.Register("PERMIT_CROSS_NAMESPACE_RESOURCE_ACCESS", "",
		"If enabled, cross-namespace resource access will be allowed for the given proxies. Format: namespace1/proxy1,namespace2/proxy2,namespace3/proxy3").Lookup()
	permits := sets.New[string]()
	if f {
		for _, s := range strings.Split(v, ",") {
			permits.Insert(s)
		}
	}
	return permits
}()
