// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package features

import (
	"istio.io/istio/pkg/env"
)

// EnableSidecarWaypointInterop enables Sidecar -> Service Waypoint
// See EnableIngressWaypointRouting for the waypoint feature
var EnableSidecarWaypointInterop = registerAmbient("ENABLE_WAYPOINT_INTEROP", false, false,
	"If true, sidecars will short-circuit all processing and connect directly to a waypoint if the destination service has a waypoint.")

// EnableAmbientEnvoyFilterUnlicensed enables EnvoyFilter in waypoints.
// This is done without a license check and usually should not be used
var EnableAmbientEnvoyFilterUnlicensed, EnableAmbientEnvoyFilterSet = env.Register("ENABLE_AMBIENT_ENVOYFILTER", true,
	"If true, ambient waypoints will support EnvoyFilter API.").Lookup()
