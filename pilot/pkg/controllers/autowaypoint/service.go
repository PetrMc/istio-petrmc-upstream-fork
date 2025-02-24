// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package autowaypoint

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/kube/krt"
)

// serviceX is a common type for Kubernetes service and Istio serviceEntry
// plural is serviceXes
// it is used to track service-like resources that use waypoints
type serviceX struct {
	name         string
	namespace    string
	resourceType schema.GroupVersionResource
}

var _ krt.ResourceNamer = serviceX{}

func (s serviceX) ResourceName() string {
	return fmt.Sprintf("%s/%s", s.namespace, s.name)
}

func serviceXesUsingAutoWaypoint(s krt.Collection[*corev1.Service],
	se krt.Collection[*networkingv1.ServiceEntry],
	opts krt.OptionsBuilder,
) (krt.Collection[serviceX], krt.Index[string, serviceX]) {
	servicesUsingAutoWaypoint := krt.NewCollection(s, func(ctx krt.HandlerContext, s *corev1.Service) *serviceX {
		svcLabels := labels.Set(s.GetLabels())
		if requestsAutoWaypoint(svcLabels) {
			return &serviceX{
				name:         s.Name,
				namespace:    s.Namespace,
				resourceType: gvr.Service,
			}
		}
		return nil
	}, opts.WithName("servicesUsingAutoWaypoint")...)

	serviceEntriesUsingAutoWaypoint := krt.NewCollection(se, func(ctx krt.HandlerContext, se *networkingv1.ServiceEntry) *serviceX {
		seLabels := labels.Set(se.GetLabels())
		if requestsAutoWaypoint(seLabels) {
			return &serviceX{
				name:         se.Name,
				namespace:    se.Namespace,
				resourceType: gvr.ServiceEntry,
			}
		}
		return nil
	}, opts.WithName("serviceEntriesUsingAutoWaypoint")...)

	serviceXesUsingAutoWaypoint := krt.JoinCollection([]krt.Collection[serviceX]{
		servicesUsingAutoWaypoint,
		serviceEntriesUsingAutoWaypoint,
	},
		opts.WithName("serviceXesUsingAutoWaypoint")...)

	serviceXesUsingAutoWaypointByNamespace := krt.NewIndex(serviceXesUsingAutoWaypoint, func(s serviceX) []string {
		return []string{s.namespace}
	})

	return serviceXesUsingAutoWaypoint, serviceXesUsingAutoWaypointByNamespace
}
