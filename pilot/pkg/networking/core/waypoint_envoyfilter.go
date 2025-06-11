// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package core

import (
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/envoyfilter"
	"istio.io/istio/pilot/pkg/util/runtime"
	"istio.io/istio/pkg/licensing"
	"istio.io/istio/pkg/log"
)

func (lb *ListenerBuilder) waypointEnvoyFilter(svc *model.Service) *WaypointEnvoyFilterPatcher {
	return waypointEnvoyFilter(lb.node, lb.push, svc)
}

func waypointEnvoyFilter(proxy *model.Proxy, push *model.PushContext, svc *model.Service) *WaypointEnvoyFilterPatcher {
	if !AmbientEnvoyFilterLicensed() {
		return &WaypointEnvoyFilterPatcher{}
	}
	efm := model.PolicyMatcherForProxy(proxy).WithService(svc).WithRootNamespace(push.Mesh.GetRootNamespace())
	mergedWrapper := push.WaypointEnvoyFilters(proxy, efm)
	if mergedWrapper == nil {
		return &WaypointEnvoyFilterPatcher{}
	}
	wrapper := &model.EnvoyFilterWrapper{
		Patches:                      mergedWrapper.Patches,
		ReferencedNamespacedServices: mergedWrapper.ReferencedNamespacedServices,
		ReferencedServices:           mergedWrapper.ReferencedServices,
	}
	return &WaypointEnvoyFilterPatcher{wrapper}
}

type WaypointEnvoyFilterPatcher struct {
	wrapper *model.EnvoyFilterWrapper
}

func (wp *WaypointEnvoyFilterPatcher) PatchFilterChain(fc *listener.FilterChain) {
	efw := wp.wrapper
	if efw == nil {
		return
	}
	defer runtime.HandleCrash(runtime.LogPanic, func(any) {
		envoyfilter.IncrementEnvoyFilterErrorMetric(envoyfilter.FilterChain)
		log.Errorf("listeners patch %s/%s caused panic, so the patches did not take effect", efw.Namespace, efw.Name)
	})
	envoyfilter.WaypointPatchFilterChain(
		networking.EnvoyFilter_GATEWAY,
		efw.Patches,
		&listener.Listener{}, // dummy. It's used for matching, and we don't allow any matches on it.
		fc,
	)
}

func (wp *WaypointEnvoyFilterPatcher) PatchRoute(rc *route.RouteConfiguration) {
	if wp == nil {
		return
	}
	efw := wp.wrapper
	if efw == nil {
		return
	}
	defer runtime.HandleCrash(runtime.LogPanic, func(any) {
		envoyfilter.IncrementEnvoyFilterErrorMetric(envoyfilter.Route)
		log.Errorf("route patch %s/%s caused panic, so the patches did not take effect", efw.Namespace, efw.Name)
	})
	mergedWrapper := &model.MergedEnvoyFilterWrapper{
		ReferencedNamespacedServices: efw.ReferencedNamespacedServices,
		ReferencedServices:           efw.ReferencedServices,
		Patches:                      efw.Patches,
	}
	envoyfilter.ApplyRouteConfigurationPatches(
		networking.EnvoyFilter_GATEWAY,
		&model.Proxy{}, // dummy, used only for gateways
		mergedWrapper,
		rc,
	)
}

// AmbientEnvoyFilterLicensed enables EnvoyFilter in waypoints.
// Requires a license
func AmbientEnvoyFilterLicensed() bool {
	if !features.EnableAmbientEnvoyFilterUnlicensed {
		return false
	}
	if !features.EnableAmbientEnvoyFilterSet {
		// If they didn't explicitly configure it, turn it on if they are licensed. Do not warn, since we don't know if they
		// wanted it or not
		return licensing.CheckLicense(licensing.FeatureEnvoyFilter, false)
	}
	return licensing.CheckLicense(licensing.FeatureEnvoyFilter, true)
}
