// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/annotation"
	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/ptr"
)

func (d *ECSDiscovery) reconcileWorkloadEntry(name string) error {
	_, clusterName, ns, resourceName, err := ParseResourceName(name)
	if err != nil {
		return err
	}
	log := log.WithLabels("workload", name)

	cluster, ok := d.clusters[clusterName]
	if !ok {
		return fmt.Errorf("cluster %s not found for workload %s", clusterName, name)
	}

	segments := strings.Split(resourceName, "/")
	uid := segments[len(segments)-1]
	groupName := segments[0]
	arn := strings.Join(segments[1:], "/")
	workloadEntryName := fmt.Sprintf("ecs-%s-%s", groupName, uid)

	snap := cluster.Snapshot()

	ecswl, f := snap[name]
	if !f {
		log.Infof("workload deleted")
		return controllers.IgnoreNotFound(cluster.workloadEntries.Delete(workloadEntryName, ns))
	}
	wl := *ecswl.Workload

	var svc *EcsService
	if wl.HasService() {
		ecsvc, f := snap[fmt.Sprintf("service/%s/%s/%s", clusterName, ns, groupName)]
		if !f && wl.HasService() {
			log.Infof("service %q for workload not found", groupName)
			return controllers.IgnoreNotFound(cluster.workloadEntries.Delete(workloadEntryName, ns))
		}
		svc = ecsvc.Service
	}

	log.Infof("workload updating")

	labels := maps.Clone(wl.Tags.Passthrough)
	if labels == nil {
		labels = make(map[string]string)
	}
	// Fetch the SA from the service.
	// TODO: should we do wl.Tags and fallback to svc.Tags?
	var sa string
	if svc != nil {
		sa = svc.Tags.ServiceAccount
		labels[ServiceLabel] = groupName
	} else {
		// We use this as a lookup label to identify it as an ECS thing, so set it even when empty
		labels[ServiceLabel] = ""
	}
	// Default a sane workload name, to avoid the UID becoming the name (which ends up in metrics)
	if _, f := labels["service.istio.io/workload-name"]; !f {
		labels["service.istio.io/workload-name"] = groupName
	}

	network := d.lookupNetwork(wl.Address.String(), labels)

	wle := &clientnetworking.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadEntryName,
			Namespace: ns,
			Labels:    labels,
			Annotations: map[string]string{
				ResourceAnnotation: name,
				ARNAnnotation:      arn,
			},
		},
		Spec: networking.WorkloadEntry{
			Address:        wl.Address.String(),
			ServiceAccount: sa,
			Locality:       translateLocality(wl.Zone),
			Network:        network.String(),
		},
	}
	if wl.Mesh {
		wle.Annotations[annotation.AmbientRedirection.Name] = constants.AmbientRedirectionEnabled
	}

	_, cerr := createOrUpdate(cluster.workloadEntries, wle)
	return cerr
}

func (d *ECSDiscovery) reconcileServiceEntry(name string) error {
	log := log.WithLabels("service", name)
	_, clusterName, ns, serviceName, err := ParseResourceName(name)
	if err != nil {
		return err
	}

	cluster, ok := d.clusters[clusterName]
	if !ok {
		return fmt.Errorf("cluster %s not found", clusterName)
	}

	snap := cluster.Snapshot()
	serviceEntryName := fmt.Sprintf("ecs-%s", serviceName)
	ecso, f := snap[name]
	if !f {
		log.Infof("service no longer present, deleting")
		// TODO: namespace may differ!
		return controllers.IgnoreNotFound(cluster.serviceEntries.Delete(serviceEntryName, ns))
	}
	log.Infof("service updated")
	service := ecso.Service
	labels := maps.Clone(service.Tags.Passthrough)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[ServiceLabel] = service.Name

	ports := []*networking.ServicePort{}
	for i, port := range service.Tags.Ports {
		ports = append(ports, &networking.ServicePort{
			Number:     uint32(port.ServicePort),
			Protocol:   port.Protocol,
			Name:       "port-" + fmt.Sprint(i),
			TargetPort: uint32(port.TargetPort),
		})
	}
	if len(ports) == 0 {
		ports = defaultPorts
	}

	se := &clientnetworking.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceEntryName,
			Namespace: ns,
			Labels:    labels,
			Annotations: map[string]string{
				ResourceAnnotation: name,
			},
		},
		Spec: networking.ServiceEntry{
			Hosts: []string{
				ptr.NonEmptyOrDefault(service.Tags.Hostname, fmt.Sprintf("%s.%s.local", service.Name, clusterName)),
			},
			WorkloadSelector: &networking.WorkloadSelector{
				Labels: map[string]string{
					ServiceLabel: service.Name,
				},
			},
			Ports:      ports,
			Resolution: networking.ServiceEntry_STATIC,
		},
	}

	_, cerr := createOrUpdate(cluster.serviceEntries, se)
	return cerr
}

var defaultPorts = []*networking.ServicePort{
	{
		Number:   80,
		Protocol: "HTTP",
		Name:     "http",
	},
	{
		Number:   8080,
		Protocol: "HTTP",
		Name:     "http-alt",
	},
}

func createOrUpdate[T controllers.ComparableObject](c kclient.ReadWriter[T], object T) (T, error) {
	res := c.Get(object.GetName(), object.GetNamespace())
	if controllers.IsNil(res) {
		return c.Create(object)
	}
	object.SetResourceVersion(res.GetResourceVersion())
	// Already exist, update
	return c.Update(object)
}

func translateLocality(az string) string {
	// az is a formt like 'us-west-2a'. We need to translate to 'us-west-2/us-west-2a'
	if strings.Count(az, "-") != 2 {
		// Unknown format?
		return ""
	}
	region := az[:len(az)-1]
	return region + "/" + az
}
