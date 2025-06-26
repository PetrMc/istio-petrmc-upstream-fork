// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package endpoints

import (
	"fmt"
	"strings"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/structpb"
	wrappers "google.golang.org/protobuf/types/known/wrapperspb"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	labelutil "istio.io/istio/pilot/pkg/serviceregistry/util/label"
	kubelabels "istio.io/istio/pkg/kube/labels"
)

// buildCrossNetworkHBONEEndpoint is used to create the endpoint initiating a cross-network HBONE call.
// This will directly send to the inner-HBONE origination listener/cluster, which will wrap it in the outer HBONE layer.
func (b *EndpointBuilder) buildCrossNetworkHBONEEndpoint(
	gw model.NetworkGateway,
	epWeight uint32,
	maybeIstioEndpoint *model.IstioEndpoint,
) (*model.IstioEndpoint, *endpoint.LbEndpoint) {
	// Generate a fake IstioEndpoint to carry network and cluster information.
	gwIstioEp := &model.IstioEndpoint{
		Network: gw.Network,
		Locality: model.Locality{
			ClusterID: gw.Cluster,
		},
		Labels: labelutil.AugmentLabels(nil, gw.Cluster, "", "", gw.Network),
	}

	var canonicalName, canonicalRevision string
	if maybeIstioEndpoint != nil {
		canonicalName, canonicalRevision = kubelabels.CanonicalService(maybeIstioEndpoint.Labels, maybeIstioEndpoint.WorkloadName)

		// don't bother sending the default value in config
		if canonicalRevision == "latest" {
			canonicalRevision = ""
		}
	}

	var sb strings.Builder
	sb.WriteString(";")
	sb.WriteString(b.service.Attributes.Namespace)
	sb.WriteString(";")
	sb.WriteString(canonicalName)
	sb.WriteString(";")
	sb.WriteString(canonicalRevision)
	sb.WriteString(";")
	sb.WriteString(gw.Network.String())

	// Generate the EDS endpoint for this gateway.
	gwEp := &endpoint.LbEndpoint{
		HostIdentifier: &endpoint.LbEndpoint_Endpoint{
			Endpoint: &endpoint.Endpoint{
				Address: util.BuildInternalAddressWithIdentifier("connect_network_originate_inner", fmt.Sprintf("%s:%d/%s", b.hostname, b.port, gw.Addr)),
			},
		},
		LoadBalancingWeight: &wrappers.UInt32Value{
			Value: epWeight,
		},
		Metadata: &core.Metadata{FilterMetadata: make(map[string]*structpb.Struct)},
	}
	gwEp.Metadata.FilterMetadata[util.OriginalDstMetadataKey] = BuildTunnelMetadataStructNetwork(fmt.Sprintf("%s:%d", gw.Addr, gw.Port))
	gwEp.Metadata.FilterMetadata[util.EnvoyTransportSocketMetadataKey] = &structpb.Struct{
		Fields: map[string]*structpb.Value{
			model.TunnelLabelShortName: {Kind: &structpb.Value_StringValue{StringValue: model.TunnelHTTP}},
		},
	}
	// Pass metadata through to let the connect_network_originate_inner determine the HBONE hostname to use
	gwEp.Metadata.FilterMetadata[util.IstioMetadataKey] = &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"destination": structpb.NewStringValue(fmt.Sprintf("%s:%d", b.hostname, b.port)),
			"workload":    structpb.NewStringValue(sb.String()),
		},
	}
	// We need to have a distinct tunnel per destination so we don't mix up connections.
	// We do this by selecting a specific endpoint on the connect_network_originate_inner cluster.
	//
	// This is fairly subtle.
	// Typically, you use endpoint metadata to as the thing we match against with a filter.
	// Ex: the route says "give me destination:x" and it would match this endpoint.
	//
	// Here, the logic is inverted.
	// We passthrough the endpoint metadata into the internal listener, which makes it the filter.
	// So normally dynamicMetadata is the selector and endpoint.metadata is the selectee, but now we copy endpoint meta to
	// dynamic meta so this becomes the selector for the next hop!
	gwEp.Metadata.FilterMetadata["envoy.lb"] = &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"destination": structpb.NewStringValue(fmt.Sprintf("%s:%d", b.hostname, b.port)),
		},
	}

	return gwIstioEp, gwEp
}

func hboneMultiNetworkEnabled(b *EndpointBuilder, gateways []model.NetworkGateway, istioEndpoint *model.IstioEndpoint) bool {
	if !features.EnableEnvoyMultiNetworkHBONE {
		return false
	}
	// An endpoint can do HBONE multi-network if there is an HBONE gateway and the endpoint supports HBONE
	if !supportTunnel(b, istioEndpoint) {
		return false
	}
	for _, gw := range gateways {
		if gw.HBONEPort != 0 {
			return true
		}
	}
	return false
}

func BuildTunnelMetadataStructNetwork(gatewayAddress string) *structpb.Struct {
	m := map[string]interface{}{
		// logical destination behind the tunnel, on which policy and telemetry will be applied
		"local": gatewayAddress,
	}
	st, _ := structpb.NewStruct(m)
	return st
}

func maybeAddMultinetworkMetadata(b *EndpointBuilder, ep *endpoint.LbEndpoint) {
	if b.proxy.IsWaypointProxy() {
		// For waypoint proxies, we need the ability to filter down to only same-network clusters
		// Add the metadata to this endpoint to declare it as such
		// Note: another code path marks cross-network endpoints as cross-network=true, so this unconditionally marks them as false.
		// Since we only need this on waypoints we skip it on other proxies to avoid any issues.
		ep.Metadata.FilterMetadata["envoy.lb"] = notCrossNetworkMetadata
	}
}

var notCrossNetworkMetadata = &structpb.Struct{
	Fields: map[string]*structpb.Value{
		"cross-network": structpb.NewBoolValue(false),
	},
}
