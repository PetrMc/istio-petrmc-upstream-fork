//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package shared

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/cluster"
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

	// DomainSuffix overrides the default peering.DomainSuffix (mesh.internal) when set
	DomainSuffix string
}

func GlobalNameWithSuffix(serviceName string, appsNs namespace.Instance, domainSuffix string) string {
	if serviceName == ServiceGlobalTakeover || serviceName == ServiceRemoteOnlyTakeover {
		return fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, appsNs.Name())
	}
	if domainSuffix == "" {
		domainSuffix = peering.DomainSuffix[1:] // Remove leading dot
	}
	return fmt.Sprintf("%s.%s.%s", serviceName, appsNs.Name(), domainSuffix)
}

func HitAllClusters(t framework.TestContext) echo.Checker {
	return check.ReachedClusters(t.Clusters(), t.Clusters())
}

func hitAllClusters(t framework.TestContext) echo.Checker {
	return HitAllClusters(t)
}

var (
	hitLocalCluster         = check.Cluster(LocalCluster)
	hitRemoteFlatCluster    = check.Cluster(RemoteFlatCluster)
	hitRemoteNetworkCluster = check.Cluster(RemoteNetworkCluster)
)

func HitRemoteClusters(t framework.TestContext) echo.Checker {
	return check.ReachedClusters(t.Clusters(), t.Clusters().Exclude(
		t.Clusters().GetByName(LocalCluster),
	))
}

func hitRemoteClusters(t framework.TestContext) echo.Checker {
	return HitRemoteClusters(t)
}

func HitRemoteNetwork(t framework.TestContext) echo.Checker {
	return check.ReachedClusters(
		t.Clusters(),
		cluster.Clusters{t.Clusters().GetByName(RemoteNetworkCluster)},
	)
}

func hitRemoteNetwork(t framework.TestContext) echo.Checker {
	return HitRemoteNetwork(t)
}

func TestFromZtunnel(t TrafficContext) {
	client := t.Apps.LocalApp[0]
	appsNs := t.Apps.Namespace
	localWaypoint := t.Apps.LocalWaypoint.Instances().ForCluster(LocalCluster)[0]
	domainSuffix := t.DomainSuffix

	callWorkload := func(service echo.Instance, c echo.Checker) {
		t.Helper()
		t.NewSubTestf("to workload %v", service.ServiceName()).Run(func(t framework.TestContext) {
			if c == nil {
				c = check.Error()
			} else {
				c = check.And(c, DestinationWorkload(service.ServiceName()))
			}
			client.CallOrFail(t, echo.CallOptions{
				ToWorkload: service,
				Port:       echo.Port{WorkloadPort: 18081},
				Scheme:     scheme.HTTP,
				Count:      25,
				Check:      c,
			})
		})
	}

	call := func(name string, c echo.Checker) {
		t.Helper()
		t.NewSubTestf("to %v", name).Run(func(t framework.TestContext) {
			if name == ServiceRemoteOnlyTakeover && domainSuffix != defaultDomain {
				// TODO remove skip when 57782 merges in upstream istio
				t.Skip("https://github.com/istio/istio/pull/57782")
			}
			if c == nil {
				c = check.Error()
			} else {
				c = check.And(c, DestinationWorkload(name))
			}
			client.CallOrFail(t, echo.CallOptions{
				Address: GlobalNameWithSuffix(name, appsNs, domainSuffix),
				Port:    echo.Port{ServicePort: 80},
				Scheme:  scheme.HTTP,
				Count:   25,
				Check:   c,
			})
		})
	}

	// No global name to call! So this should fail
	call(ServiceLocal, nil)
	call(ServiceLocalGlobal, check.And(IsL4(), check.OK(), hitLocalCluster))
	call(ServiceRemoteGlobal, check.And(IsL4(), check.OK(), hitRemoteClusters(t)))
	call(ServiceAllGlobal, check.And(IsL4(), check.OK(), hitAllClusters(t)))
	call(ServiceGlobalTakeover, check.And(IsL4(), check.OK(), hitAllClusters(t)))
	call(ServiceRemoteOnlyTakeover, check.And(IsL4(), check.OK(), hitRemoteClusters(t)))

	// Calls should always go to the local waypoint, then sent to all backends.
	call(ServiceLocalWaypoint, WaypointLocalClusterToAll)

	// Calls will go to any _network_ local waypoint because:
	// * RemoteFlatNetwork has a global waypoint, so waypoint svc is peered
	// * The local waypoint service is considered 'global' because it's a waypoint for some global svcs
	// * Since we have a local and global svc for waypoint itself, we select both flat WE and local pod for waypoint
	// After that, they will go to all backends
	call(ServiceRemoteWaypoint, check.And(check.OK(), WaypointLocalNetworkToAll))

	call(ServiceRemoteFlatOnlyWaypoint, check.And(check.OK(), hitRemoteFlatCluster, CheckTraversedLocalNetworkWaypoints()))
	call(ServiceCrossNetworkOnlyWaypoint, check.And(check.OK(), hitRemoteNetworkCluster, CheckTraversedRemoteNetworkWaypoint()))

	// Calls should always go to the local waypoint -- they should always skip the remote waypoint
	call(ServiceAllWaypoint, WaypointLocalNetworkToAll)

	// Should hit remote and local, and in both cases be L7 (from the inbound sidecar)
	call(ServiceSidecar, check.And(check.OK(), hitAllClusters(t), IsL7()))

	// Make sure to-workload waypoint traffic works
	callWorkload(localWaypoint, check.And(check.OK(), hitLocalCluster, IsL7()))
}

func TestFromSidecar(t TrafficContext) {
	client := t.Apps.Sidecar[0]
	appsNs := t.Apps.Namespace
	domainSuffix := t.DomainSuffix

	call := func(t framework.TestContext, name string, c echo.Checker) {
		t.Helper()
		t.NewSubTestf("to %v", name).Run(func(t framework.TestContext) {
			if c == nil {
				c = check.Error()
			} else {
				c = check.And(c, DestinationWorkload(name))
			}
			client.CallOrFail(t, echo.CallOptions{
				Address: GlobalNameWithSuffix(name, appsNs, domainSuffix),
				Port:    echo.Port{ServicePort: 80},
				Scheme:  scheme.HTTP,
				Count:   10,
				Check:   c,
			})
		})
	}

	// No global name to call! So this should fail
	call(t, ServiceLocal, nil)
	call(t, ServiceLocalGlobal, check.And(check.OK(), hitLocalCluster))
	call(t, ServiceRemoteGlobal, check.And(check.OK(), hitRemoteClusters(t)))
	call(t, ServiceAllGlobal, check.And(check.OK(), hitAllClusters(t)))
	call(t, ServiceGlobalTakeover, check.And(check.OK(), hitAllClusters(t)))

	// Calls should always go to the local waypoint, then may be sent either local or remote.
	call(t, ServiceLocalWaypoint, WaypointLocalClusterToAll)

	call(t, ServiceRemoteWaypoint, check.And(
		check.OK(),
		hitAllClusters(t),
		WaypointLocalNetworkToAll,
	))

	// We should hit the remote waypoint and skip policy on the local sidecar
	call(t, ServiceCrossNetworkOnlyWaypoint, check.And(
		check.OK(),
		hitRemoteNetwork(t),
		CheckTraversedRemoteNetworkWaypoint(),
		CheckPolicyAppliedByWorkload(WaypointXNet),
		// TODO test that policy is applied by sidecar when we hit local-network endpoints
	))

	// Calls should always go to the local waypoint -- they should always skip the remote waypoint
	call(t, ServiceAllWaypoint, WaypointLocalNetworkToAll)

	// Should hit remote and local
	call(t, ServiceSidecar, check.And(check.OK(), hitAllClusters(t)))

	// Test PreferClose traffic distribution
	// Note: Node locality labels are set during test setup (see main_test.go SetupNodeLocality)
	// so pods in each cluster automatically get different locality from their nodes
	t.NewSubTest("prefer-close-waypoint").Run(func(t framework.TestContext) {
		// Set traffic distribution to PreferClose on the waypoint Gateway itself
		SetTrafficDistributionOnGateway(t, WaypointDefault, appsNs.Name(), "PreferClose")
		// Set traffic distribution on the all-waypoint service that uses the waypoint
		SetTrafficDistributionOnService(t, ServiceAllWaypoint, appsNs.Name(), "PreferClose")

		// Traffic should only use local waypoint and only reach local cluster backends
		call(t, ServiceAllWaypoint, check.And(
			check.OK(),
			hitLocalCluster,
			CheckAllTraversedWaypointIn(LocalCluster),
		))
	})
}

func TestFromGateway(t TrafficContext) {
	apps := t.Apps
	domainSuffix := t.DomainSuffix
	if domainSuffix == "" {
		domainSuffix = peering.DomainSuffix[1:]
	}

	// Add a suffix to resource names if using a custom domain to avoid conflicts
	nameSuffix := ""
	if t.DomainSuffix != "" && t.DomainSuffix != peering.DomainSuffix[1:] {
		nameSuffix = "-segment"
	}

	// Setup GW for
	cb := t.ConfigIstio().YAML(apps.Namespace.Name(), fmt.Sprintf(`
apiVersion: networking.istio.io/v1
kind: Gateway
metadata:
  name: gateway%s
spec:
  selector:
    istio: ingressgateway
  servers:
  - port:
      number: 80
      name: http
      protocol: HTTP
    hosts:
    - "*.%s" # global
    - "*.svc.cluster.local" # global takeover
`, nameSuffix, domainSuffix))

	for _, svc := range AllServices {
		// For global-takeover, the destination should always be .svc.cluster.local
		destHost := GlobalNameWithSuffix(svc, apps.Namespace, t.DomainSuffix)
		if svc == ServiceGlobalTakeover {
			destHost = fmt.Sprintf("%s.%s.svc.cluster.local", svc, apps.Namespace.Name())
		}

		cb.Eval(apps.Namespace.Name(), map[string]string{
			"name":        svc,
			"nameSuffix":  nameSuffix,
			"host":        GlobalNameWithSuffix(svc, apps.Namespace, t.DomainSuffix),
			"destHost":    destHost,
			"gatewayName": "gateway" + nameSuffix,
		}, `apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: {{.name}}{{.nameSuffix}}
spec:
  hosts:
  - "{{.host}}"
  gateways:
  - {{.gatewayName}}
  http:
  - route:
    - destination:
        host: "{{.destHost}}"
        port:
          number: 80
`)
	}
	cb.ApplyOrFail(t)
	defaultIngress := istio.DefaultIngressOrFail(t, t)
	domainSuffixForCall := t.DomainSuffix
	call := func(t framework.TestContext, name string, c echo.Checker) {
		t.Helper()
		t.NewSubTestf("to %v", name).Run(func(t framework.TestContext) {
			if c == nil {
				c = check.Status(503)
			} else {
				c = check.And(c, DestinationWorkload(name))
			}
			defaultIngress.CallOrFail(t, echo.CallOptions{
				Address: GlobalNameWithSuffix(name, apps.Namespace, domainSuffixForCall),
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
		call(t, ServiceRemoteGlobal, check.And(check.OK(), hitRemoteClusters(t)))
		call(t, ServiceAllGlobal, check.And(check.OK(), hitAllClusters(t)))
		call(t, ServiceGlobalTakeover, check.And(check.OK(), hitAllClusters(t)))

		// For all the waypointcases, we skip waypoints because ingress-use-waypoint is false
		call(t, ServiceLocalWaypoint, check.And(check.OK(), hitAllClusters(t), CheckNotTraversedWaypoint()))
		call(t, ServiceRemoteWaypoint, check.And(check.OK(), hitAllClusters(t), CheckNotTraversedWaypoint()))
		// No local, so we always hit remote but not the waypoint
		call(t, ServiceCrossNetworkOnlyWaypoint, check.And(hitRemoteNetwork(t), CheckNotTraversedWaypoint()))
		call(t, ServiceAllWaypoint, check.And(check.OK(), hitAllClusters(t), CheckNotTraversedWaypoint()))

		// Should hit remote and local
		call(t, ServiceSidecar, check.And(check.OK(), hitAllClusters(t)))
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
		call(t, ServiceRemoteGlobal, check.And(check.OK(), hitRemoteClusters(t)))
		call(t, ServiceAllGlobal, check.And(check.OK(), hitAllClusters(t)))
		call(t, ServiceGlobalTakeover, check.And(check.OK(), hitAllClusters(t)))

		// Calls should always go to the local waypoint -- they should always skip the remote waypoint
		call(t, ServiceLocalWaypoint, WaypointLocalClusterToAll)

		// No local, so we always hit remote and its waypoint
		call(t, ServiceCrossNetworkOnlyWaypoint, check.And(hitRemoteNetwork(t), CheckTraversedRemoteNetworkWaypoint()))

		// Calls should always go to the local network waypoints then go to any cluster's backend
		// since the waypoints are merged across flat network
		call(t, ServiceAllWaypoint, WaypointLocalNetworkToAll)
		call(t, ServiceRemoteWaypoint, check.And(check.OK(), WaypointLocalNetworkToAll))

		// Should hit remote and local
		call(t, ServiceSidecar, check.And(check.OK(), hitAllClusters(t)))

		// Test that this envoy proxy respects PreferClose when routing to waypoints
		// Note: Node locality labels are set during test setup (see main_test.go SetupNodeLocality)
		// so pods in each cluster automatically get different locality from their nodes
		t.NewSubTest("prefer-close-waypoint").Run(func(t framework.TestContext) {
			// Set traffic distribution to PreferClose on the waypoint Gateway itself
			SetTrafficDistributionOnGateway(t, WaypointDefault, apps.Namespace.Name(), "PreferClose")
			// Set traffic distribution on the all-waypoint service that uses the waypoint
			SetTrafficDistributionOnService(t, ServiceAllWaypoint, apps.Namespace.Name(), "PreferClose")

			// Traffic should only use local waypoint and only reach local cluster backends
			call(t, ServiceAllWaypoint, check.And(
				check.OK(),
				hitLocalCluster,
				CheckAllTraversedWaypointIn(LocalCluster),
			))
		})
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

func ApplyDrainingWorkaround(t TrafficContext) {
	domainSuffix := t.DomainSuffix
	if domainSuffix == "" {
		domainSuffix = peering.DomainSuffix[1:]
	}
	// Workaround https://github.com/istio/istio/issues/43239
	t.ConfigIstio().YAML(t.Apps.Namespace.Name(), fmt.Sprintf(`apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: single-request
spec:
  host: '*.%s'
  trafficPolicy:
    connectionPool:
      http:
        maxRequestsPerConnection: 1`, domainSuffix)).ApplyOrFail(t)
}

func applyDrainingWorkaround(t TrafficContext) {
	ApplyDrainingWorkaround(t)
}

var defaultDomain = strings.TrimPrefix(peering.DomainSuffix, ".")

func RunAllTrafficTests(t framework.TestContext, apps *EchoDeployments) {
	if apps == nil {
		t.Errorf("apps must be set and deployed before running traffic tests")
	}
	RunCase := func(t framework.TestContext, name string, domainSuffix string, f func(t TrafficContext)) {
		// always make sure this is set at the start of each test
		SetTrafficDistributionOnGateway(t, WaypointDefault, apps.Namespace.Name(), "Any")

		t.NewSubTest(name).Run(func(t framework.TestContext) {
			f(TrafficContext{TestContext: t, Apps: *apps, DomainSuffix: domainSuffix})
		})
	}

	t.NewSubTest("default mode").Run(func(t framework.TestContext) {
		// Use the default domain suffix (mesh.internal)
		RunCase(t, "test-from-ztunnel", defaultDomain, TestFromZtunnel)
		RunCase(t, "test-from-sidecar", defaultDomain, TestFromSidecar)
		RunCase(t, "test-from-gateway", defaultDomain, TestFromGateway)
	})

	t.NewSubTest("single segment").Run(func(t framework.TestContext) {
		segmentName := "test-segment"
		segmentDomain := "test.mesh"

		// put all clusters on the same segment
		CreateSegmentOrFail(t, segmentName, segmentDomain)
		UseSegmentForTest(t, segmentName)

		// Setup HTTPRoutes for the segment
		if err := SetupHTTPRoutesForSegment(t, apps.Namespace, segmentName, segmentDomain); err != nil {
			t.Fatalf("Failed to setup HTTPRoutes for segment: %v", err)
		}

		// tests are unchanged except we send traffic to `test.mesh` instead of `mesh.internal`
		RunCase(t, "test-from-ztunnel", segmentDomain, TestFromZtunnel)
		RunCase(t, "test-from-sidecar", segmentDomain, TestFromSidecar)
		RunCase(t, "test-from-gateway", segmentDomain, TestFromGateway)
	})

	// TODO test multi-segment

	t.NewSubTest("namespace-service-scope_traffic-tests").Run(func(t framework.TestContext) {
		// setting the scope via SetupApps only works on Creation, and we want it
		// to unset after this subtest scope.
		SetLabelForTest(t, apps.Namespace.Name(), peering.ServiceScopeLabel, string(peering.ServiceScopeGlobal))

		SetupApps(t, apps, true)
		RunCase(t, "test-from-ztunnel", defaultDomain, TestFromZtunnel)
		RunCase(t, "test-from-sidecar", defaultDomain, TestFromSidecar)
		RunCase(t, "test-from-gateway", defaultDomain, TestFromGateway)
	})
}
