// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant
package peering

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/workloadapi"
)

type servicesForWorkload struct {
	selector map[string]string
	services []serviceForWorkload
}
type serviceForWorkload struct {
	namespace string
	name      string
	ports     []*workloadapi.Port

	local *corev1.Service // maybe nil
}

func (sw serviceForWorkload) federatedName() string {
	return sw.namespace + "/" + sw.name
}

// mergedServicesForWorkload grabs all the Services that this workload
// is a part of and merged selectors that would otherwise fully overlap (subsets)
func (c *NetworkWatcher) mergedServicesForWorkload(workload *RemoteWorkload) []servicesForWorkload {
	var unmerged []servicesForWorkload
	for s, servicePorts := range workload.Services {
		// we want to find something like 'ns/name.ns.mesh.internal'
		_, hostname, ok := strings.Cut(s, "/")
		if !ok {
			// not a valid service?
			continue
		}
		nsname, _, ok := strings.Cut(hostname, DomainSuffix)
		if !ok {
			// Not a peering service
			continue
		}
		svcName, svcNs, ok := strings.Cut(nsname, ".")
		if !ok {
			// not a valid service?
			continue
		}

		labels := map[string]string{}

		// Query the gateway WorkloadEntry from this Workload's cluster to get the scope
		// This ensures consistency even after istiod restarts
		weScope := ServiceScopeGlobal // default to global if WE not found
		weName := fmt.Sprintf("autogen.%v.%v.%v", workload.ClusterId, svcNs, svcName)
		if we := c.workloadEntries.Get(weName, PeeringNamespace); we != nil {
			if scope, ok := we.Labels[ServiceScopeLabel]; ok {
				weScope = scope
			}
		}

		localService := c.services.Get(svcName, svcNs)
		if localService != nil {
			ns := ptr.OrEmpty(kclient.New[*corev1.Namespace](c.client).Get(localService.GetNamespace(), ""))
			scope := CalculateScope(localService.GetLabels(), ns.GetLabels())
			if !IsGlobal(scope) && !c.isGlobalWaypoint(localService) {
				// It's not global, so ignore it
				localService = nil
			} else {
				weScope = scope
				// If the local service exists, the SE is going to set the label selectors to the service's so we can select the Pod
				// We need to include those
				labels = maps.Clone(localService.Spec.Selector)
				if labels == nil {
					labels = map[string]string{}
				}
			}
		}

		// always add parent labels, otherwise we won't be treated as a peering
		// object in and won't get selected
		labels[ParentServiceNamespaceLabel] = svcNs
		labels[ParentServiceLabel] = svcName

		// maybe inherit scope from local service
		// even though this isn't used in the selector, we need the permutations
		// logic to build one for every local value of scope for the services this
		// is a part of
		labels[ServiceScopeLabel] = weScope

		unmerged = append(unmerged, servicesForWorkload{
			selector: labels,
			services: []serviceForWorkload{{
				name:      svcName,
				namespace: svcNs,
				ports:     servicePorts.GetPorts(),

				local: localService,
			}},
		})
	}
	return mergedServicesForWorkload(unmerged)
}

func mergedServicesForWorkload(unmerged []servicesForWorkload) []servicesForWorkload {
	if len(unmerged) == 0 {
		return nil
	}

	// sort by largest selectors first so we can do a single pass that checks if
	// an item is a subset of any other item already in the output
	sort.Slice(unmerged, func(i, j int) bool {
		return len(unmerged[i].selector) > len(unmerged[j].selector)
	})

	var mergedBySelector []servicesForWorkload
	for _, item := range unmerged {
		sel := labels.Set(item.selector).AsSelectorPreValidated()
		didMerge := false
		for i := range mergedBySelector {
			if sel.Matches(labels.Set(mergedBySelector[i].selector)) {
				mergedBySelector[i].services = append(mergedBySelector[i].services, item.services...)
				didMerge = true
			}
		}
		if !didMerge {
			mergedBySelector = append(mergedBySelector, servicesForWorkload{
				selector: item.selector, // no need to clone
				services: item.services,
			})
		}
	}
	return mergedBySelector
}
