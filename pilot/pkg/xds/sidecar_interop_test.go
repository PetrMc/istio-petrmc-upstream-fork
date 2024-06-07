// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package xds_test

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"

	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/simulation"
	"istio.io/istio/pilot/test/xds"
	"istio.io/istio/pilot/test/xdstest"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/tmpl"
)

func TestWaypointSidecarInterop(t *testing.T) {
	test.SetForTest(t, &features.EnableAmbient, true)
	test.SetForTest(t, &features.EnableAmbientWaypoints, true)
	test.SetForTest(t, &features.EnableSidecarWaypointInterop, true)
	baseService := `
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: with-vip
  namespace: default
  labels:
    istio.io/use-waypoint: {{.Waypoint}}
spec:
  hosts:
  - vip.example.com
  addresses: [{{.VIP}}]
  location: MESH_INTERNAL
  resolution: {{.Resolution}}
  ports:
  - name: tcp
    number: 70
    protocol: TCP
  - name: http
    number: 80
    protocol: HTTP
  - name: auto-http
    number: 8080
  - name: auto-tcp
    number: 8081
  - name: tls
    number: 443
    protocol: TLS
---
`
	waypointSvc := `
apiVersion: v1
kind: Service
metadata:
  labels:
    gateway.istio.io/managed: istio.io-mesh-controller
    gateway.networking.k8s.io/gateway-name: waypoint
    istio.io/gateway-name: waypoint
  name: waypoint
  namespace: default
spec:
  clusterIP: 3.0.0.0
  ports:
  - appProtocol: hbone
    name: mesh
    port: 15008
  selector:
    gateway.networking.k8s.io/gateway-name: waypoint
`
	vs := `apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: route
spec:
  hosts:
  - {{.Host}}
{{- if eq .Type "http" }}
  http:
  -
{{- else if eq .Type "tls" }}
  tls:
  - match:
    - sniHosts: [{{.Host}}]
{{- else }}
  tcp:
  -
{{- end }}
    route:
    - destination:
        host: virtual-service-applied
---
`
	proxy := func(ns string) *model.Proxy {
		return &model.Proxy{ConfigNamespace: ns}
	}
	cases := []struct {
		name   string
		port   string
		vsType string
	}{
		{
			name:   "http",
			port:   "http",
			vsType: "http",
		},
		{
			name:   "auto http",
			port:   "auto-http",
			vsType: "http",
		},
		{
			name:   "auto tcp",
			port:   "auto-tcp",
			vsType: "tcp",
		},
		{
			name:   "tcp",
			port:   "tcp",
			vsType: "tcp",
		},
		{
			name:   "tls",
			port:   "tls",
			vsType: "tls",
		},
	}
	for _, tt := range cases {
		for _, vip := range []string{"1.1.1.1", ""} {
			for _, resolution := range []string{"STATIC", "DNS"} {
				for _, useWaypoint := range []string{"waypoint", ""} {
					tt := tt
					useWaypoint := useWaypoint
					resolution := resolution
					vip := vip
					name := tt.name + "-" + resolution
					if vip != "" {
						name += "-vip"
					}
					if useWaypoint != "" {
						name += "-waypoint"
					}
					t.Run(name, func(t *testing.T) {
						baseService := tmpl.MustEvaluate(baseService, map[string]string{"Waypoint": useWaypoint, "VIP": vip, "Resolution": resolution})
						cfg := baseService
						cfg += tmpl.MustEvaluate(vs, map[string]string{
							"Host": "vip.example.com",
							"Type": tt.vsType,
						})
						// Instances of waypoint
						cfg += `apiVersion: networking.istio.io/v1
kind: WorkloadEntry
metadata:
  name: waypoint-a
  namespace: default
spec:
  address: 3.0.0.1
  labels:
    gateway.networking.k8s.io/gateway-name: waypoint
---
apiVersion: networking.istio.io/v1
kind: WorkloadEntry
metadata:
  name: waypoint-b
  namespace: default
spec:
  address: 3.0.0.2
  labels:
    gateway.networking.k8s.io/gateway-name: waypoint
---
`
						s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{
							KubernetesObjectString: waypointSvc,
							ConfigString:           cfg,
							KubeClientModifier: func(c kube.Client) {
								se, err := kubernetesObjectFromString(baseService)
								assert.NoError(t, err)
								clienttest.NewWriter[*networkingv1.ServiceEntry](t, c).Create(se.(*networkingv1.ServiceEntry))
								gw := &gateway.Gateway{
									ObjectMeta: metav1.ObjectMeta{
										Name:      "waypoint",
										Namespace: "default",
									},
									Spec: gateway.GatewaySpec{
										GatewayClassName: constants.WaypointGatewayClassName,
										Listeners: []gateway.Listener{{
											Name:     "mesh",
											Port:     15008,
											Protocol: gateway.ProtocolType(protocol.HBONE),
										}},
									},
									Status: gateway.GatewayStatus{
										Addresses: []gatewayv1.GatewayStatusAddress{{
											Type:  ptr.Of(gateway.HostnameAddressType),
											Value: "waypoint.default.svc.cluster.local",
										}},
									},
								}
								clienttest.NewWriter[*gateway.Gateway](t, c).Create(gw)
							},
						})
						ports := map[string]int{
							"tcp":       70,
							"http":      80,
							"auto-http": 8080,
							"auto-tcp":  8081,
							"tls":       443,
						}
						protocols := map[string]simulation.Protocol{
							"tcp":       simulation.TCP,
							"http":      simulation.HTTP,
							"auto-http": simulation.HTTP,
							"auto-tcp":  simulation.TCP,
							"tls":       simulation.TCP,
						}
						mode := simulation.Plaintext
						if tt.port == "tls" {
							mode = simulation.TLS
						}
						proxy := s.SetupProxy(proxy("default"))
						sim := simulation.NewSimulation(t, s, proxy)
						res := sim.Run(simulation.Call{
							Address:    "1.1.1.1",
							TLS:        mode,
							Port:       ports[tt.port],
							Protocol:   protocols[tt.port],
							Sni:        "vip.example.com",
							HostHeader: "vip.example.com",
							CallMode:   simulation.CallModeOutbound,
						})

						if useWaypoint != "" && vip != "" {
							// We should ignore the VS and go to the waypoint directly
							cluster := fmt.Sprintf("outbound|%d||vip.example.com", ports[tt.port])
							res.Matches(t, simulation.Result{ClusterMatched: cluster})
							gotEps := slices.Sort(xdstest.ExtractLoadAssignments(s.Endpoints(proxy))[cluster])
							// Endpoints should be HBONE references to the waypoint pods (3.0.0.{1,2}), with the target set to the service VIP
							assert.Equal(t, gotEps, []string{
								fmt.Sprintf("connect_originate;%s:%d;3.0.0.1:15008", vip, ports[tt.port]),
								fmt.Sprintf("connect_originate;%s:%d;3.0.0.2:15008", vip, ports[tt.port]),
							})
						} else {
							res.Matches(t, simulation.Result{ClusterMatched: fmt.Sprintf("outbound|%d||virtual-service-applied.default", ports[tt.port])})
						}
					})
				}
			}
		}
	}
}

func kubernetesObjectFromString(s string) (runtime.Object, error) {
	decode := kube.IstioCodec.UniversalDeserializer().Decode
	if len(strings.TrimSpace(s)) == 0 {
		return nil, fmt.Errorf("empty kubernetes object")
	}
	o, _, err := decode([]byte(s), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed deserializing kubernetes object: %v (%v)", err, s)
	}
	return o, nil
}
