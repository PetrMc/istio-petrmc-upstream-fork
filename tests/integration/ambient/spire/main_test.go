//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package spire

import (
	"testing"

	"istio.io/api/label"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/crd"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/deployment"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/istioctl"
	"istio.io/istio/pkg/test/framework/components/namespace"
	testlabel "istio.io/istio/pkg/test/framework/label"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/tests/integration/security/util/cert"
	"istio.io/istio/tests/util/sanitycheck"
)

var i istio.Instance

type EchoDeployments struct {
	// Namespace echo apps will be deployed
	Namespace namespace.Instance
	// Captured echo service
	Captured echo.Instances
	// Uncaptured echo Service
	Uncaptured echo.Instances

	// All echo services
	All echo.Instances
}

// TODO best way to get the systemnamespace before we actually install Istio? this is weirdly non-obvious
var spireOverrides = `
global:
  spire:
    trustDomain: cluster.local
spire-agent:
    authorizedDelegates:
        - "spiffe://cluster.local/ns/istio-system/sa/ztunnel"
    sockets:
        admin:
            enabled: true
            mountOnHost: true
        hostBasePath: /run/spire/agent/sockets

spire-server:
    persistence:
        type: emptyDir
`

// TestMain defines the entrypoint for pilot tests using a standard Istio installation.
// If a test requires a custom install it should go into its own package, otherwise it should go
// here to reuse a single install across tests.
func TestMain(m *testing.M) {
	// nolint: staticcheck
	framework.
		NewSuite(m).
		RequireSingleCluster().
		RequireMinVersion(26).
		Label(testlabel.IPv4). // https://github.com/istio/istio/issues/41008
		Setup(func(t resource.Context) error {
			t.Settings().Ambient = true
			return nil
		}).
		Setup(func(t resource.Context) error {
			// *not* running this in Setup would be preferred, but it needs to
			// be installed *before* Istio is, or not all istio pods will go
			// healthy, and the test rig doesn't provide a nice way to install things
			// that Istio depends on before Istio
			err := DeploySpireWithOverrides(t, spireOverrides)
			if err != nil {
				if t.Settings().CIMode {
					namespace.Dump(t, SpireNamespace)
				}
			}
			return err
		}).
		Setup(istio.Setup(&i, func(ctx resource.Context, cfg *istio.Config) {
			// can't deploy VMs without eastwest gateway
			ctx.Settings().SkipVMs()
			cfg.EnableCNI = true
			cfg.DeployEastWestGW = false
			cfg.ControlPlaneValues = `
values:
  gateways:
    spire:
      workloads: true
  cni:
    repair:
      enabled: true
  ztunnel:
    spire:
      enabled: true
    terminationGracePeriodSeconds: 5
    env:
      SECRET_TTL: 5m
`
		}, cert.CreateCASecretAlt)).
		Teardown(func(t resource.Context) {
			TeardownSpire(t)
		}).
		Run()
}

const (
	Captured   = "captured"
	Uncaptured = "uncaptured"
)

func TestTrafficWithSpire(t *testing.T) {
	framework.NewTest(t).
		Run(func(t framework.TestContext) {
			ns, client, server := setupSmallTrafficTest(t)
			sanitycheck.RunTrafficTestClientServer(t, client, server)

			// Deploy waypoint
			crd.DeployGatewayAPIOrSkip(t)
			istioctl.NewOrFail(t, istioctl.Config{}).InvokeOrFail(t, []string{
				"waypoint",
				"apply",
				"--namespace",
				ns.Name(),
				"--enroll-namespace",
				"--wait",
			})
			// Test again
			sanitycheck.RunTrafficTestClientServer(t, client, server)
		})
}

func setupSmallTrafficTest(t framework.TestContext) (namespace.Instance, echo.Instance, echo.Instance) {
	var client, server echo.Instance
	testNs := namespace.NewOrFail(t, namespace.Config{
		Prefix: "default",
		Inject: false,
		Labels: map[string]string{
			label.IoIstioDataplaneMode.Name: "ambient",
			"istio-injection":               "disabled",
		},
	})
	deployment.New(t).
		With(&client, echo.Config{
			Service:   "client",
			Namespace: testNs,
			Ports:     []echo.Port{},
		}).
		With(&server, echo.Config{
			Service:   "server",
			Namespace: testNs,
			Ports: []echo.Port{
				{
					Name:         "http",
					Protocol:     protocol.HTTP,
					WorkloadPort: 8090,
				},
			},
		}).
		BuildOrFail(t)

	return testNs, client, server
}
