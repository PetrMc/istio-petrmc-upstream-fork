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
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/util/sets"
)

type FederatedServiceGenerator struct {
	Server *DiscoveryServer
}

var (
	_ model.XdsResourceGenerator      = &FederatedServiceGenerator{}
	_ model.XdsDeltaResourceGenerator = &FederatedServiceGenerator{}
)

// GenerateDeltas computes Workload resources. This is design to be highly optimized to delta updates,
// and supports *on-demand* client usage. A client can subscribe with a wildcard subscription and get all
// resources (with delta updates), or on-demand and only get responses for specifically subscribed resources.
//
// Incoming requests may be for VIP or Pod IP addresses. However, all responses are Workload resources, which are pod based.
// This means subscribing to a VIP may end up pushing many resources of different name than the request.
// On-demand clients are expected to handle this (for wildcard, this is not applicable, as they don't specify any resources at all).
func (e FederatedServiceGenerator) GenerateDeltas(
	proxy *model.Proxy,
	req *model.PushRequest,
	w *model.WatchedResource,
) (model.Resources, model.DeletedResources, model.XdsLogDetails, bool, error) {
	updatedServices := model.ConfigNamesOfKind(req.ConfigsUpdated, kind.FederatedService)
	isReq := req.IsRequest()
	if len(updatedServices) == 0 && len(req.ConfigsUpdated) > 0 {
		// Nothing changed..
		return nil, nil, model.XdsLogDetails{}, false, nil
	}

	subs := w.ResourceNames
	var services sets.String
	if isReq {
		// this is from request, we only send response for the subscribed address
		// At t0, a client request A, we only send A and additional resources back to the client.
		// At t1, a client request B, we only send B and additional resources back to the client, no A here.
		services = req.Delta.Subscribed
	} else {
		if w.Wildcard {
			services = updatedServices
		} else {
			// this is from the external triggers instead of request
			// send response for all the subscribed intersect with the updated
			services = updatedServices.IntersectInPlace(subs)
		}
	}

	// TODO: it is needlessly wasteful to do a full sync just because the rest of Istio thought it was "full"
	// The rest of Istio xDS types would treat `req.Full && len(req.ConfigsUpdated) == 0` as a need to trigger a "full" push.
	// This is only an escape hatch for a lack of complete mapping of "Input changed -> Output changed".
	// WDS does not suffer this limitation, so we could almost safely ignore these.
	// However, other code will merge "Partial push + Full push -> Full push", so skipping full pushes isn't viable.
	full := (isReq && w.Wildcard) || (!isReq && req.Full && len(req.ConfigsUpdated) == 0)

	// Nothing to do
	if len(services) == 0 && !full {
		if isReq {
			// We need to respond for requests, even if we have nothing to respond with
			return make(model.Resources, 0), nil, model.XdsLogDetails{}, false, nil
		}
		// For NOP pushes, no need
		return nil, nil, model.XdsLogDetails{}, false, nil
	}
	resources := make(model.Resources, 0)
	federatedServices := services
	if full {
		federatedServices = nil
	}
	addrs, removed := e.Server.Env.ServiceDiscovery.FederatedServices(federatedServices)
	// Note: while "removed" is a weird name for a resource that never existed, this is how the spec works:
	// https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol#id2
	have := sets.New[string]()
	for _, addr := range addrs {
		n := addr.ResourceName()
		have.Insert(n)
		resources = append(resources, &discovery.Resource{
			Name:     n,
			Resource: protoconv.MessageToAny(addr.FederatedService), // TODO: pre-marshal
		})
	}

	if full {
		// If it's a full push, AddressInformation won't have info to compute the full set of removals.
		// Instead, we need can see what resources are missing that we were subscribe to; those were removed.
		removed = subs.Difference(have).Merge(removed)
	}

	if !w.Wildcard {
		// For on-demand, we may have requested a VIP but gotten Pod IPs back. We need to update
		// the internal book-keeping to subscribe to the Pods, so that we push updates to those Pods.
		w.ResourceNames = subs.Merge(have)
	} else {
		// For wildcard, we record all resources that have been pushed and not removed
		// It was to correctly calculate removed resources during full push alongside with specific address removed.
		w.ResourceNames = subs.Merge(have).DeleteAllSet(removed)
	}
	return resources, removed.UnsortedList(), model.XdsLogDetails{}, true, nil
}

func (e FederatedServiceGenerator) Generate(
	proxy *model.Proxy,
	w *model.WatchedResource,
	req *model.PushRequest,
) (model.Resources, model.XdsLogDetails, error) {
	resources, _, details, _, err := e.GenerateDeltas(proxy, req, w)
	return resources, details, err
}
