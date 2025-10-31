// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/api/annotation"
	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/ptr"
)

func (a *ecsAccountDiscovery) reconcileWorkloadEntry(nn types.NamespacedName) error {
	log := log.WithLabels("workload", nn.Name)

	// parse internal name and create WorkloadEntry name
	resourceType, ns, arn, err := ParseResourceName(nn.Name)
	if err != nil {
		return err
	}
	_, cluster, identifier, err := ParseARNResource(arn.Resource)
	if err != nil {
		return err
	}
	workloadEntryName := fmt.Sprintf("ecs-%s-%s-%s-%s-%s", resourceType, arn.AccountID, arn.Region, cluster, identifier)

	// find task in snapshot, delete WE if not found
	snap, found := a.Snapshot(nn.Namespace)[nn.Name]
	if !found {
		log.Infof("task no longer present, deleting WorkloadEntry")
		return controllers.IgnoreNotFound(a.workloadEntries.Delete(workloadEntryName, ns))
	}

	// found snapshot, need to update
	log.Infof("workload updating")

	// set labels
	labels := maps.Clone(snap.Tags.Passthrough)
	if labels == nil {
		labels = make(map[string]string)
	}
	sa := ptr.NonEmptyOrDefault(snap.Tags.ServiceAccount, "default")
	labels[HostnameLabel] = snap.Hostname()
	labels[ServiceLabel] = snap.ServiceName()
	// Default a sane workload name, to avoid the UID becoming the name (which ends up in metrics)
	if _, f := labels[WorkloadNameLabel]; !f {
		labels[WorkloadNameLabel] = snap.ServiceName()
	}

	network := a.lookupNetwork(snap.Task.Address.String(), labels)

	wle := &clientnetworking.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadEntryName,
			Namespace: snap.Namespace(),
			Labels:    labels,
			Annotations: map[string]string{
				ARNAnnotation:          snap.ARN,
				ResourceAnnotation:     nn.Name,
				DiscoveredByAnnotation: a.account.role,
				ClusterAnnotation:      snap.Cluster,
			},
		},
		Spec: networking.WorkloadEntry{
			Address:        snap.Task.Address.String(),
			ServiceAccount: sa,
			Locality:       translateLocality(snap.Task.Zone),
			Network:        network.String(),
		},
	}
	if snap.Task.Mesh {
		wle.Annotations[annotation.AmbientRedirection.Name] = constants.AmbientRedirectionEnabled
	}

	_, cerr := createOrUpdate(a.workloadEntries, wle)
	return cerr
}

func (a *ecsAccountDiscovery) reconcileServiceEntry(nn types.NamespacedName) error {
	log := log.WithLabels("service", nn)

	// parse internal name and create ServiceEntry name
	resourceType, ns, arn, err := ParseResourceName(nn.Name)
	if err != nil {
		return err
	}
	_, cluster, identifier, err := ParseARNResource(arn.Resource)
	if err != nil {
		return err
	}
	serviceEntryName := fmt.Sprintf("ecs-%s-%s-%s-%s-%s", resourceType, arn.AccountID, arn.Region, cluster, identifier)

	// find service in snapshot, delete SE if not found
	snap, found := a.Snapshot(nn.Namespace)[nn.Name]
	if !found {
		log.Infof("service no longer present, deleting ServiceEntry")
		return controllers.IgnoreNotFound(a.serviceEntries.Delete(serviceEntryName, ns))
	}

	// found snapshot, need to update
	log.Infof("service updating")

	// set labels
	labels := maps.Clone(snap.Tags.Passthrough)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[ServiceLabel] = snap.Service.Name

	// set ports
	ports := []*networking.ServicePort{}
	for i, port := range snap.Tags.Ports {
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

	// build SE
	se := &clientnetworking.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceEntryName,
			Namespace: snap.Namespace(),
			Labels:    labels,
			Annotations: map[string]string{
				ARNAnnotation:          snap.ARN,
				ResourceAnnotation:     nn.Name,
				DiscoveredByAnnotation: a.account.role,
				ClusterAnnotation:      snap.Cluster,
			},
		},
		Spec: networking.ServiceEntry{
			Hosts: []string{
				snap.Hostname(),
			},
			WorkloadSelector: &networking.WorkloadSelector{
				Labels: map[string]string{
					HostnameLabel: snap.Hostname(),
				},
			},
			Ports:      ports,
			Resolution: networking.ServiceEntry_STATIC,
		},
	}

	_, cerr := createOrUpdate(a.serviceEntries, se)
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
