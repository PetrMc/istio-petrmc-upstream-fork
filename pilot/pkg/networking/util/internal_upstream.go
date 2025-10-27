// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	internalupstream "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/internal_upstream/v3"
	rawbuffer "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/raw_buffer/v3"
	metadata "github.com/envoyproxy/go-control-plane/envoy/type/metadata/v3"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/util/protoconv"
)

func RawBufferTransport() *core.TransportSocket {
	return &core.TransportSocket{
		Name:       "raw_buffer",
		ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&rawbuffer.RawBuffer{})},
	}
}

// InternalUpstreamTransportSocket wraps provided transport socket into Envoy InteralUpstreamTransport.
func InternalUpstreamTransportSocket(name string, transport *core.TransportSocket) *core.TransportSocket {
	return &core.TransportSocket{
		Name: name,
		ConfigType: &core.TransportSocket_TypedConfig{
			TypedConfig: protoconv.MessageToAny(
				&internalupstream.InternalUpstreamTransport{
					TransportSocket: transport,
				},
			),
		},
	}
}

// DefaultInternalUpstreamTransportSocket provides an internal_upstream transport that does not passthrough any metadata.
var DefaultInternalUpstreamTransportSocket = InternalUpstreamTransportSocket("internal_upstream", RawBufferTransport())

// WaypointInternalUpstreamTransportSocket builds an internal upstream transport socket suitable for usage in a waypoint
// This will passthrough the OrigDst key and HBONE destination address.
func WaypointInternalUpstreamTransportSocket(inner *core.TransportSocket) *core.TransportSocket {
	if features.EnableEnvoyMultiNetworkHBONE {
		return &core.TransportSocket{
			Name: "internal_upstream",
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
				PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
					{
						Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{Host: &metadata.MetadataKind_Host{}}},
						Name: OriginalDstMetadataKey,
					},
					// In the event we are going cross network we are going to need additional bits of metadata...

					{
						// istio.destination passed from endpoint, to set the CONNECT header
						// This will get translated to io.istio.destination filter state by the next hop.
						Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{
							Host: &metadata.MetadataKind_Host{},
						}},
						Name: "istio",
					},
					{
						// istio.destination passed from endpoint, to select a distinct endpoint on our HBONE pool
						// this ensures we don't mix connections to the wrong host
						Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{
							Host: &metadata.MetadataKind_Host{},
						}},
						Name: "envoy.lb",
					},
					//{
					//	Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Cluster_{
					//		Cluster: &metadata.MetadataKind_Cluster{},
					//	}},
					//	Name: "envoy.lb",
					//},
				},

				TransportSocket: inner,
			})},
		}
	}
	return &core.TransportSocket{
		Name: "internal_upstream",
		ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
			PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{Host: &metadata.MetadataKind_Host{}}},
					Name: OriginalDstMetadataKey,
				},
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{Host: &metadata.MetadataKind_Host{}}},
					Name: "istio",
				},
			},

			TransportSocket: inner,
		})},
	}
}

// FullMetadataPassthroughInternalUpstreamTransportSocket builds an internal upstream transport socket suitable for usage in
// originating HBONE. For waypoints, use WaypointInternalUpstreamTransportSocket.
func FullMetadataPassthroughInternalUpstreamTransportSocket(inner *core.TransportSocket) *core.TransportSocket {
	if features.EnableEnvoyMultiNetworkHBONE {
		return &core.TransportSocket{
			Name: "envoy.transport_sockets.internal_upstream",
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
				PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
					{
						Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{}},
						Name: OriginalDstMetadataKey,
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
						Name: "envoy.lb",
					},
					{
						Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Cluster_{
							Cluster: &metadata.MetadataKind_Cluster{},
						}},
						Name: "envoy.lb",
					},
				},
				TransportSocket: inner,
			})},
		}
	}
	return &core.TransportSocket{
		Name: "envoy.transport_sockets.internal_upstream",
		ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
			PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
				{
					Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{}},
					Name: OriginalDstMetadataKey,
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
			},
			TransportSocket: inner,
		})},
	}
}
