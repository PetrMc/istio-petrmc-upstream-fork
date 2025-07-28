// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package xds

import (
	"encoding/json"
	"fmt"
	"net/http"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"

	"istio.io/istio/pkg/env"
	"istio.io/istio/pkg/lazy"
	"istio.io/istio/pkg/licensing"
	istioversion "istio.io/istio/pkg/version"
)

func validateXdsLicense(con *Connection) error {
	ssl := con.node.GetUserAgentBuildVersion().GetMetadata().GetFields()["ssl.version"]
	if ssl.GetStringValue() == "BoringSSL-FIPS" && !licensing.CheckLicense(licensing.FeatureFIPS, true) {
		return fmt.Errorf("detected FIPS proxy, but license does not enable FIPS")
	}
	ztunnelSsl := con.node.Metadata.GetFields()["TLS_MODE"]
	if ztunnelSsl.GetStringValue() == "FIPS" && !licensing.CheckLicense(licensing.FeatureFIPS, true) {
		return fmt.Errorf("detected FIPS proxy, but license does not enable FIPS")
	}
	lts := con.node.Metadata.GetFields()["SOLO_LTS"]
	if lts.GetStringValue() == "true" && !licensing.CheckLicense(licensing.FeatureLTS, true) {
		return fmt.Errorf("detected LTS proxy, but license does not enable LTS")
	}
	return nil
}

// Evaluate the controlPlane lazily in order to allow "POD_NAME" env var setting after running the process.
var controlPlaneLicense = lazy.New(func() (*core.ControlPlane, error) {
	// The Pod Name (instance identity) is in PilotArgs, but not reachable globally nor from DiscoveryServer
	podName := env.Register("POD_NAME", "", "").Get()
	byVersion, err := json.Marshal(IstioControlPlaneInstance{
		Component: "istiod",
		ID:        podName,
		Info:      istioversion.Info,
		License:   string(licensing.GetXDSLicenseState()),
	})
	if err != nil {
		log.Warnf("XDS: Could not serialize control plane id: %v", err)
	}
	return &core.ControlPlane{Identifier: string(byVersion)}, nil
})

// LicenseControlPlane identifies the instance and Istio version including license info
func LicenseControlPlane() *core.ControlPlane {
	// Error will never happen because the getter of lazy does not return error.
	cp, _ := controlPlaneLicense.Get()
	return cp
}

func (s *DiscoveryServer) debugLicenseHandler(w http.ResponseWriter, req *http.Request) {
	info := licensing.GetLicenseInfo()
	writeJSON(w, info, req)
}
