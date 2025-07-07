// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package core

import (
	"fmt"
	"net"
	"strconv"
	"time"

	xds "github.com/cncf/xds/go/xds/core/v3"
	matcher "github.com/cncf/xds/go/xds/type/matcher/v3"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matching "github.com/envoyproxy/go-control-plane/envoy/extensions/common/matching/v3"
	sfsvalue "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	sfs "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/set_filter_state/v3"
	smd "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/set_metadata/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	sfsnetwork "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	tcp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	network "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	internalupstream "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/internal_upstream/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	metadata "github.com/envoyproxy/go-control-plane/envoy/type/metadata/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	wrappers "google.golang.org/protobuf/types/known/wrapperspb"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	istionetworking "istio.io/istio/pilot/pkg/networking"
	"istio.io/istio/pilot/pkg/networking/core/match"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/util/protoconv"
	xdsfilters "istio.io/istio/pilot/pkg/xds/filters"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	sec_model "istio.io/istio/pkg/model"
	"istio.io/istio/pkg/util/protomarshal"
	"istio.io/istio/pkg/wellknown"
)

// nolint: lll
/*
This file provides support for Double HBONE origination from Envoy clients (sidecar, waypoint, and ingress).
This datapath is quite complicated.
In envoy, each tunnel requires a hop through an internal listener.
For double HBONE, we have 2 tunnel layers, so 2 internal listeners.

A given request needs to know the target service on the remote network, and the network gateway address.
This information needs to be passed through the internal listener hops.

To avoid bloated configuration, we choose to make the tunnel clusters ORIGINAL_DST.
The full flow looks something like so:
* Standard service cluster (`outbound|80||example`) has an endpoint representing the remote gateway. For example:

```yaml
  endpoint:
    address:
      envoyInternalAddress:
        endpointId: echo.default.mesh.internal:80
        serverListenerName: connect_network_originate_inner
  metadata:
    filterMetadata:
      envoy.filters.listener.original_dst:
        local: <gateway IP address>:15008
      envoy.transport_socket_match:
        tunnel: http
      istio:
        destination: echo.default.mesh.internal:80
  ```

The envoyInternalAddress represents the next hop -- in this case, a constant `connect_network_originate_inner` which is what will build the inner tunnel.
The metadata is critical:
* `tunnel` tells us to use the transport_socket_matcher that will allowing sending to the upstream listener (with appropriate metadata passing through)
* `istio:destination` will be read by the `connect_network_originate_inner` listener `tunneling_config.hostname`.
* `envoy.filters.listener.original_dst:local` will be passed through to the final `ORIGINAL_DST` cluster to program where to go. This gets passed through 2 internal listeners!

* Next we get to the `connect_network_originate_inner` listener. This will originate a CONNECT to `istio:destination` (somewhat indirectly; we translate the dynamic metadata to filter state metadata first. Its quite likely there is a simpler way to do this) and program the listener to passthrough the `envoy.filters.listener.original_dst:local` from the previous hop. This is statically configured to always send to `connect_network_originate_outer`, with TLS.
* Finally we get to the `connect_network_originate_outer` listener. This will originate a CONNECT to `envoy.filters.listener.original_dst:local` (this is the gateway address, passed through each internal listener) with TLS.
*/

const (
	ConnectOriginateOuter = "connect_network_originate_outer"
	ConnectOriginateInner = "connect_network_originate_inner"

	// MainInternalFromRemoteNetworkName is the name for the copy of main_internal that filters to only same-network endpoints
	MainInternalFromRemoteNetworkName = "main_internal_from_remote_network"
)

func (cb *ClusterBuilder) buildConnectOriginateInner(proxy *model.Proxy, push *model.PushContext, services []*model.Service) *cluster.Cluster {
	ctx := buildCommonConnectTLSContext(proxy, push)
	// Compliance for Envoy tunnel upstreams.
	sec_model.EnforceCompliance(ctx)

	lbEps := []*endpoint.LbEndpoint{}
	for _, svc := range services {
		for _, port := range svc.Ports {
			hp := fmt.Sprintf("%s:%d", svc.Hostname, port.Port)
			ep := &endpoint.LbEndpoint{
				HostIdentifier: &endpoint.LbEndpoint_Endpoint{
					Endpoint: &endpoint.Endpoint{
						Address: util.BuildInternalAddressWithIdentifier(ConnectOriginateOuter, hp),
					},
				},
				LoadBalancingWeight: &wrappers.UInt32Value{
					Value: 1,
				},
				Metadata: &core.Metadata{
					FilterMetadata: map[string]*structpb.Struct{
						"envoy.lb": {
							Fields: map[string]*structpb.Value{
								"destination": structpb.NewStringValue(hp),
							},
						},
					},
				},
			}
			lbEps = append(lbEps, ep)
		}
	}
	c := &cluster.Cluster{
		Name:                 ConnectOriginateInner,
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STATIC},
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: ConnectOriginateInner,
			Endpoints: []*endpoint.LocalityLbEndpoints{{
				Locality:    &core.Locality{},
				LbEndpoints: lbEps,
			}},
		},

		// We need to select the specific endpoint so we don't accidentally share a connection when calling different services
		LbSubsetConfig: &cluster.Cluster_LbSubsetConfig{
			SubsetSelectors: []*cluster.Cluster_LbSubsetConfig_LbSubsetSelector{{
				Keys:                []string{"destination"},
				SingleHostPerSubset: true,
			}},
			FallbackPolicy: cluster.Cluster_LbSubsetConfig_NO_FALLBACK,
			// LocalityWeightAware: true, // This seems to make things crash
		},
		ConnectTimeout:                protomarshal.Clone(cb.req.Push.Mesh.ConnectTimeout),
		CleanupInterval:               durationpb.New(60 * time.Second),
		CircuitBreakers:               &cluster.CircuitBreakers{Thresholds: []*cluster.CircuitBreakers_Thresholds{getDefaultCircuitBreakerThresholds()}},
		TypedExtensionProtocolOptions: h2connectUpgrade(),
		TransportSocket: internalUpstreamInnerConnectSocket(&core.TransportSocket{
			Name: "tls",
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&tlsv3.UpstreamTlsContext{
				CommonTlsContext: ctx,
			})},
		}),
	}

	c.AltStatName = util.DelimitedStatsPrefix(ConnectOriginateInner)

	return c
}

// internalUpstreamInnerConnectSocket is like internalUpstreamSocket but also passes "envoy.lb" through
func internalUpstreamInnerConnectSocket(inner *core.TransportSocket) *core.TransportSocket {
	return &core.TransportSocket{
		Name: "envoy.transport_sockets.internal_upstream",
		ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
			PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{}},
					Name: util.OriginalDstMetadataKey,
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Cluster_{
						Cluster: &metadata.MetadataKind_Cluster{},
					}},
					Name: "istio",
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{
						Host: &metadata.MetadataKind_Host{},
					}},
					Name: "istio",
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{
						Host: &metadata.MetadataKind_Host{},
					}},
					// To passhthrough the "destination" subset selector
					Name: "envoy.lb",
				},
			},
			TransportSocket: inner,
		})},
	}
}

func (cb *ClusterBuilder) buildConnectOriginateOuter(proxy *model.Proxy, push *model.PushContext) *cluster.Cluster {
	ctx := buildCommonConnectTLSContext(proxy, push)

	// Compliance for Envoy tunnel upstreams.
	sec_model.EnforceCompliance(ctx)
	c := &cluster.Cluster{
		Name:                          ConnectOriginateOuter,
		ClusterDiscoveryType:          &cluster.Cluster_Type{Type: cluster.Cluster_ORIGINAL_DST},
		LbPolicy:                      cluster.Cluster_CLUSTER_PROVIDED,
		ConnectTimeout:                protomarshal.Clone(cb.req.Push.Mesh.ConnectTimeout),
		CleanupInterval:               durationpb.New(60 * time.Second),
		CircuitBreakers:               &cluster.CircuitBreakers{Thresholds: []*cluster.CircuitBreakers_Thresholds{getDefaultCircuitBreakerThresholds()}},
		TypedExtensionProtocolOptions: h2connectUpgrade(),
		LbConfig: &cluster.Cluster_OriginalDstLbConfig_{
			OriginalDstLbConfig: &cluster.Cluster_OriginalDstLbConfig{
				UpstreamPortOverride: &wrappers.UInt32Value{
					Value: model.HBoneInboundListenPort,
				},
			},
		},
		TransportSocket: &core.TransportSocket{
			Name: "tls",
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&tlsv3.UpstreamTlsContext{
				CommonTlsContext: ctx,
			})},
		},
	}

	c.AltStatName = util.DelimitedStatsPrefix(ConnectOriginateOuter)

	return c
}

const baggageFormat = "k8s.cluster.name=%s,k8s.namespace.name=%s,k8s.%s.name=%s,service.name=%s,service.version=%s"

func baggage(proxy *model.Proxy) string {
	canonicalName := proxy.Labels[model.IstioCanonicalServiceLabelName]
	canonicalRevision := proxy.Labels[model.IstioCanonicalServiceRevisionLabelName]
	b := fmt.Sprintf(baggageFormat,
		proxy.Metadata.ClusterID, proxy.ConfigNamespace,
		// TODO do not hardcode deployment. But I think we ignore it anyways?
		"deployment", proxy.Metadata.WorkloadName,
		canonicalName, canonicalRevision,
	)
	return b
}

func buildConnectOriginateInnerListener(push *model.PushContext, proxy *model.Proxy, class istionetworking.ListenerClass) *listener.Listener {
	tcpProxy := &tcp.TcpProxy{
		StatPrefix:       ConnectOriginateInner,
		ClusterSpecifier: &tcp.TcpProxy_Cluster{Cluster: ConnectOriginateInner},
		TunnelingConfig: &tcp.TcpProxy_TunnelingConfig{
			Hostname: "%DYNAMIC_METADATA(istio:destination)%",
			HeadersToAdd: []*core.HeaderValueOption{
				{
					Header: &core.HeaderValue{
						Key:   "x-istio-source",
						Value: toSourceHeader(proxy),
					},
				},
				{
					Header: &core.HeaderValue{
						Key:   "x-forwarded-network",
						Value: proxy.Metadata.Network.String(),
					},
				},
				{
					Header: &core.HeaderValue{
						Key:   "baggage",
						Value: baggage(proxy),
					},
				},
			},
		},
	}
	// Set access logs. These are filtered down to only connection establishment errors, to avoid double logs in most cases.
	accessLogBuilder.setHboneOriginationAccessLog(push, proxy, tcpProxy, class)
	l := &listener.Listener{
		Name:              ConnectOriginateInner,
		UseOriginalDst:    wrappers.Bool(false),
		ListenerSpecifier: &listener.Listener_InternalListener{InternalListener: &listener.Listener_InternalListenerConfig{}},
		ListenerFilters: []*listener.ListenerFilter{
			xdsfilters.OriginalDestination,
		},
		FilterChains: []*listener.FilterChain{
			{
				Filters: []*listener.Filter{
					connectOriginateInnerMetadataPropagation,
					{
						Name: wellknown.TCPProxy,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: protoconv.MessageToAny(tcpProxy),
						},
					},
				},
			},
		},
	}
	accessLogBuilder.setListenerAccessLog(push, proxy, l, class)
	return l
}

func toSourceHeader(proxy *model.Proxy) string {
	switch proxy.Type {
	case model.SidecarProxy:
		return "sidecar"
	case model.Router:
		return "gateway"
	case model.Waypoint:
		return "waypoint"
	}
	panic("unknown proxy type " + proxy.Type)
}

func buildConnectOriginateOuterListener(push *model.PushContext, proxy *model.Proxy, class istionetworking.ListenerClass) *listener.Listener {
	tcpProxy := &tcp.TcpProxy{
		StatPrefix:       ConnectOriginateOuter,
		ClusterSpecifier: &tcp.TcpProxy_Cluster{Cluster: ConnectOriginateOuter},
		TunnelingConfig: &tcp.TcpProxy_TunnelingConfig{
			Hostname: "%FILTER_STATE(io.istio.destination:PLAIN)%",
			HeadersToAdd: []*core.HeaderValueOption{
				{
					Header: &core.HeaderValue{
						Key:   "x-istio-source",
						Value: toSourceHeader(proxy),
					},
				},
				{
					Header: &core.HeaderValue{
						Key:   "x-forwarded-network",
						Value: proxy.Metadata.Network.String(),
					},
				},
			},
		},
	}
	// Set access logs. These are filtered down to only connection establishment errors, to avoid double logs in most cases.
	accessLogBuilder.setHboneOriginationAccessLog(push, proxy, tcpProxy, class)
	l := &listener.Listener{
		Name:              ConnectOriginateOuter,
		UseOriginalDst:    wrappers.Bool(false),
		ListenerSpecifier: &listener.Listener_InternalListener{InternalListener: &listener.Listener_InternalListenerConfig{}},
		ListenerFilters: []*listener.ListenerFilter{
			xdsfilters.OriginalDestination,
		},
		FilterChains: []*listener.FilterChain{{
			Filters: []*listener.Filter{{
				Name: wellknown.TCPProxy,
				ConfigType: &listener.Filter_TypedConfig{
					TypedConfig: protoconv.MessageToAny(tcpProxy),
				},
			}},
		}},
	}
	accessLogBuilder.setListenerAccessLog(push, proxy, l, class)
	return l
}

var connectOriginateInnerMetadataPropagation = &listener.Filter{
	Name: ConnectOriginateInner + "_metadata_propagation",
	ConfigType: &listener.Filter_TypedConfig{
		TypedConfig: protoconv.MessageToAny(&sfsnetwork.Config{
			OnNewConnection: []*sfsvalue.FilterStateValue{
				{
					Key: &sfsvalue.FilterStateValue_ObjectKey{
						ObjectKey: "envoy.filters.listener.original_dst.local_ip",
					},
					Value: &sfsvalue.FilterStateValue_FormatString{
						FormatString: &core.SubstitutionFormatString{
							Format: &core.SubstitutionFormatString_TextFormatSource{
								TextFormatSource: &core.DataSource{
									Specifier: &core.DataSource_InlineString{
										InlineString: "%DOWNSTREAM_LOCAL_ADDRESS%",
									},
								},
							},
						},
					},
					SharedWithUpstream: sfsvalue.FilterStateValue_TRANSITIVE,
				},
				{
					Key: &sfsvalue.FilterStateValue_ObjectKey{
						ObjectKey: "io.istio.destination",
					},
					FactoryKey: "envoy.string",
					Value: &sfsvalue.FilterStateValue_FormatString{
						FormatString: &core.SubstitutionFormatString{
							Format: &core.SubstitutionFormatString_TextFormatSource{
								TextFormatSource: &core.DataSource{
									Specifier: &core.DataSource_InlineString{
										InlineString: "%DYNAMIC_METADATA(istio:destination)%",
									},
								},
							},
						},
					},
					SharedWithUpstream: sfsvalue.FilterStateValue_ONCE,
				},
			},
		}),
	},
}

func applyMultinetworkSubset(c *cluster.Cluster) {
	if !features.EnableEnvoyMultiNetworkHBONE {
		return
	}
	if c.GetType() != cluster.Cluster_EDS {
		return
	}
	// Conditionally enable localityWeightAware based on if we are using locality weighting
	// If we set localityWeightAware without locality weighting, Envoy rejects.
	// If we don't set it, envoy ignores the locality weights.
	localityWeightAware := false
	if c.GetCommonLbConfig().GetLocalityWeightedLbConfig() != nil {
		localityWeightAware = true
	}
	c.LbSubsetConfig = &cluster.Cluster_LbSubsetConfig{
		SubsetSelectors: []*cluster.Cluster_LbSubsetConfig_LbSubsetSelector{
			{Keys: []string{"cross-network"}},
		},
		// We want to be able to filter to cross-network=false, but not require it
		FallbackPolicy:      cluster.Cluster_LbSubsetConfig_ANY_ENDPOINT,
		LocalityWeightAware: localityWeightAware,
	}
}

func appendServiceSpecificHBONEVhosts(vhosts []*route.VirtualHost, lb *ListenerBuilder, svcs []*model.Service) []*route.VirtualHost {
	if !features.EnableEnvoyMultiNetworkHBONE {
		return vhosts
	}
	if svcs == nil {
		// This is a sidecar, find all the services attached to us
		myIP := lb.node.IPAddresses[0]
		for _, st := range lb.node.ServiceTargets {
			match := net.JoinHostPort(string(st.Service.Hostname), strconv.Itoa(st.Port.ServicePort.Port))
			vhost := lb.createPerServiceHBONERoute(myIP, int(st.Port.TargetPort), match)
			vhosts = append(vhosts, vhost)
		}
	} else {
		// This is a waypoint
		for _, svc := range svcs {
			for _, port := range svc.Ports {
				match := net.JoinHostPort(string(svc.Hostname), strconv.Itoa(port.Port))
				vhost := lb.createPerServiceHBONERoute(svc.GetAddressForProxy(lb.node), port.Port, match)
				vhosts = append(vhosts, vhost)
			}
		}
	}
	return vhosts
}

func (lb *ListenerBuilder) createPerServiceHBONERoute(targetIP string, targetPort int, match string) *route.VirtualHost {
	tpfc := map[string]*anypb.Any{
		xdsfilters.ConnectAuthorityFilter.Name: protoconv.MessageToAny(&route.FilterConfig{
			// The ability to run set_filter_state as a per-route filter was added in Istio 1.25 (https://github.com/envoyproxy/envoy/pull/37507)
			// We backported it internally in a 1.24.x patch release.
			// Mark it as optional since we may be serving older clients.
			// If it is not present then users will just not be able to serve multi-network traffic for those proxies (sidecar inbound and waypoints).
			IsOptional: true,
			Config: protoconv.MessageToAny(&sfs.Config{
				OnRequestHeaders: []*sfsvalue.FilterStateValue{
					{
						Key: &sfsvalue.FilterStateValue_ObjectKey{
							ObjectKey: "envoy.filters.listener.original_dst.local_ip",
						},
						Value: &sfsvalue.FilterStateValue_FormatString{
							FormatString: &core.SubstitutionFormatString{
								Format: &core.SubstitutionFormatString_TextFormatSource{
									TextFormatSource: &core.DataSource{
										Specifier: &core.DataSource_InlineString{
											InlineString: net.JoinHostPort(targetIP, strconv.Itoa(targetPort)),
										},
									},
								},
							},
						},
						SharedWithUpstream: sfsvalue.FilterStateValue_ONCE,
					},

					{
						Key: &sfsvalue.FilterStateValue_ObjectKey{
							ObjectKey: "io.istio.destination",
						},
						FactoryKey: "envoy.string",
						Value: &sfsvalue.FilterStateValue_FormatString{
							FormatString: &core.SubstitutionFormatString{
								Format: &core.SubstitutionFormatString_TextFormatSource{
									TextFormatSource: &core.DataSource{
										Specifier: &core.DataSource_InlineString{
											InlineString: "%DYNAMIC_METADATA(istio:destination)%",
										},
									},
								},
							},
						},
						SharedWithUpstream: sfsvalue.FilterStateValue_ONCE,
					},
					{
						Key: &sfsvalue.FilterStateValue_ObjectKey{
							ObjectKey: "io.istio.disable_cross_network",
						},
						FactoryKey: "envoy.string",
						Value: &sfsvalue.FilterStateValue_FormatString{
							FormatString: &core.SubstitutionFormatString{
								Format: &core.SubstitutionFormatString_TextFormatSource{
									TextFormatSource: &core.DataSource{
										Specifier: &core.DataSource_InlineString{
											InlineString: "true",
										},
									},
								},
							},
						},
						SharedWithUpstream: sfsvalue.FilterStateValue_ONCE,
					},
				},
			}),
		}),
	}
	vhost := &route.VirtualHost{
		Name:                 "service-" + match,
		Domains:              []string{match},
		TypedPerFilterConfig: tpfc,

		Routes: buildConnectRoutes(lb.node),
	}
	return vhost
}

func buildConnectRoutes(node *model.Proxy) []*route.Route {
	upgrades := []*route.RouteAction_UpgradeConfig{{
		UpgradeType:   ConnectUpgradeType,
		ConnectConfig: &route.RouteAction_UpgradeConfig_ConnectConfig{},
	}}
	routes := make([]*route.Route, 0, 2)
	if node.IsWaypointProxy() && features.EnableEnvoyMultiNetworkHBONE {
		// For waypoint proxies, we need to insert an additional match for traffic from another network
		// This is to ensure we only send to local endpoints, and don't bound back to another network
		routes = append(routes, &route.Route{
			Match: &route.RouteMatch{
				PathSpecifier: &route.RouteMatch_ConnectMatcher_{ConnectMatcher: &route.RouteMatch_ConnectMatcher{}},
				Headers: []*route.HeaderMatcher{{
					// In theory, we should probably check this does not equal our local network.
					// However, we plausibly could have a state where each cluster does not have a consistent network naming scheme.
					// For now, we will just only send this header when we do cross-network requests so we can just do a presence check.
					Name:                 "x-forwarded-network",
					HeaderMatchSpecifier: &route.HeaderMatcher_PresentMatch{PresentMatch: true},
				}},
			},
			Action: &route.Route_Route{Route: &route.RouteAction{
				UpgradeConfigs:   upgrades,
				ClusterSpecifier: &route.RouteAction_Cluster{Cluster: MainInternalFromRemoteNetworkName},
			}},
		})
	}
	routes = append(routes, &route.Route{
		Match: &route.RouteMatch{
			PathSpecifier: &route.RouteMatch_ConnectMatcher_{ConnectMatcher: &route.RouteMatch_ConnectMatcher{}},
		},
		Action: &route.Route_Route{Route: &route.RouteAction{
			UpgradeConfigs:   upgrades,
			ClusterSpecifier: &route.RouteAction_Cluster{Cluster: MainInternalName},
		}},
	})
	return routes
}

// buildCrossNetworkMainInternalUpstreamCluster builds a single endpoint cluster to the internal listener.
func buildCrossNetworkMainInternalUpstreamCluster() *cluster.Cluster {
	c := &cluster.Cluster{
		Name:                 MainInternalFromRemoteNetworkName,
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STATIC},
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: MainInternalFromRemoteNetworkName,
			Endpoints:   util.BuildInternalEndpoint(MainInternalName, nil),
		},
		Metadata: &core.Metadata{FilterMetadata: map[string]*structpb.Struct{
			"envoy.lb": {
				Fields: map[string]*structpb.Value{
					"cross-network": structpb.NewBoolValue(false),
				},
			},
		}},
		TransportSocket: &core.TransportSocket{
			Name: "internal_upstream",
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
				PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
					{
						Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Cluster_{
							Cluster: &metadata.MetadataKind_Cluster{},
						}},
						Name: "envoy.lb",
					},
				},
				TransportSocket: util.RawBufferTransport(),
			})},
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			v3.HttpProtocolOptionsType: passthroughHttpProtocolOptions,
		},
	}

	c.AltStatName = util.DelimitedStatsPrefix(MainInternalFromRemoteNetworkName)

	return c
}

// GetMainInternalWaypointCluster builds the main_internal for waypoints.
func GetMainInternalWaypointCluster() *cluster.Cluster {
	// Use the default one as a base...
	c := GetMainInternalCluster()
	// For multi-network, we need to have additional metadata passed through
	if features.EnableEnvoyMultiNetworkHBONE {
		c.TransportSocket = WaypointInternalUpstreamTransportSocketWithMultiNetwork(util.RawBufferTransport())
	}
	return c
}

func WaypointInternalUpstreamTransportSocketWithMultiNetwork(inner *core.TransportSocket) *core.TransportSocket {
	// Passthrough additional metadata
	return &core.TransportSocket{
		Name: "internal_upstream",
		ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
			PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{}},
					Name: util.OriginalDstMetadataKey,
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Cluster_{
						Cluster: &metadata.MetadataKind_Cluster{},
					}},
					Name: "istio",
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{
						Host: &metadata.MetadataKind_Host{},
					}},
					Name: "envoy.lb",
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Cluster_{
						Cluster: &metadata.MetadataKind_Cluster{},
					}},
					Name: "envoy.lb",
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{
						Host: &metadata.MetadataKind_Host{},
					}},
					Name: "istio",
				},
			},
			TransportSocket: inner,
		})},
	}
}

var CrossNetworkFilterStateInput = &xds.TypedExtensionConfig{
	Name: "cross-network-filter-state",
	TypedConfig: protoconv.MessageToAny(&network.FilterStateInput{
		Key: "io.istio.disable_cross_network",
	}),
}

func notMultinetworkMatcher() *matcher.Matcher {
	sp := &matcher.Matcher_MatcherList_Predicate_SinglePredicate_{
		SinglePredicate: &matcher.Matcher_MatcherList_Predicate_SinglePredicate{
			Input: CrossNetworkFilterStateInput,
			Matcher: &matcher.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{
				ValueMatch: &matcher.StringMatcher{
					MatchPattern: &matcher.StringMatcher_Exact{Exact: "true"},
				},
			},
		},
	}
	return &matcher.Matcher{
		MatcherType: &matcher.Matcher_MatcherList_{
			MatcherList: &matcher.Matcher_MatcherList{
				Matchers: []*matcher.Matcher_MatcherList_FieldMatcher{{
					Predicate: &matcher.Matcher_MatcherList_Predicate{
						MatchType: &matcher.Matcher_MatcherList_Predicate_NotMatcher{
							NotMatcher: &matcher.Matcher_MatcherList_Predicate{MatchType: sp},
						},
					},
					OnMatch: match.ToSkipFilter(),
				}},
			},
		},
	}
}

// nolint: staticcheck
// waypointDisableCrossNetwork conditionally sets the envoy.lb cross-network=false metadata.
// This allows a waypoint to filter out cross-network calls when it gets a call that already traversed *into* this network,
// avoiding back-and-forth across networks.
// While we pass this through as dynamic metadata across the internal listener, Envoy has a different HTTP and TCP dynamic metadata,
// leading to it being ignored for the HTTP context.
// As such, we utilize a filter state match to selectively activate the filter.
var waypointDisableCrossNetwork = &hcm.HttpFilter{
	Name: "set_envoy_lb",
	ConfigType: &hcm.HttpFilter_TypedConfig{
		TypedConfig: protoconv.MessageToAny(&matching.ExtensionWithMatcher{
			XdsMatcher: notMultinetworkMatcher(),
			ExtensionConfig: &core.TypedExtensionConfig{
				Name: "set-cross-network",
				TypedConfig: protoconv.MessageToAny(&smd.Config{
					MetadataNamespace: "envoy.lb",
					Value: &structpb.Struct{
						Fields: map[string]*structpb.Value{
							"cross-network": structpb.NewBoolValue(false),
						},
					},
				}),
			},
		}),
	},
}
