// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "sigs.k8s.io/gateway-api/apis/v1"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/api/annotation"
	"istio.io/api/label"
	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/model/kstatus"
	"istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/maps"
	networkid "istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/workloadapi"
)

type Kind int

const (
	Cluster Kind = iota
	Gateway
	WorkloadEntry
	FlatWorkloadEntry
	ServiceEntry
)

func (k Kind) String() string {
	switch k {
	case Cluster:
		return "Cluster"
	case Gateway:
		return "Gateway"
	case WorkloadEntry:
		return "WorkloadEntry"
	case FlatWorkloadEntry:
		return "FlatWorkloadEntry"
	case ServiceEntry:
		return "ServiceEntry"
	}
	return "Unknown"
}

func AutogenHostname(name, ns string) string {
	return fmt.Sprintf("%s.%s%s", name, ns, DomainSuffix)
}

func ParseAutogenHostname(hostname string) (string, string, bool) {
	nns, suffixOk := strings.CutSuffix(hostname, DomainSuffix)
	name, ns, nnsOk := strings.Cut(nns, ".")
	return name, ns, suffixOk && nnsOk
}

func (c *NetworkWatcher) Reconcile(raw any) error {
	key := raw.(typedNamespace)
	log.WithLabels("kind", key.kind, "resource", key.NamespacedName).Infof("reconciling")
	switch key.kind {
	case Gateway:
		// Gateway: triggered when we have a change to a Gateway object, which may mean we need to add/remove/update our
		// remote network watches or add/remove/update the generated istio-remote gateway necessary for GME automated peering
		return c.reconcileGateway(key.NamespacedName)
	case WorkloadEntry:
		// WorkloadEntry: triggered when we may need to change a generated WE
		// The key is: Name=ns/name of target service, Namespace=cluster
		return c.reconcileGatewayWorkloadEntry(key.NamespacedName)
	case FlatWorkloadEntry:
		// FlatWorkloadEntry: triggered when we may need to change a generated WE
		// The key is: Name=ns/name of target workload, Namespace=cluster
		return c.reconcileFlatWorkloadEntry(key.NamespacedName)
	case ServiceEntry:
		// WorkloadEntry: triggered when we may need to change a generated SE
		// The key is: Name=name of target service, Namespace=namespace of target service
		return c.reconcileServiceEntry(key.NamespacedName)
	case Cluster:
		// Cluster: triggered when an entire cluster has changed.
		// Intended to cleanup stale resources
		return c.reconcileClusterSync(key.Name)
	default:
		log.Errorf("unknown resource kind: %v", key.kind)
	}
	return nil
}

func (c *NetworkWatcher) reconcileGateway(name types.NamespacedName) error {
	c.clustersMu.Lock()
	defer c.clustersMu.Unlock()
	log := log.WithLabels("gateway", name)
	gw := c.gateways.Get(name.Name, name.Namespace)
	if EnableAutomaticGatewayCreation {
		c.reconcileEastWestGateway(gw)
	}
	peer, err := TranslatePeerGateway(gw)
	if err != nil {
		oldCluster, f := c.gatewaysToCluster[name]
		delete(c.gatewaysToCluster, name)
		if !f {
			c.enqueueStatusUpdate(name)
			log.Debugf("gateway is not a peer gateway (%v) and we were not tracking it", err)
			return nil
		}
		log.Infof("gateway for cluster %q is no longer a peer gateway (%v), shutting down cluster controller", oldCluster, err)
		peerCluster := c.remoteClusters[oldCluster]
		peerCluster.shutdownNow()
		delete(c.remoteClusters, oldCluster)
		c.enqueueStatusUpdate(name)
		return nil
	}

	oldClusterID, f := c.gatewaysToCluster[name]
	if f {
		cluster := c.remoteClusters[oldClusterID]
		oldNetworkID := cluster.networkName
		if oldClusterID != peer.Cluster || oldNetworkID != peer.Network {
			// total cluster update, id or network changed
			log.Infof(
				"cluster changed from %v/%v to %v/%v",
				oldClusterID, oldNetworkID,
				peer.Cluster, peer.Network,
			)
			cluster.shutdownNow()
			delete(c.remoteClusters, oldClusterID)
			c.enqueueStatusUpdate(name)
		} else {
			// partial cluster update
			cluster.mu.Lock()
			defer cluster.mu.Unlock()

			changed := false
			if cluster.locality != peer.Locality {
				log.Infof(
					"gateway for cluster %s locality changed from %v to %v",
					peer.Cluster,
					cluster.locality, peer.Locality,
				)
				cluster.locality = peer.Locality
				changed = true
			}

			// TODO handle other changes to the cluster that affect the
			// services/workloads from that cluster or use krt to automatically
			// detect dependencies and changes
			if changed {
				// update all the generated resources for this cluster
				c.queue.Add(typedNamespace{
					NamespacedName: types.NamespacedName{
						Name: cluster.clusterID.String(),
					},
					kind: Cluster,
				})
			} else {
				log.Infof("gateway changed but not in a meaningful way")
			}

			return nil
		}
	}
	if peer.Cluster == c.localCluster {
		log.Infof("peer gateway to %v is for local cluster network, ignoring", peer.Address)
		return nil
	}
	peerCluster := newPeerCluster(
		c.localNetwork,
		peer,
		c.buildConfig(fmt.Sprintf("peering-%s", peer.Cluster)),
		c.debugger,
		c.queue,
		func(o krt.Event[RemoteFederatedService]) {
			obj := o.Latest()
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{
					Namespace: obj.Cluster,
					Name:      config.NamespacedName(obj.Service).String(),
				},
				kind: WorkloadEntry,
			})
		},
		func(o krt.Event[RemoteWorkload]) {
			obj := o.Latest()
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{
					Namespace: obj.Cluster,
					Name:      obj.ResourceName(),
				},
				kind: FlatWorkloadEntry,
			})
		},
		func() {
			c.enqueueStatusUpdate(name)
		},
	)
	c.remoteClusters[peer.Cluster] = peerCluster
	c.gatewaysToCluster[name] = peer.Cluster
	// we started a network, so enqueue the resource for a status update
	c.enqueueStatusUpdate(name)
	return nil
}

// reconcileClusterSync is called when a cluster watch has synced. This is used to cleanup stale resources
func (c *NetworkWatcher) reconcileClusterSync(cluster string) error {
	// Get all current workload entries. This appears "racy" to look at one time and act on it, but
	// since we are cleaning up stale resources this is not a problem.
	currentWorkloadEntries := c.workloadEntries.List(PeeringNamespace, klabels.SelectorFromSet(map[string]string{
		SourceClusterLabel: cluster,
	}))
	for _, w := range currentWorkloadEntries {
		// Enqueue it; if it no longer exists it will be removed
		c.queue.Add(typedNamespace{
			NamespacedName: types.NamespacedName{
				Namespace: cluster,
				Name:      w.Labels[ParentServiceNamespaceLabel] + "/" + w.Labels[ParentServiceLabel],
			},
			kind: WorkloadEntry,
		})
	}
	return nil
}

func (c *NetworkWatcher) reconcileFlatWorkloadEntry(tn types.NamespacedName) error {
	log := log.WithLabels("flatworkloadentry", tn)

	clusterID := tn.Namespace
	// format of UID: <cluster>/<group>/<kind>/<namespace>/<name></section-name>
	items := strings.Split(tn.Name, "/")
	if len(items) < 5 {
		// TODO: maybe NACK? probably ok to just ignore
		log.Warnf("received invalid resource name %v", tn.Name)
		return nil
	}
	uidKind := items[2]
	uidNamespace := items[3]
	uidName := items[4]
	if uidKind != "Pod" {
		// We only handle Pod
		return nil
	}

	// because a single RemoteWorkload results in multiple WorkloadEntries (when it needs to be selected by more than one Service)
	// we add a common label to all of the WorkloadEntries so we can reliably clean them up.
	weGroupLabel := fmt.Sprintf("autogenflat.%v.%v.%v", clusterID, uidNamespace, uidName)
	if len(weGroupLabel) > 63 {
		// Use a hash to ensure uniqueness when truncating
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(weGroupLabel)))[:8]
		weGroupLabel = "autogenflat." + hash
	}

	reconcile := func(want []*clientnetworking.WorkloadEntry) error {
		req, err := klabels.NewRequirement(SourceWorkloadLabel, selection.Equals, []string{weGroupLabel})
		if err != nil {
			log.Errorf("failed to create label requirement: %v", err)
			return err
		}
		entries := c.workloadEntries.List(PeeringNamespace, klabels.NewSelector().Add(*req))

		var errs []error
		for _, toDel := range entries {
			// don't delete it
			if nil != slices.FindFunc(want, func(want *clientnetworking.WorkloadEntry) bool {
				return toDel.GetName() == want.GetName() && toDel.GetNamespace() == want.GetNamespace()
			}) {
				continue
			}
			if err := controllers.IgnoreNotFound(c.workloadEntries.Delete(toDel.GetName(), PeeringNamespace)); err != nil {
				errs = append(errs, err)
			}
		}

		for _, we := range want {
			changed, err := CreateOrUpdateIfNeeded(c.workloadEntries, we, workloadEntryChanged)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			log.WithLabels("updated", changed).Infof("workload entry %q processed", we.Name)
		}

		return errors.Join(errs...)
	}

	cluster := c.getClusterByID(cluster.ID(clusterID))
	if cluster == nil {
		log.Infof("workload entry will be deleted (network not found for key %v)", tn.String())
		return reconcile(nil)
	}
	if !cluster.HasSynced() {
		log.Debugf("skipping, network not yet synced")
		return nil
	}
	workload := cluster.workloads.GetKey(tn.Name)
	if workload == nil {
		log.Infof("workload entry will be deleted (key %v not found)", tn.String())
		return reconcile(nil)
	}
	if len(workload.Addresses) == 0 {
		// workload entry is not valid for cross-cluster
		return reconcile(nil)
	}
	address, ok := netip.AddrFromSlice(workload.Addresses[0])
	if !ok {
		// workload entry is not valid for cross-cluster
		return reconcile(nil)
	}

	mergedServices := c.mergedServicesForWorkload(clusterID, cluster, workload)

	// potentially write multiple workloadEntries so we can be selected by multiple services
	wantEntries := slices.MapFilter(mergedServices, func(servicesWithMergedSelector servicesForWorkload) **clientnetworking.WorkloadEntry {
		// the selector may use peering labels, or may use the local service labels
		// so that this WE can be seelcted alongside local pods
		// TODO right now we never re-use this map, so it's not cloned
		labels := servicesWithMergedSelector.selector

		// label to let us list these for cleanup
		labels[SourceWorkloadLabel] = weGroupLabel
		// always write the source cluster
		labels[SourceClusterLabel] = clusterID
		// Indicate we support tunneling. This is for Envoy dataplanes mostly.
		if workload.TunnelProtocol == workloadapi.TunnelProtocol_HBONE {
			labels[model.TunnelLabel] = model.TunnelHTTP
		}
		if workload.GetCanonicalName() != "" {
			labels[label.ServiceWorkloadName.Name] = workload.GetCanonicalName()
		}

		annos := map[string]string{
			// Signal we should use HBONE
			annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
		}

		svcNames := slices.Map(servicesWithMergedSelector.services, serviceForWorkload.federatedName)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(svcNames, ","))))[:12]
		weName := fmt.Sprintf("autogenflat.%v.%v.%v.%v", clusterID, uidNamespace, uidName, hash)
		if len(weName) > 253 {
			weName = weName[:253]
		}

		// by default use the cluster locality based on istio-remote Gateway labels
		locality := cluster.locality
		if workload.Locality != nil {
			region, zone, subzone := workload.Locality.GetRegion(), workload.Locality.GetZone(), workload.Locality.GetSubzone()
			if region != "" {
				locality = region
				if zone != "" {
					locality += "/" + zone
					if subzone != "" {
						locality += "/" + subzone
					}
				}
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
				Address:  address.String(),
				Network:  string(c.localNetwork),
				Ports:    flatWorkloadEntryPorts(servicesWithMergedSelector),
				Weight:   1, // weight,
				Locality: locality,
				// TODO ztunnel (if trustdomain validtion enabled) will validate the local cluster's td
				// we'd need a way to propagate the remote td for to-pod-ip calls with TD validation enabled.
				ServiceAccount: workload.GetServiceAccount(),
			},
		}
		return &we
	})
	return reconcile(wantEntries)
}

// TODO this doesn't detect overlap at all
func flatWorkloadEntryPorts(
	services servicesForWorkload,
) map[string]uint32 {
	svcPortToName := make(map[int32]string)
	var workloadSvcPorts []*workloadapi.Port
	for _, ss := range services.services {
		// when we have a local service, use actual port names
		// otherwise we use port-<number> as the names
		// this must match the logic in reconcileServiceEntry
		// TODO if names conflict across multiple services we must handle that
		// or if we have one locally and the other is remote only, the names are different
		workloadSvcPorts = append(workloadSvcPorts, ss.ports...)

		if ss.local != nil {
			// TODO see if local services have name conflict
			for _, p := range ss.local.Spec.Ports {
				svcPortToName[p.Port] = p.Name
			}
		}
	}

	// the actual port mapping is done on the server who sent the RemoteWorkload
	workloadPorts := make(map[string]uint32, len(workloadSvcPorts))
	for _, p := range workloadSvcPorts {
		sp, tp := p.GetServicePort(), p.GetTargetPort()
		if tp == 0 {
			// shouldn't happen, we usually have every port here, sometimes mapping to itself
			tp = sp
		}

		portName := fmt.Sprintf("port-%d", p.GetServicePort())
		if knownName, ok := svcPortToName[int32(p.GetServicePort())]; ok {
			portName = knownName
		}

		workloadPorts[portName] = tp
	}

	return workloadPorts
}

func workloadEntryChanged(desired *clientnetworking.WorkloadEntry, live *clientnetworking.WorkloadEntry) bool {
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
}

func (c *NetworkWatcher) reconcileGatewayWorkloadEntry(tn types.NamespacedName) error {
	log := log.WithLabels("workloadentry", tn)

	clusterID := tn.Namespace
	ns, name, _ := strings.Cut(tn.Name, "/")

	weName := fmt.Sprintf("autogen.%v.%v.%v", clusterID, ns, name)
	labels := map[string]string{}

	var locality string
	var fs *RemoteFederatedService
	var networkName networkid.ID
	if pc := c.getClusterByID(cluster.ID(clusterID)); pc != nil {
		if !pc.HasSynced() {
			log.Debugf("skipping, cluster not yet synced")
			return nil
		}
		fs = pc.federatedServices.GetKey(tn.String())
		locality = pc.locality
		networkName = pc.networkName
	}
	if fs == nil {
		// No federated service exists, remove
		log.Infof("workload entry will be deleted (key %v not found)", tn.String())
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}

	// HACK: don't peer waypoint services for remote networks, let the gateway route to the waypoint
	if fs.Service.WaypointFor != "" && networkName != c.localNetwork {
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}

	localService := c.services.Get(name, ns)
	weScope := ServiceScopeGlobal
	var localPorts []corev1.ServicePort
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
		localPorts = localService.Spec.Ports
	}
	// Add our identification labels always. If there is no labels this will be used as the selector in the SE
	labels[ParentServiceLabel] = name
	labels[ServiceScopeLabel] = weScope
	labels[ParentServiceNamespaceLabel] = ns
	labels[SourceClusterLabel] = clusterID
	// Indicate we support tunneling. This is for Envoy dataplanes mostly.
	labels[model.TunnelLabel] = model.TunnelHTTP
	fss := fs.Service
	if fss.Waypoint != nil && fss.Waypoint.Name != "" && fss.Waypoint.Namespace != "" {
		labels[RemoteWaypointLabel] = AutogenHostname(fss.Waypoint.Name, fss.Waypoint.Namespace)
	}

	ports := map[string]uint32{}
	for _, p := range fss.Ports {
		if sp := slices.FindFunc(localPorts, func(port corev1.ServicePort) bool {
			return uint32(port.Port) == p.ServicePort && port.TargetPort.Type == intstr.String
		}); sp != nil {
			ports[sp.TargetPort.StrVal] = p.ServicePort
		} else {
			ports[fmt.Sprintf("port-%d", p.ServicePort)] = p.ServicePort
		}
	}
	annos := map[string]string{
		ServiceSubjectAltNamesAnnotation: strings.Join(slices.Sort(fss.SubjectAltNames), ","),
		// Signal we should use HBONE
		annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
	}
	// propagate traffic distribution from this WE to the SE
	if td := fss.GetTrafficDistribution(); td != "" {
		annos[annotation.NetworkingTrafficDistribution.Name] = td
	}

	weight := uint32(0)
	if fss.Capacity != nil {
		weight = fss.Capacity.GetValue()
		if weight == 0 {
			// WorkloadEntry cannot distinguish 0 and unset.
			annos[ServiceEndpointStatus] = ServiceEndpointStatusUnhealthy
		}

	}

	if EnableFlatNetworks && c.localNetwork == networkName {
		// In flat mode, we still make the network WE, but disable it.
		// This is pretty hacky, but the reason is we derive FederatedService --> WorkloadEntry --> ServiceEntry.
		// This is so if there is an outage connecting to a remote, we have the FederatedService state stored locally in order
		// to properly compute the ServiceEntry
		// TODO: john make this per-service opt-in
		annos[ServiceEndpointStatus] = ServiceEndpointStatusUnhealthy
	}

	we := &clientnetworking.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:        weName,
			Namespace:   PeeringNamespace,
			Labels:      labels,
			Annotations: annos,
		},
		Spec: networking.WorkloadEntry{
			Address:  "",
			Network:  networkName.String(),
			Ports:    ports,
			Weight:   weight,
			Locality: locality,
		},
	}

	changed, err := CreateOrUpdateIfNeeded(c.workloadEntries, we, workloadEntryChanged)
	if err != nil {
		return err
	}
	log.WithLabels("updated", changed).Infof("workload entry %q processed", we.Name)
	return nil
}

func (c *NetworkWatcher) reconcileServiceEntry(name types.NamespacedName) error {
	log := log.WithLabels("federatedservice", name)
	hostname := AutogenHostname(name.Name, name.Namespace)
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
	if localService != nil && !HasGlobalLabel(localService.Labels) && !c.isGlobalWaypoint(localService) {
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
		annos[annotation.NetworkingTrafficDistribution.Name] = computeTrafficDistribution(remoteServices)
	}
	se.Annotations = annos

	// Collect all federated services for this service
	var federatedServices []*RemoteFederatedService
	for _, remote := range remoteServices {
		if clusterID := remote.Labels[SourceClusterLabel]; clusterID != "" {
			if pc := c.getClusterByID(cluster.ID(clusterID)); pc != nil {
				ns := remote.Labels[ParentServiceNamespaceLabel]
				serviceName := remote.Labels[ParentServiceLabel]
				key := fmt.Sprintf("%s/%s/%s", clusterID, ns, serviceName)
				if fs := pc.federatedServices.GetKey(key); fs != nil {
					federatedServices = append(federatedServices, fs)
				}
			}
		}
	}

	if localService != nil {
		se.Spec.Ports = convertPorts(localService.Spec.Ports)
		se.Spec.WorkloadSelector = &networking.WorkloadSelector{Labels: localService.Spec.Selector}
	} else {
		se.Spec.Ports = mergeRemotePorts(remoteServices, federatedServices)
		se.Spec.WorkloadSelector = &networking.WorkloadSelector{Labels: map[string]string{
			ParentServiceLabel:          name.Name,
			ParentServiceNamespaceLabel: name.Namespace,
		}}
	}

	// Semantics: sidecar will skip processing if there is a local waypoint (determined by standard waypoint codepath)
	// OR if any cluster has a remote waypoint (determined by remote waypoint label).
	// This has a few awkward edge cases...
	// * If there are 2 remote clusters, and only 1 has a remote waypoint, then the sidecar will skip processing regardless.
	// * If there is a local service but no local waypoint, the sidecar will do processing

	remoteWaypoint := ""
	for _, remote := range remoteServices {
		if rw, f := remote.Labels[RemoteWaypointLabel]; f && rw != "" {
			remoteWaypoint = rw
			break // TODO handle the case where two remote clusters disagree...
		}
	}
	if remoteWaypoint != "" {
		// this label now effectively overrides use-waypoint!
		// we should only overwrite the label if the local doesn't use-waypoint,
		// or use-waypoint points to the same logical service
		remoteWaypointName, remoteWaypointNs, useRemote := ParseAutogenHostname(remoteWaypoint)
		if useRemote && localService != nil {
			localWaypointName := localService.GetLabels()[label.IoIstioUseWaypoint.Name]
			localWaypointNs := ptr.NonEmptyOrDefault(localService.GetLabels()[label.IoIstioUseWaypointNamespace.Name], localService.Namespace)
			if localWaypointName != "" && (localWaypointName != remoteWaypointName || localWaypointNs != remoteWaypointNs) {
				useRemote = false
			}
		}

		if useRemote {
			labels[RemoteWaypointLabel] = remoteWaypoint
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

// computeTrafficDistribution attempts to find the value for networking.istio.io/traffic-distribution
// that all the remoteServices agree upon. remoteServices that don't specify any value are not considered.
func computeTrafficDistribution(remoteServices []*clientnetworking.WorkloadEntry) string {
	consensusDistribution := ""
	for _, remote := range remoteServices {
		distribution, ok := remote.GetAnnotations()[annotation.NetworkingTrafficDistribution.Name]
		if !ok {
			// if a single remote doesn't specify, inherit from remote services that do
			continue
		}

		if consensusDistribution == "" {
			consensusDistribution = distribution
		}

		if distribution != consensusDistribution {
			log.Warnf(
				"remote services do not agree on %s: %q != %q; falling back to %s",
				annotation.NetworkingTrafficDistribution.Name,
				consensusDistribution,
				distribution,
				DefaultTrafficDistribution,
			)
			consensusDistribution = ""
			break
		}
	}

	// If there is no local service and remotes disagree or have no value, prefer network by default
	if consensusDistribution != "" {
		return consensusDistribution
	}

	return DefaultTrafficDistribution
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
	case Gateway:
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
		}, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeerConnected,
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
	peerCluster := c.getClusterByGateway(name)
	if peerCluster == nil {
		log.Debugf("unable to set status for gateway %q, no cluster found", name)
		return nil
	}

	if peerCluster.IsConnected() {
		gatewayConditions = append(gatewayConditions, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeerConnected,
			Status:             metav1.ConditionTrue,
			Reason:             string(k8s.GatewayReasonProgrammed),
			LastTransitionTime: metav1.Now(),
		})
	} else {
		gatewayConditions = append(gatewayConditions, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeerConnected,
			Status:             metav1.ConditionFalse,
			Reason:             string(k8s.GatewayReasonPending),
			Message:            "not connected to peer",
			LastTransitionTime: metav1.Now(),
		})
	}

	if !peerCluster.HasSynced() {
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
		kind:           Gateway,
	})
}
