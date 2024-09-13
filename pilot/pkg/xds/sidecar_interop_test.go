// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package xds_test

import (
	"fmt"
	"strings"
	"testing"

	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
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
	"istio.io/istio/pkg/licensing"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/tmpl"
	"istio.io/istio/pkg/util/sets"
)

func init() {
	licensing.SetForTest()
}

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

func TestWaypointEnvoyFilter(t *testing.T) {
	envoyfilter := `apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: filter-ratelimit
  namespace: default
  annotations:
    envoyfilter.istio.io/referenced-services: ratelimit.default.svc.cluster.local
spec:
  targetRefs:
  - kind: ServiceEntry
    name: app
    group: networking.istio.io
  configPatches:
    - applyTo: HTTP_FILTER
      match:
        context: GATEWAY
        listener:
          filterChain:
            filter:
              name: "envoy.filters.network.http_connection_manager"
              subFilter:
                name: "envoy.filters.http.router"
      patch:
        operation: INSERT_BEFORE
        value:
          name: envoy.filters.http.ratelimit
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.http.ratelimit.v3.RateLimit
            domain: ratelimit
            failure_mode_deny: true
            timeout: 10s
            rate_limit_service:
              grpc_service:
                envoy_grpc:
                  # Must be exposed by envoyfilter.istio.io/referenced-services
                  cluster_name: outbound|8081||ratelimit.default.svc.cluster.local
                  authority: ratelimit.default.svc.cluster.local
              transport_api_version: V3
    - applyTo: VIRTUAL_HOST
      match:
        context: GATEWAY
      patch:
        operation: MERGE
        value:
          rate_limits:
            - actions: # any actions in here
              - request_headers:
                  header_name: ":path"
                  descriptor_key: "PATH"
    - applyTo: HTTP_ROUTE
      match:
        context: GATEWAY
        routeConfiguration:
          vhost:
            route:
              name: slow
      patch:
        operation: MERGE
        value:
          route:
            rate_limits:
              - actions: # any actions in here
                - request_headers:
                    header_name: ":method"
                    descriptor_key: "METHOD"
`
	envoyfilterGatewayLevel := `apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: filter-lua
  namespace: default
spec:
  targetRefs:
  - name: waypoint
    kind: Gateway
    group: gateway.networking.k8s.io
  configPatches:
    - applyTo: HTTP_FILTER
      match:
        context: GATEWAY
        listener:
          filterChain:
            filter:
              name: "envoy.filters.network.http_connection_manager"
              subFilter:
                name: "envoy.filters.http.router"
      patch:
        operation: INSERT_BEFORE
        value:
          name: envoy.lua
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
            inlineCode: ""
`
	notSelectedAppServiceEntry := `apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: not-app
  namespace: default
  labels:
    istio.io/use-waypoint: waypoint
spec:
  hosts: [not-app.com]
  addresses: [2.3.4.5]
  ports:
  - number: 80
    name: http
    protocol: HTTP
  resolution: STATIC
  workloadSelector:
    labels:
      app: app`
	referencedService := `apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: ratelimit
  namespace: default
spec:
  hosts: [ratelimit.default.svc.cluster.local]
  addresses: [2.3.4.6]
  ports:
  - number: 8081
    name: http
    protocol: HTTP
  resolution: STATIC`
	route := `apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: header
  namespace: default
spec:
  parentRefs:
  - kind: ServiceEntry
    name: app
    group: networking.istio.io
  rules:
  - name: slow
    matches:
    - path:
        type: PathPrefix
        value: /slow
    backendRefs:
    - name: echo
      port: 80
  - name: fast
    backendRefs:
    - name: echo
      port: 80`
	d, proxy := setupWaypointTest(t,
		waypointGateway, waypointSvc, waypointInstance,
		appServiceEntry, notSelectedAppServiceEntry,
		envoyfilter, envoyfilterGatewayLevel, referencedService,
		route)

	terminateListener := xdstest.ExtractListener("connect_terminate", d.Listeners(proxy))
	l := xdstest.ExtractListener("main_internal", d.Listeners(proxy))
	app := xdstest.ExtractHTTPConnectionManager(t,
		xdstest.ExtractFilterChain(model.BuildSubsetKey(model.TrafficDirectionInboundVIP, "http", "app.com", 80), l))
	notApp := xdstest.ExtractHTTPConnectionManager(t,
		xdstest.ExtractFilterChain(model.BuildSubsetKey(model.TrafficDirectionInboundVIP, "http", "not-app.com", 80), l))
	connectTerminate := xdstest.ExtractHTTPConnectionManager(t, xdstest.ExtractFilterChain("default", terminateListener))

	// Only selected service should get the service-attached filter
	assert.Equal(t, sets.New(slices.Map(app.HttpFilters, (*hcm.HttpFilter).GetName)...).Contains("envoy.filters.http.ratelimit"), true)
	assert.Equal(t, sets.New(slices.Map(notApp.HttpFilters, (*hcm.HttpFilter).GetName)...).Contains("envoy.filters.http.ratelimit"), false)
	assert.Equal(t, sets.New(slices.Map(connectTerminate.HttpFilters, (*hcm.HttpFilter).GetName)...).Contains("envoy.filters.http.ratelimit"), false)

	// Only service filter should get the gateway attached filter (not the connect terminate!)
	assert.Equal(t, sets.New(slices.Map(app.HttpFilters, (*hcm.HttpFilter).GetName)...).Contains("envoy.lua"), true)
	assert.Equal(t, sets.New(slices.Map(notApp.HttpFilters, (*hcm.HttpFilter).GetName)...).Contains("envoy.lua"), true)
	assert.Equal(t, sets.New(slices.Map(connectTerminate.HttpFilters, (*hcm.HttpFilter).GetName)...).Contains("envoy.lua"), false)

	// Cluster should be added...
	assert.Equal(t, xdstest.ExtractCluster("outbound|8081||ratelimit.default.svc.cluster.local", d.Clusters(proxy)) != nil, true)

	vhost := app.GetRouteConfig().GetVirtualHosts()[0].GetRateLimits()
	assert.Equal(t, len(vhost), 1)
	assert.Equal(t, vhost[0].GetActions()[0].GetRequestHeaders().GetHeaderName(), ":path")

	// We should apply the rate limit config to the matched route ('slow') but not the unmatched one
	routes := app.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	assert.Equal(t, len(routes), 2)
	assert.Equal(t, len(routes[0].GetRoute().GetRateLimits()), 1)
	assert.Equal(t, routes[0].GetRoute().GetRateLimits()[0].GetActions()[0].GetRequestHeaders().GetHeaderName(), ":method")
	assert.Equal(t, len(routes[1].GetRoute().GetRateLimits()), 0)
}

func TestWaypointEnvoyFilterClusters(t *testing.T) {
	gatewayFilter := `apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: global-cluster-mods
spec:
  targetRefs:
  - name: waypoint
    kind: Gateway
    group: gateway.networking.k8s.io
  priority: -1 # Make it lower priority (we will overwrite in the next config)
  configPatches:
  - applyTo: CLUSTER
    patch:
      operation: MERGE
      value:
        http2_protocol_options:
          initial_connection_window_size: 65539
    match:
      context: GATEWAY
  # ADD a new cluster
  - applyTo: CLUSTER
    match:
      context: GATEWAY
    patch:
      operation: ADD
      value:
        name: "lua_cluster_global"
        type: STRICT_DNS
        connect_timeout: 0.5s
        lb_policy: ROUND_ROBIN
        load_assignment:
          cluster_name: lua_cluster
          endpoints:
          - lb_endpoints:
            - endpoint:
                address:
                  socket_address:
                    protocol: TCP
                    address: "internal.org.net"
                    port_value: 8888
`
	serviceFilter := `
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: echo-http2-options
spec:
  targetRefs:
  - kind: ServiceEntry
    name: app
    group: networking.istio.io
  configPatches:
    - applyTo: CLUSTER
      patch:
        operation: MERGE
        value:
          http2_protocol_options:
            initial_stream_window_size: 65536
            initial_connection_window_size: 65536
      match:
        context: GATEWAY
`
	notSelectedAppServiceEntry := `apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: not-app
  namespace: default
  labels:
    istio.io/use-waypoint: waypoint
spec:
  hosts: [not-app.com]
  addresses: [2.3.4.5]
  ports:
  - number: 80
    name: http
    protocol: HTTP
  resolution: STATIC
  workloadSelector:
    labels:
      app: app`

	d, proxy := setupWaypointTest(t,
		waypointGateway, waypointSvc, waypointInstance,
		appServiceEntry, notSelectedAppServiceEntry,
		gatewayFilter, serviceFilter)

	clusters := d.Clusters(proxy)

	// We can add a cluster
	added := xdstest.ExtractCluster("lua_cluster_global", clusters)
	assert.Equal(t, added != nil, true)

	// We matched this via a Service targetRef
	app := xdstest.ExtractCluster("inbound-vip|80|http|app.com", clusters)
	// nolint
	assert.Equal(t, app.GetHttp2ProtocolOptions().GetInitialStreamWindowSize().GetValue(), 65536)
	// nolint
	assert.Equal(t, app.GetHttp2ProtocolOptions().GetInitialConnectionWindowSize().GetValue(), 65536)

	// This one is matched by a Gateway targetRef and NOT the service target ref
	notApp := xdstest.ExtractCluster("inbound-vip|80|http|not-app.com", clusters)
	// nolint
	assert.Equal(t, notApp.GetHttp2ProtocolOptions().GetInitialStreamWindowSize().GetValue(), 0)
	// nolint
	assert.Equal(t, notApp.GetHttp2ProtocolOptions().GetInitialConnectionWindowSize().GetValue(), 65539)
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
