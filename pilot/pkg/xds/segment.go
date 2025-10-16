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

package xds

import (
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/protoconv"
	"istio.io/istio/pkg/workloadapi"
	"istio.io/istio/platform/discovery/peering"
	soloapi "istio.io/istio/soloapi/v1alpha1"
)

type SegmentGenerator struct {
	Server          *DiscoveryServer
	SystemNamespace string
}

var (
	_ model.XdsResourceGenerator      = &SegmentGenerator{}
	_ model.XdsDeltaResourceGenerator = &SegmentGenerator{}
)

// GenerateDeltas generates the local cluster's Segment configuration to send to peer clusters
func (e SegmentGenerator) GenerateDeltas(
	proxy *model.Proxy,
	req *model.PushRequest,
	w *model.WatchedResource,
) (model.Resources, model.DeletedResources, model.XdsLogDetails, bool, error) {
	// For peering, we only send segment info to peer clusters
	if !proxy.Metadata.PeeringMode {
		return nil, nil, model.XdsLogDetails{}, false, nil
	}

	// Get the local cluster's segment information
	segmentInfo := e.Server.Env.ServiceDiscovery.GetSegmentInfo()

	var segment *soloapi.Segment
	if segmentInfo == nil || segmentInfo.Segment == nil {
		// No segment configured, use default
		defaultSeg := peering.DefaultSegment
		segment = &defaultSeg
	} else {
		segment = segmentInfo.Segment
	}

	// Ensure domain doesn't start with '.'
	domain := segment.Spec.Domain
	if len(domain) > 0 && domain[0] == '.' {
		domain = domain[1:]
	}

	resources := model.Resources{
		&discovery.Resource{
			Name: peering.ClusterSegmentResourceName,
			Resource: protoconv.MessageToAny(&workloadapi.Segment{
				Name:   segment.Name,
				Domain: domain,
			}),
		},
	}

	// Update watched resources
	if !w.Wildcard {
		w.ResourceNames = w.ResourceNames.Insert(peering.ClusterSegmentResourceName)
	}

	return resources, nil, model.XdsLogDetails{}, true, nil
}

func (e SegmentGenerator) Generate(
	proxy *model.Proxy,
	w *model.WatchedResource,
	req *model.PushRequest,
) (model.Resources, model.XdsLogDetails, error) {
	resources, _, details, _, err := e.GenerateDeltas(proxy, req, w)
	return resources, details, err
}
