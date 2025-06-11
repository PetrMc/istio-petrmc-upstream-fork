// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering

import (
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	k8s "sigs.k8s.io/gateway-api/apis/v1"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/api/annotation"
	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/model/kstatus"
	"istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
)

func (c *NetworkWatcher) Reconcile(raw any) error {
	key := raw.(typedNamespace)
	log.WithLabels("kind", key.kind, "resource", key.NamespacedName).Infof("reconciling")
	switch key.kind {
	case kind.Gateway:
		// Gateway: triggered when we have a change to a Gateway object, which may mean we need to add/remove/update our
		// remote network watches or add/remove/update the generated istio-remote gateway necessary for GME automated peering
		return c.reconcileGateway(key.NamespacedName)
	case kind.WorkloadEntry:
		// WorkloadEntry: triggered when we may need to change a generated WE
		// The key is: Name=ns/name of target service, Namespace=cluster
		return c.reconcileWorkloadEntry(key.NamespacedName)
	case kind.ServiceEntry:
		// WorkloadEntry: triggered when we may need to change a generated SE
		// The key is: Name=name of target service, Namespace=namespace of target service
		return c.reconcileServiceEntry(key.NamespacedName)
	case kind.MeshNetworks:
		// MeshNetworks: triggered when an entire network has changed.
		// Intended to cleanup stale resources
		return c.reconcileNetworkSync(key.Name)
	default:
		log.Errorf("unknown resource kind: %v", key.kind)
	}
	return nil
}

func (c *NetworkWatcher) reconcileGateway(name types.NamespacedName) error {
	log := log.WithLabels("gateway", name)
	gw := c.gateways.Get(name.Name, name.Namespace)
	if EnableAutomaticGatewayCreation {
		c.reconcileEastWestGateway(gw)
	}
	peer, err := TranslatePeerGateway(gw)
	if err != nil {
		oldNet, f := c.gatewaysToNetwork[name]
		if !f {
			c.enqueueStatusUpdate(name)
			log.Debugf("gateway is not a peer gateway (%v) and we were not tracking it", err)
			return nil
		}
		log.Infof("gateway for network %q is no longer a peer gateway (%v), shutting down network controller", oldNet.networkName, err)
		oldNet.ShutdownNow()
		delete(c.gatewaysToNetwork, name)
		c.enqueueStatusUpdate(name)
		return nil
	}
	oldNet, f := c.gatewaysToNetwork[name]
	if f {
		if oldNet.networkName != peer.Network {
			log.Infof("network changed from %v to %v", oldNet.networkName, peer.Network)
			oldNet.ShutdownNow()
			delete(c.gatewaysToNetwork, name)
			c.enqueueStatusUpdate(name)
		} else {
			// TODO: respect address change
			log.Infof("gateway changed but not in a meaningful way")
			return nil
		}
	}
	clusterNetwork := c.clusterID // we already assume the network is the clusterID in the buildConfig
	if peer.Network == clusterNetwork {
		log.Infof("peer gateway to %v is for local cluster network, ignoring", peer.Address)
		return nil
	}
	net := newNetwork(
		peer.Network,
		peer.Address,
		c.buildConfig(fmt.Sprintf("peering-%s", peer.Network)),
		c.debugger,
		c.queue,
		func(o krt.Event[RemoteFederatedService]) {
			obj := o.Latest()
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{
					Namespace: obj.Cluster,
					Name:      config.NamespacedName(obj).String(),
				},
				kind: kind.WorkloadEntry,
			})
		},
		func() {
			c.enqueueStatusUpdate(name)
		},
	)
	c.gatewaysToNetwork[name] = &NamedNetwork{
		networkName: peer.Network,
		network:     net,
	}
	// we started a network, so enqueue the resource for a status update
	c.enqueueStatusUpdate(name)
	return nil
}

// reconcileNetworkSync is called when a network watch has synced. This is used to cleanup stale resources
func (c *NetworkWatcher) reconcileNetworkSync(network string) error {
	// Get all current workload entries. This appears "racy" to look at one time and act on it, but
	// since we are cleaning up stale resources this is not a problem.
	currentWorkloadEntries := c.workloadEntries.List(PeeringNamespace, klabels.SelectorFromSet(map[string]string{
		SourceClusterLabel: network,
	}))
	for _, w := range currentWorkloadEntries {
		// Enqueue it; if it no longer exists it will be removed
		c.queue.Add(typedNamespace{
			NamespacedName: types.NamespacedName{
				Namespace: network,
				Name:      w.Labels[ParentServiceNamespaceLabel] + "/" + w.Labels[ParentServiceLabel],
			},
			kind: kind.WorkloadEntry,
		})
	}
	return nil
}

func (c *NetworkWatcher) reconcileWorkloadEntry(tn types.NamespacedName) error {
	log := log.WithLabels("workloadentry", tn)

	cluster := tn.Namespace
	ns, name, _ := strings.Cut(tn.Name, "/")

	weName := fmt.Sprintf("autogen.%v.%v.%v", cluster, ns, name)
	labels := map[string]string{}

	var fs *RemoteFederatedService
	for _, net := range c.gatewaysToNetwork {
		if net.networkName == cluster {
			if !net.network.HasSynced() {
				log.Debugf("skipping, network not yet synced")
				return nil
			}
			fs = net.network.collection.GetKey(tn.String())
		}
	}
	if fs == nil {
		// No federated service exists, remove
		log.Infof("workload entry will be deleted (key %v not found)", tn.String())
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}

	localService := c.services.Get(name, ns)
	weScope := ServiceScopeGlobal
	if localService != nil && !HasGlobalLabel(localService.Labels) {
		// It's not global, so ignore it
		localService = nil
	}
	if localService != nil {
		weScope = localService.Labels[ServiceScopeLabel]
		// If the local service exists, the SE is going to set the label selectors to the service's so we can select the Pod
		// We need to include those
		labels = maps.Clone(localService.Spec.Selector)
		if labels == nil {
			labels = map[string]string{}
		}
	}
	// Add our identification labels always. If there is no labels this will be used as the selector in the SE
	labels[ParentServiceLabel] = name
	labels[ServiceScopeLabel] = weScope
	labels[ParentServiceNamespaceLabel] = ns
	labels[SourceClusterLabel] = cluster
	// Indicate we support tunneling. This is for Envoy dataplanes mostly.
	labels[model.TunnelLabel] = model.TunnelHTTP
	if fs.Waypoint != nil {
		labels[RemoteWaypointLabel] = "true"
	}

	ports := map[string]uint32{}
	for _, p := range fs.Ports {
		ports[fmt.Sprintf("port-%d", p.ServicePort)] = p.ServicePort
	}
	annos := map[string]string{
		ServiceSubjectAltNamesAnnotation: strings.Join(slices.Sort(fs.SubjectAltNames), ","),
		// Signal we should use HBONE
		annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
	}
	weight := uint32(0)
	if fs.Capacity != nil {
		weight = fs.Capacity.GetValue()
		if weight == 0 {
			// WorkloadEntry cannot distinguish 0 and unset.
			annos[ServiceEndpointStatus] = ServiceEndpointStatusUnhealthy
		}

	}
	we := &clientnetworking.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:        weName,
			Namespace:   PeeringNamespace,
			Labels:      labels,
			Annotations: annos,
		},
		Spec: networking.WorkloadEntry{
			Address: "",
			Network: cluster,
			Ports:   ports,
			Weight:  weight,
		},
	}

	changed, err := CreateOrUpdateIfNeeded(c.workloadEntries, we, func(desired *clientnetworking.WorkloadEntry, live *clientnetworking.WorkloadEntry) bool {
		if desired.Name != live.Name {
			return false
		}
		if desired.Namespace != live.Namespace {
			return false
		}
		if !maps.Equal(desired.Labels, live.Labels) {
			return false
		}
		if !maps.Equal(desired.Annotations, live.Annotations) {
			return false
		}
		return proto.Equal(&desired.Spec, &live.Spec)
	})
	if err != nil {
		return err
	}
	log.WithLabels("updated", changed).Infof("workload entry %q processed", we.Name)
	return nil
}

func (c *NetworkWatcher) reconcileServiceEntry(name types.NamespacedName) error {
	log := log.WithLabels("federatedservice", name)
	hostname := fmt.Sprintf("%s.%s%s", name.Name, name.Namespace, DomainSuffix)
	seName := fmt.Sprintf("autogen.%v.%v", name.Namespace, name.Name)

	se := &clientnetworking.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      seName,
			Namespace: PeeringNamespace,
		},
		Spec: networking.ServiceEntry{
			Hosts:      []string{hostname},
			Addresses:  nil, // Will be auto assigned
			Resolution: networking.ServiceEntry_STATIC,
		},
	}

	localService := c.services.Get(name.Name, name.Namespace)
	if localService != nil && !HasGlobalLabel(localService.Labels) {
		// It's not global, so ignore it
		localService = nil
	}
	remoteServices := c.workloadEntryServiceIndex.Lookup(name)

	if localService == nil && len(remoteServices) == 0 {
		log.Infof("service entry will be deleted (no local or remote services)")
		return controllers.IgnoreNotFound(c.serviceEntries.Delete(se.Name, PeeringNamespace))
	}

	labels := make(map[string]string)
	if localService != nil && localService.Labels != nil {
		labels = maps.Clone(localService.Labels)
	}
	labels[ParentServiceLabel] = name.Name
	labels[ParentServiceNamespaceLabel] = name.Namespace
	se.Labels = labels

	annos := make(map[string]string)
	if localService != nil && localService.Annotations != nil {
		annos = maps.Clone(localService.Annotations)
		// A user may have traffic distribution configured on the service. We want to copy that to the SE by default.
		if _, f := localService.Annotations[annotation.NetworkingTrafficDistribution.Name]; !f {
			if localService.Spec.TrafficDistribution != nil {
				annos[annotation.NetworkingTrafficDistribution.Name] = *localService.Spec.TrafficDistribution
			} else {
				// If none set, prefer network by default
				annos[annotation.NetworkingTrafficDistribution.Name] = DefaultTrafficDistribution
			}
		}
	} else {
		// If there is no local service, prefer network by default
		annos[annotation.NetworkingTrafficDistribution.Name] = DefaultTrafficDistribution
	}
	se.Annotations = annos

	if localService != nil {
		se.Spec.Ports = convertPorts(localService.Spec.Ports)
		se.Spec.WorkloadSelector = &networking.WorkloadSelector{Labels: localService.Spec.Selector}
	} else {
		for _, remote := range remoteServices {
			se.Spec.Ports = mergeRemotePorts(se.Spec.Ports, remote.Spec.Ports)
		}
		se.Spec.WorkloadSelector = &networking.WorkloadSelector{Labels: map[string]string{
			ParentServiceLabel:          name.Name,
			ParentServiceNamespaceLabel: name.Namespace,
		}}
		// Semantics: sidecar will skip processing if there is a local waypoint (determined by standard waypoint codepath)
		// OR if any cluster has a remote waypoint (determined by remote waypoint label).
		// This has a few awkward edge cases...
		// * If there are 2 remote clusters, and only 1 has a remote waypoint, then the sidecar will skip processing regardless.
		// * If there is a local service but no local waypoint, the sidecar will do processing
		remoteWaypoint := false
		for _, remote := range remoteServices {
			if rw, f := remote.Labels[RemoteWaypointLabel]; f && rw == "true" {
				remoteWaypoint = true
				break
			}
		}
		if remoteWaypoint {
			labels[RemoteWaypointLabel] = "true"
		}
	}

	sans := sets.New[string]()
	for _, remote := range remoteServices {
		if san := remote.Annotations[ServiceSubjectAltNamesAnnotation]; san != "" {
			sans.InsertAll(strings.Split(san, ",")...)
		}
	}
	se.Spec.SubjectAltNames = sets.SortedList(sans)

	changed, err := CreateOrUpdateIfNeeded(c.serviceEntries, se, func(desired *clientnetworking.ServiceEntry, live *clientnetworking.ServiceEntry) bool {
		if desired.Name != live.Name {
			return false
		}
		if desired.Namespace != live.Namespace {
			return false
		}
		if !maps.Equal(desired.Labels, live.Labels) {
			return false
		}
		if !maps.Equal(desired.Annotations, live.Annotations) {
			return false
		}
		return proto.Equal(&desired.Spec, &live.Spec)
	})
	if err != nil {
		return err
	}
	log.WithLabels("updated", changed).Infof("service entry processed")
	return nil
}

// reconcileEastWestGateway creates/updates/deletes the generated istio-remote peer gateway for the given istio-eastwest gateway
func (c *NetworkWatcher) reconcileEastWestGateway(gw *gateway.Gateway) {
	if gw == nil {
		log.Debug("gateway was deleted")
		return
	}
	if gw.Spec.GatewayClassName != constants.EastWestGatewayClassName {
		log.Debugf("gateway is not an istio-eastwest gateway (was %q)", gw.Spec.GatewayClassName)
		return
	}
	meshConfig := c.meshConfigWatcher.Mesh()
	if meshConfig == nil {
		log.Error("mesh config not found")
		return
	}
	td := meshConfig.GetTrustDomain()
	generatedPeer, err := TranslateEastWestGateway(gw, td)
	if err != nil {
		log.Warnf("failed to generate peer gateway: %v", err)
		return
	}
	changed, err := CreateOrUpdateIfNeeded(c.gateways, generatedPeer, func(desired *gateway.Gateway, live *gateway.Gateway) bool {
		if desired.Name != live.Name {
			return false
		}
		if desired.Namespace != live.Namespace {
			return false
		}
		if !maps.Equal(desired.Labels, live.Labels) {
			return false
		}
		if !maps.Equal(desired.Annotations, live.Annotations) {
			return false
		}
		return reflect.DeepEqual(desired, live)
	})
	if err != nil {
		log.Errorf("failed to create or update peer gateway: %v", err)
		return
	}
	log.WithLabels("updated", changed).Infof("generated peer gateway %q processed", generatedPeer.Name)
}

func (c *NetworkWatcher) ReconcileStatus(raw any) error {
	key := raw.(typedNamespace)
	log.WithLabels("kind", key.kind, "resource", key.NamespacedName).Infof("reconciling status")
	switch key.kind {
	case kind.Gateway:
		return c.reconcileGatewayStatus(key.NamespacedName)
	default:
		log.Errorf("unknown resource kind: %v", key.kind)
	}
	return nil
}

func (c *NetworkWatcher) reconcileGatewayStatus(name types.NamespacedName) error {
	log := log.WithLabels("gateway status", name)
	gw := c.gateways.Get(name.Name, name.Namespace)
	if gw == nil {
		log.Debug("gateway was deleted no status to reconcile")
		return nil
	}

	// maybe we should reconcile to a default state, but it's hard to work out when that is ok and
	// nothing else in the codebase handles status updates so we will ignore status updates for now
	if gw.Spec.GatewayClassName != constants.RemoteGatewayClassName {
		return nil
	}

	var gatewayConditions []metav1.Condition
	err := ValidatePeerGateway(*gw)
	if err != nil {
		gatewayConditions = append(gatewayConditions, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeeringSucceeded,
			Status:             metav1.ConditionFalse,
			Reason:             string(k8s.GatewayReasonInvalid),
			Message:            fmt.Sprintf("no network started encountered error validating gateway: %v", err.Error()),
			LastTransitionTime: metav1.Now(),
		})
		c.setGatewayStatus(gw, gatewayConditions)
		return nil
	}

	// if the gateway is syntactically valid and has all the required fields for processing
	// check for the following conditions
	// If the network for the gateway exists
	//    a. If the network exists but is not synced, write Programmed: Pending status and Accepted: Accepted status
	//    b. If the network exists and is synced, write Programmed: Programmed and Accepted: Accepted status
	namedNet, f := c.gatewaysToNetwork[name]
	if !f {
		log.Debugf("unable to set status for gateway %q, no network found", name)
		return nil
	}

	if !namedNet.network.HasSynced() {
		gatewayConditions = append(gatewayConditions, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeeringSucceeded,
			Status:             metav1.ConditionUnknown,
			Reason:             string(k8s.GatewayReasonPending),
			Message:            "network not yet synced check istiod log for more details",
			LastTransitionTime: metav1.Now(),
		})
		c.setGatewayStatus(gw, gatewayConditions)
		return nil
	}

	gatewayConditions = append(gatewayConditions, metav1.Condition{
		ObservedGeneration: gw.Generation,
		Type:               constants.SoloConditionPeeringSucceeded,
		Status:             metav1.ConditionTrue,
		Reason:             string(k8s.GatewayReasonProgrammed),
		LastTransitionTime: metav1.Now(),
	})
	c.setGatewayStatus(gw, gatewayConditions)
	return nil
}

func (c *NetworkWatcher) setGatewayStatus(gw *gateway.Gateway, conditions []metav1.Condition) {
	// transform gateway into config.Config type so we can use the status mutator
	meta := config.FromObjectMeta(gw.ObjectMeta)
	meta.GroupVersionKind = gvk.KubernetesGateway
	obj := &config.Config{
		Meta:   meta,
		Spec:   &gw.Spec,
		Status: config.DeepCopy(&gw.Status),
	}

	oldStatus := obj.Status.(*k8s.GatewayStatus)
	newStatus := config.DeepCopy(&gw.Status).(*k8s.GatewayStatus)
	existingConditions := oldStatus.Conditions
	for _, condition := range conditions {
		existingConditions = kstatus.UpdateConditionIfChanged(existingConditions, condition)
	}
	newStatus.Conditions = existingConditions

	res := status.ResourceFromModelConfig(*obj)
	c.statusController.EnqueueStatusUpdateResource(newStatus, res)
}

func (c *NetworkWatcher) enqueueStatusUpdate(name types.NamespacedName) {
	c.statusQueue.Add(typedNamespace{
		NamespacedName: name,
		kind:           kind.Gateway,
	})
}
