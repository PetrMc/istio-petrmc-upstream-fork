//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambient

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/ambient"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/echo/common/ports"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/scopes"
)

func TestInterop(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		// Apply a deny-all waypoint policy. This allows us to test the traffic traverses the waypoint
		t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
			"Waypoint": apps.ServiceAddressedWaypoint.Config().ServiceWaypointProxy,
		}, `
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: deny-all-waypoint
spec:
  targetRefs:
  - kind: Gateway
    group: gateway.networking.k8s.io
    name: {{.Waypoint}}
`).ApplyOrFail(t)
		t.NewSubTest("sidecar-service").Run(func(t framework.TestContext) {
			for _, src := range apps.Sidecar {
				for _, dst := range apps.ServiceAddressedWaypoint {
					for _, opt := range callOptions {
						t.NewSubTestf("%v", opt.Scheme).Run(func(t framework.TestContext) {
							opt = opt.DeepCopy()
							opt.To = dst
							opt.Check = CheckDeny
							src.CallOrFail(t, opt)
						})
					}
				}
			}
		})
		t.NewSubTest("sidecar-workload").Run(func(t framework.TestContext) {
			t.Skip("not yet implemented")
			for _, src := range apps.Sidecar {
				for _, dst := range apps.WorkloadAddressedWaypoint {
					for _, dstWl := range dst.WorkloadsOrFail(t) {
						for _, opt := range callOptions {
							t.NewSubTestf("%v-%v", opt.Scheme, dstWl.Address()).Run(func(t framework.TestContext) {
								opt = opt.DeepCopy()
								opt.Address = dstWl.Address()
								opt.Port = echo.Port{ServicePort: ports.All().MustForName(opt.Port.Name).WorkloadPort}
								opt.Check = CheckDeny
								src.CallOrFail(t, opt)
							})
						}
					}
				}
			}
		})
		t.NewSubTest("ingress-service").Run(func(t framework.TestContext) {
			t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
				"Destination": apps.ServiceAddressedWaypoint.ServiceName(),
			}, `apiVersion: networking.istio.io/v1alpha3
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
    hosts: ["*"]
---
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: route
spec:
  gateways:
  - gateway
  hosts:
  - "*"
  http:
  - route:
    - destination:
        host: "{{.Destination}}"
`).ApplyOrFail(t)
			ingress := istio.DefaultIngressOrFail(t, t)
			t.NewSubTest("endpoint routing").Run(func(t framework.TestContext) {
				ingress.CallOrFail(t, echo.CallOptions{
					Port: echo.Port{
						Protocol:    protocol.HTTP,
						ServicePort: 80,
					},
					Scheme: scheme.HTTP,
					Check:  check.OK(),
				})
			})
			t.NewSubTest("service routing").Run(func(t framework.TestContext) {
				SetServiceAddressed(t, apps.ServiceAddressedWaypoint.ServiceName(), apps.ServiceAddressedWaypoint.NamespaceName())
				ingress.CallOrFail(t, echo.CallOptions{
					Port: echo.Port{
						Protocol:    protocol.HTTP,
						ServicePort: 80,
					},
					Scheme: scheme.HTTP,
					Check:  CheckDeny,
				})
			})
		})
		t.NewSubTest("ingress-workload").Run(func(t framework.TestContext) {
			t.Skip("not implemented")
			t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
				"Destination": apps.WorkloadAddressedWaypoint.ServiceName(),
			}, `apiVersion: networking.istio.io/v1alpha3
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
    hosts: ["*"]
---
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: route
spec:
  gateways:
  - gateway
  hosts:
  - "*"
  http:
  - route:
    - destination:
        host: "{{.Destination}}"
`).ApplyOrFail(t)
			ingress := istio.DefaultIngressOrFail(t, t)
			t.NewSubTest("endpoint routing").Run(func(t framework.TestContext) {
				ingress.CallOrFail(t, echo.CallOptions{
					Port: echo.Port{
						Protocol:    protocol.HTTP,
						ServicePort: 80,
					},
					Scheme: scheme.HTTP,
					Check:  CheckDeny,
				})
			})
			t.NewSubTest("service routing").Run(func(t framework.TestContext) {
				// This will be ignored entirely if there is only workload waypoint, so this behaves the same as endpoint routing.
				SetServiceAddressed(t, apps.WorkloadAddressedWaypoint.ServiceName(), apps.WorkloadAddressedWaypoint.NamespaceName())
				ingress.CallOrFail(t, echo.CallOptions{
					Port: echo.Port{
						Protocol:    protocol.HTTP,
						ServicePort: 80,
					},
					Scheme: scheme.HTTP,
					Check:  CheckDeny,
				})
			})
		})
		t.NewSubTest("waypoint-waypoint").Run(func(t framework.TestContext) {
			// Test traffic flows where we have (possibly) two waypoints in the path.
			// `captured --> captured-waypoint --HTTPRoute--> <service with a waypoint>`
			// Give captured a waypoint..
			ambient.NewWaypointProxyOrFail(t, apps.Namespace, "captured-waypoint")
			SetWaypoint(t, Captured, "captured-waypoint")
			// Create a SE; one of our tests will use a DNS service.
			// This is primarily to catch a specific regression (https://github.com/solo-io/istio/issues/1746).
			t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
				"Namespace": apps.Namespace.Name(),
				"Waypoint":  echo.Waypoint,
			}, `apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: dns-service
  labels:
    istio.io/use-waypoint: {{.Waypoint}}
spec:
  addresses:
  - 240.240.240.238
  endpoints:
  - address: captured.{{.Namespace}}.svc.cluster.local
  exportTo:
  - .
  hosts:
  - dns-service.example.com
  location: MESH_EXTERNAL
  ports:
  - name: http
    number: 80
    protocol: HTTP
  resolution: DNS
`).ApplyOrFail(t)
			t.NewSubTest("service").Run(func(t framework.TestContext) {
				// We will set up a route such that we call captured --> captured-waypoint --HTTPRoute--> service addressed waypoint.
				t.ConfigIstio().YAML(apps.Namespace.Name(), `apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: reroute
spec:
  parentRefs:
  - name: captured
    kind: Service
    group: ""
    port: 80
  rules:
  - backendRefs:
    - name: service-addressed-waypoint
      port: 80`).ApplyOrFail(t)
				// Pick a random workload that is captured as src.
				for _, src := range apps.WorkloadAddressedWaypoint {
					for _, dst := range apps.Captured {
						src.CallOrFail(t, echo.CallOptions{
							To:   dst,
							Port: echo.Port{Name: "http"},
						})
					}
				}
			})
			t.NewSubTest("serviceentry").Run(func(t framework.TestContext) {
				// We will set up a route such that we call captured --> captured-waypoint --HTTPRoute--> service addressed waypoint.
				t.ConfigIstio().YAML(apps.Namespace.Name(), `apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: reroute
spec:
  parentRefs:
  - name: captured
    kind: Service
    group: ""
    port: 80
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /foo
    backendRefs:
    - group: networking.istio.io
      kind: Hostname
      name: dns-service.example.com`).ApplyOrFail(t)
				// Pick a random workload that is captured as src.
				for _, src := range apps.WorkloadAddressedWaypoint {
					for _, dst := range apps.Captured {
						src.CallOrFail(t, echo.CallOptions{
							To:   dst,
							Port: echo.Port{Name: "http"},
							// Mostly to make sure we are not hitting the stale route
							HTTP: echo.HTTP{Path: "/foo"},
						})
					}
				}
			})
		})
	})
}

func SetServiceAddressed(t framework.TestContext, name, ns string) {
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
			return err
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
