//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package shared

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/scopes"
	"istio.io/istio/platform/discovery/peering"
)

type TrafficContext struct {
	framework.TestContext
	Apps EchoDeployments
}

func globalName(serviceName string, appsNs namespace.Instance) string {
	if serviceName == ServiceGlobalTakeover {
		return fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, appsNs.Name())
	}
	return fmt.Sprintf("%s.%s%s", serviceName, appsNs.Name(), peering.DomainSuffix)
}

func testFromZtunnel(t TrafficContext) {
	hitBothClusters := check.ReachedClusters(t.Clusters(), t.Clusters())
	client := t.Apps.LocalApp[0]
	appsNs := t.Apps.Namespace

	call := func(name string, c echo.Checker) {
		t.Helper()
		t.NewSubTestf("to %v", name).Run(func(t framework.TestContext) {
			if c == nil {
				c = check.Error()
			} else {
				c = check.And(c, DestinationWorkload(name))
			}
			client.CallOrFail(t, echo.CallOptions{
				Address: globalName(name, appsNs),
				Port:    echo.Port{ServicePort: 80},
				Scheme:  scheme.HTTP,
				Count:   10,
				Check:   c,
			})
		})
	}

	// No global name to call! So this should fail
	call(ServiceLocal, nil)
	call(ServiceLocalGlobal, check.And(IsL4(), check.OK(), check.Cluster(LocalCluster)))
	call(ServiceRemoteGlobal, check.And(IsL4(), check.OK(), check.Cluster(RemoteCluster)))
	call(ServiceBothGlobal, check.And(IsL4(), check.OK(), hitBothClusters))
	call(ServiceGlobalTakeover, check.And(IsL4(), check.OK(), hitBothClusters))

	// Calls should always go to the local waypoint, then may be sent either local or remote.
	call(ServiceLocalWaypoint, WaypointLocalForBoth)
	// TODO(https://github.com/solo-io/ztunnel/issues/730): this should be WaypointRemoteDirectLocal.
	// Today, we don't have a way to prevent sending back to the source cluster for this flow
	// So we do `client (local net) -> waypoint (remote net) -> backend (local net)
	call(ServiceRemoteWaypoint, check.And(check.OK(), PerClusterChecker(map[string]echo.Checker{
		LocalCluster:  check.OK(),
		RemoteCluster: CheckTraversedWaypointIn(RemoteCluster),
	})))
	call(ServiceRemoteOnlyWaypoint, check.And(check.OK(), check.Cluster(RemoteCluster), CheckTraversedWaypointIn(RemoteCluster)))
	// Calls should always go to the local waypoint -- they should always skip the remote waypoint
	call(ServiceBothWaypoint, WaypointLocalForBoth)

	// Should hit remote and local, and in both cases be L7 (from the inbound sidecar)
	call(ServiceSidecar, check.And(check.OK(), hitBothClusters, IsL7()))
}

func testFromSidecar(t TrafficContext) {
	hitBothClusters := check.ReachedClusters(t.Clusters(), t.Clusters())
	client := t.Apps.Sidecar[0]
	appsNs := t.Apps.Namespace

	call := func(name string, c echo.Checker) {
		t.Helper()
		t.NewSubTestf("to %v", name).Run(func(t framework.TestContext) {
			if c == nil {
				c = check.Error()
			} else {
				c = check.And(c, DestinationWorkload(name))
			}
			client.CallOrFail(t, echo.CallOptions{
				Address: globalName(name, appsNs),
				Port:    echo.Port{ServicePort: 80},
				Scheme:  scheme.HTTP,
				Count:   10,
				Check:   c,
			})
		})
	}

	// No global name to call! So this should fail
	call(ServiceLocal, nil)
	call(ServiceLocalGlobal, check.And(check.OK(), check.Cluster(LocalCluster)))
	call(ServiceRemoteGlobal, check.And(check.OK(), check.Cluster(RemoteCluster)))
	call(ServiceBothGlobal, check.And(check.OK(), hitBothClusters))
	call(ServiceGlobalTakeover, check.And(check.OK(), hitBothClusters))

	// Calls should always go to the local waypoint, then may be sent either local or remote.
	call(ServiceLocalWaypoint, WaypointLocalForBoth)
	// This setup is broken; users should NOT do this. However, we test it to ensure we have the right behavior.
	call(ServiceRemoteWaypoint, check.And(
		check.OK(),
		hitBothClusters,
		// There are two cases: we hit the local pod (no waypoint) or we hit the remote pod (waypoint).
		// In both cases, the sidecar applies policies.
		// This is because we need to decide to apply policies before we pick the endpoint.
		// Real users should deploy a waypoint in all clusters or no clusters, not have a mix like this case.
		CheckPolicyEnforced(
			map[string]string{
				LocalCluster: ServiceSidecar,
			},
			map[string]string{
				RemoteCluster: "waypoint",
			},
		),
	))

	// We should hit the remote waypoint and skip policy on the local sidecar
	call(ServiceRemoteOnlyWaypoint, check.And(
		check.OK(),
		check.Cluster(RemoteCluster),
		CheckTraversedWaypointIn(RemoteCluster),
		CheckPolicyAppliedByWorkload("waypoint"),
	))
	// Calls should always go to the local waypoint -- they should always skip the remote waypoint
	call(ServiceBothWaypoint, WaypointLocalForBoth)

	// Should hit remote and local
	call(ServiceSidecar, check.And(check.OK(), hitBothClusters))
}

func testFromGateway(t TrafficContext) {
	hitBothClusters := check.ReachedClusters(t.Clusters(), t.Clusters())
	apps := t.Apps

	// Setup GW for
	cb := t.ConfigIstio().YAML(apps.Namespace.Name(), `
apiVersion: networking.istio.io/v1
kind: Gateway
metadata:
  name: gateway
spec:
  selector:
    istio: ingressgateway
  servers:
  - port:
      number: 80
      name: http
      protocol: HTTP
    hosts:
    - "*"
`)

	for _, svc := range AllServices {
		cb.Eval(apps.Namespace.Name(), map[string]string{
			"name": svc,
			"host": globalName(svc, apps.Namespace),
		}, `apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: {{.name}}
spec:
  hosts:
  - "{{.host}}"
  gateways:
  - gateway
  http:
  - route:
    - destination:
        host: "{{.host}}"
        port:
          number: 80
`)
	}
	cb.ApplyOrFail(t)
	defaultIngress := istio.DefaultIngressOrFail(t, t)
	call := func(t framework.TestContext, name string, c echo.Checker) {
		t.Helper()
		t.NewSubTestf("to %v", name).Run(func(t framework.TestContext) {
			if c == nil {
				c = check.Status(503)
			} else {
				c = check.And(c, DestinationWorkload(name))
			}
			defaultIngress.CallOrFail(t, echo.CallOptions{
				Address: globalName(name, apps.Namespace),
				Port:    echo.Port{ServicePort: 80},
				Scheme:  scheme.HTTP,
				Count:   10,
				Check:   c,
			})
		})
	}

	// Changing ingress-use-waypoint will not drain pooled connections
	applyDrainingWorkaround(t)
	t.NewSubTest("default").Run(func(t framework.TestContext) {
		// No global name to call! So this should fail
		call(t, ServiceLocal, nil) // Should get a NC
		call(t, ServiceLocalGlobal, check.And(check.OK(), check.Cluster(LocalCluster)))
		call(t, ServiceRemoteGlobal, check.And(check.OK(), check.Cluster(RemoteCluster)))
		call(t, ServiceBothGlobal, check.And(check.OK(), hitBothClusters))
		call(t, ServiceGlobalTakeover, check.And(check.OK(), hitBothClusters))

		// For all the waypointcases, we skip waypoints because ingress-use-waypoint is false
		call(t, ServiceLocalWaypoint, check.And(check.OK(), hitBothClusters, CheckNotTraversedWaypoint()))
		call(t, ServiceRemoteWaypoint, check.And(check.OK(), hitBothClusters, CheckNotTraversedWaypoint()))
		// No local, so we always hit remote but not the waypoint
		call(t, ServiceRemoteOnlyWaypoint, check.And(check.Cluster(RemoteCluster), CheckNotTraversedWaypoint()))
		call(t, ServiceBothWaypoint, check.And(check.OK(), hitBothClusters, CheckNotTraversedWaypoint()))

		// Should hit remote and local
		call(t, ServiceSidecar, check.And(check.OK(), hitBothClusters))
	})
	t.NewSubTest("use-waypoint").Run(func(t framework.TestContext) {
		for _, svc := range AllServices {
			SetIngressUseWaypoint(t, svc, apps.Namespace.Name())
		}
		// This is really unfortunate but the only way to trigger a drain.
		// ingress-use-waypoint is determined by the eastwest gateway. This uses the *outer* HBONE connection.
		// This gets aggressively pooled, but we need to re-establish things.
		// This can impact real users, but only if they are trying to change this back and forth cross-cluster which is odd.
		RestartDeployment(t, "istio-ingressgateway", "istio-system")
		t.Cleanup(func() {
			// Do it again so we don't impact other tests
			RestartDeployment(t, "istio-ingressgateway", "istio-system")
		})
		// No global name to call! So this should fail
		call(t, ServiceLocal, nil) // Should get a NC
		call(t, ServiceLocalGlobal, check.And(check.OK(), check.Cluster(LocalCluster)))
		call(t, ServiceRemoteGlobal, check.And(check.OK(), check.Cluster(RemoteCluster)))
		call(t, ServiceBothGlobal, check.And(check.OK(), hitBothClusters))
		call(t, ServiceGlobalTakeover, check.And(check.OK(), hitBothClusters))

		// Calls should always go to the local waypoint, then may be sent either local or remote.
		call(t, ServiceLocalWaypoint, WaypointLocalForBoth)
		call(t, ServiceRemoteWaypoint, WaypointRemoteDirectLocal)
		// No local, so we always hit remote and its waypoint
		call(t, ServiceRemoteOnlyWaypoint, check.And(check.Cluster(RemoteCluster), CheckTraversedWaypointIn(RemoteCluster)))
		// Calls should always go to the local waypoint -- they should always skip the remote waypoint
		call(t, ServiceBothWaypoint, WaypointLocalForBoth)

		// Should hit remote and local
		call(t, ServiceSidecar, check.And(check.OK(), hitBothClusters))
	})
}

func SetIngressUseWaypoint(t framework.TestContext, name, ns string) {
	for _, c := range t.Clusters() {
		set := func(service bool) error {
			var set string
			if service {
				set = fmt.Sprintf("%q", "true")
			} else {
				set = "null"
			}
			label := []byte(fmt.Sprintf(`{"metadata":{"labels":{"%s":%s}}}`,
				"istio.io/ingress-use-waypoint", set))
			_, err := c.Kube().CoreV1().Services(ns).Patch(context.TODO(), name, types.MergePatchType, label, metav1.PatchOptions{})
			return controllers.IgnoreNotFound(err)
		}

		if err := set(true); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := set(false); err != nil {
				scopes.Framework.Errorf("failed resetting service-addressed for %s", name)
			}
		})
	}
}

func RestartDeployment(t framework.TestContext, name, ns string) {
	patchData := []byte(fmt.Sprintf(`{
			"spec": {
				"template": {
					"metadata": {
						"annotations": {
							"kubectl.kubernetes.io/restartedAt": %q
						}
					}
				}
			}
		}`, time.Now().Format(time.RFC3339)))
	for _, c := range t.Clusters() {
		_, err := c.Kube().AppsV1().Deployments(ns).Patch(context.TODO(), name, types.MergePatchType, patchData, metav1.PatchOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func applyDrainingWorkaround(t TrafficContext) {
	// Workaround https://github.com/istio/istio/issues/43239
	t.ConfigIstio().YAML(t.Apps.Namespace.Name(), `apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: single-request
spec:
  host: '*.mesh.internal'
  trafficPolicy:
    connectionPool:
      http:
        maxRequestsPerConnection: 1`).ApplyOrFail(t)
}

func RunAllTrafficTests(t framework.TestContext, apps *EchoDeployments) {
	if apps == nil {
		t.Errorf("apps must be set and deployed before running traffic tests")
	}
	RunCase := func(name string, f func(t TrafficContext)) {
		t.NewSubTest(name).Run(func(t framework.TestContext) {
			f(TrafficContext{TestContext: t, Apps: *apps})
		})
	}
	RunCase("test-from-ztunnel", testFromZtunnel)
	RunCase("test-from-sidecar", testFromSidecar)
	RunCase("test-from-gateway", testFromGateway)
}
