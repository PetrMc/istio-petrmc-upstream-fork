// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package autowaypoint

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/test/util/assert"
)

func TestServiceXesUsingWaypoint(t *testing.T) {
	client := kube.NewFakeClient()
	dbg := new(krt.DebugHandler)
	ctx, cancel := context.WithCancel(context.Background())
	fullStop := make(chan struct{})
	opts := krt.NewOptionsBuilder(fullStop, "test", dbg)
	t.Cleanup(func() {
		if t.Failed() {
			b, err := dbg.MarshalJSON()
			if err != nil {
				t.Logf("failed to marshal krt debug info: %v", err)
			}
			t.Logf("krt debug info: %s", string(b))
		}
		cancel()
		close(fullStop)
	})

	services := krt.NewInformerFiltered[*corev1.Service](client, kclient.Filter{
		ObjectFilter: client.ObjectFilter(),
	}, opts.WithName("informer/services")...)

	serviceEntries := krt.NewInformerFiltered[*networkingv1.ServiceEntry](client, kclient.Filter{
		ObjectFilter: client.ObjectFilter(),
	}, opts.WithName("informer/serviceentries")...)

	serviceXesUsingAutoWaypoint, serviceXesUsingAutoWaypointByNamespace := serviceXesUsingAutoWaypoint(services, serviceEntries, opts)

	serviceXesTracker := assert.NewTracker[string](t)

	serviceXesUsingAutoWaypoint.Register(func(o krt.Event[serviceX]) {
		s := o.New
		if o.Event == controllers.EventDelete {
			s = o.Old
		}
		serviceXesTracker.Record(fmt.Sprintf("%s/%s/%s/%s", o.Event, s.resourceType.Resource, s.namespace, s.name))
	})

	client.RunAndWait(ctx.Done())

	// completed setting up a test environment, from here test logic proceeds

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
	}

	client.Kube().CoreV1().Services("default").Create(ctx, service, metav1.CreateOptions{})

	// this service isn't asking for a waypoint yet
	serviceXesTracker.Empty()

	// add a label to the service to ask for a waypoint
	service.Labels = map[string]string{"istio.io/use-waypoint": "auto"}
	client.Kube().CoreV1().Services("default").Update(ctx, service, metav1.UpdateOptions{})

	// this service is now asking for a waypoint
	serviceXesTracker.WaitOrdered("add/services/default/test-service")

	assert.Equal(t, len(serviceXesUsingAutoWaypointByNamespace.Lookup("default")), 1)

	// add a cross-namespace label to the service
	service.Labels["istio.io/use-waypoint-namespace"] = "boop"
	client.Kube().CoreV1().Services("default").Update(ctx, service, metav1.UpdateOptions{})

	// this service can't get an auto waypoint in a different namespace
	serviceXesTracker.WaitOrdered("delete/services/default/test-service")
	assert.Equal(t, len(serviceXesUsingAutoWaypointByNamespace.Lookup("default")), 0)

	// remove cross-namespace label
	delete(service.Labels, "istio.io/use-waypoint-namespace")
	client.Kube().CoreV1().Services("default").Update(ctx, service, metav1.UpdateOptions{})

	// this service can get an auto waypoint in the default namespace again
	serviceXesTracker.WaitOrdered("add/services/default/test-service")
	assert.Equal(t, len(serviceXesUsingAutoWaypointByNamespace.Lookup("default")), 1)

	// add a service entry to the default namespace
	serviceEntry := &networkingv1.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-entry",
			Namespace: "default",
			Labels: map[string]string{
				"istio.io/use-waypoint": "auto",
			},
		},
	}
	client.Istio().NetworkingV1().ServiceEntries("default").Create(ctx, serviceEntry, metav1.CreateOptions{})

	// this service can get an auto waypoint in the default namespace
	serviceXesTracker.WaitOrdered("add/serviceentries/default/test-service-entry")
	assert.Equal(t, len(serviceXesUsingAutoWaypointByNamespace.Lookup("default")), 2)

	// remove the auto waypoint label from the service
	delete(service.Labels, "istio.io/use-waypoint")
	client.Kube().CoreV1().Services("default").Update(ctx, service, metav1.UpdateOptions{})

	serviceXesTracker.WaitOrdered("delete/services/default/test-service")
	assert.Equal(t, len(serviceXesUsingAutoWaypointByNamespace.Lookup("default")), 1)

	// delete the service entry
	client.Istio().NetworkingV1().ServiceEntries("default").Delete(ctx, serviceEntry.Name, metav1.DeleteOptions{})

	serviceXesTracker.WaitOrdered("delete/serviceentries/default/test-service-entry")
	assert.Equal(t, len(serviceXesUsingAutoWaypointByNamespace.Lookup("default")), 0)

	// delete the service
	client.Kube().CoreV1().Services("default").Delete(ctx, service.Name, metav1.DeleteOptions{})

	// this service isn't asking for a waypoint so we don't see the delete event
	serviceXesTracker.Empty()
	assert.Equal(t, len(serviceXesUsingAutoWaypointByNamespace.Lookup("default")), 0)
}
