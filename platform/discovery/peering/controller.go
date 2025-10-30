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
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	istiolog "istio.io/istio/pkg/log"
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
	GatewayServiceEntry
	NodeWorkloadEntry
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
	case GatewayServiceEntry:
		return "GatewayServiceEntry"
	case NodeWorkloadEntry:
		return "NodeWorkloadEntry"
	}
	return "Unknown"
}

func AutogenHostname(name, ns, domain string) string {
	if domain == "" {
		domain = strings.TrimPrefix(DomainSuffix, ".")
	}
	return fmt.Sprintf("%s.%s.%s", name, ns, domain)
}

func ParseAutogenHostname(hostname, domain string) (string, string, bool) {
	if domain == "" {
		domain = strings.TrimPrefix(DomainSuffix, ".")
	}
	suffix := "." + domain
	nns, suffixOk := strings.CutSuffix(hostname, suffix)
	name, ns, nnsOk := strings.Cut(nns, ".")
	return name, ns, suffixOk && nnsOk
}

// ParseAutogenHostnameGuessDomain parses a hostname of the form "name.namespace.domain[.more.parts]"
// This is useful when we know the domain comes from a generated peering resource and we can safely assume
// the first two parts are name and namespace.
func ParseAutogenHostnameGuessDomain(wpAutogenHost string) (string, string, string, bool) {
	if wpAutogenHost == "" {
		return "", "", "", false
	}

	parts := strings.Split(wpAutogenHost, ".")
	if len(parts) < 3 {
		return "", "", "", false
	}

	// The domain is everything after name and namespace
	wpDomain := strings.Join(parts[2:], ".")

	// Now parse with the extracted domain to validate and extract name/namespace
	wpName, wpNs, ok := ParseAutogenHostname(wpAutogenHost, wpDomain)
	if !ok {
		return "", "", "", false
	}
	return wpName, wpNs, wpDomain, true
}

func NodePortPeeringWellKnownHost(serviceAccount, network string) string {
	return fmt.Sprintf("node.%s.%s%s", serviceAccount, network, DomainSuffix)
}

// ServiceEntryName generates the ServiceEntry name for a given service and segment.
// For the default segment, it returns "autogen.namespace.serviceName"
// For other segments, it returns "autogen.segmentName.namespace.serviceName"
func ServiceEntryName(serviceName, namespace, segment string) string {
	if segment == DefaultSegmentName || segment == "" {
		return fmt.Sprintf("autogen.%s.%s", namespace, serviceName)
	}
	return fmt.Sprintf("autogen.%s.%s.%s", segment, namespace, serviceName)
}

func (c *NetworkWatcher) Reconcile(raw any) error {
	if _, ok := raw.(cachesSyncedMarker); ok {
		c.gatewaysProcessed = true
		return nil
	}
	key := raw.(typedNamespace)
	if key.kind != Gateway && !c.gatewaysProcessed {
		log.WithLabels("kind", key.kind, "resource", key.NamespacedName).Debugf("requeueing %v because gateways are not synced", key.kind)
		c.queue.Add(raw)
		return nil
	}
	log := log.WithLabels("kind", key.kind, "resource", key.NamespacedName)
	log.Infof("reconciling")
	defer log.Infof("reconciled")
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
		c.reconcileClusterSync(key.Name)
		return nil
	case NodeWorkloadEntry:
		// NodeWorkloadEntry: triggered when we may need to change a generated WE
		// The key is: Name=name of target node, Namespace=cluster
		return c.reconcileNodeWorkloadEntry(key.NamespacedName)
	case GatewayServiceEntry:
		// GatewayServiceEntry: triggered when we may need to change a generated gateway-based SE
		// The key is: Name=name and Namespace=namespace of gateway the SE is derived from
		return c.reconcileGatewayServiceEntry(key.NamespacedName)
	default:
		log.Errorf("unknown resource kind: %v", key.kind)
	}
	return nil
}

func (c *NetworkWatcher) reconcileGateway(name types.NamespacedName) error {
	log := log.WithLabels("gateway", name)

	c.mu.Lock()
	defer c.mu.Unlock()

	// don't use getters here, they rlock
	localNetwork := c.localNetwork

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
		wasFlat := cluster.IsFlat()
		isFlat := EnableFlatNetworks && peer.Network == localNetwork
		if oldClusterID != peer.Cluster || oldNetworkID != peer.Network || wasFlat != isFlat {
			// total cluster update, id or network changed
			log.Infof(
				"cluster changed from %v/%v (%s) to %v/%v (%s)",
				oldClusterID, oldNetworkID, wasFlat,
				peer.Cluster, peer.Network, isFlat,
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
			// always enqueue a status update for the gateway since labels or annotations may have changed which affects status of data plane programming
			c.enqueueStatusUpdate(name)
			return nil
		}
	}
	if peer.Cluster == c.localCluster {
		log.Infof("peer gateway to %v is for local cluster network, ignoring", peer.Address)
		return nil
	}

	// set server side filtering to only peer node workloads, unless in flat networking mode
	peeringNodesOnly := true
	if EnableFlatNetworks && peer.Network == c.localNetwork {
		peeringNodesOnly = false
	}

	peerCluster := newPeerCluster(
		localNetwork,
		peer,
		c.buildConfig(fmt.Sprintf("peering-%s", peer.Cluster), peeringNodesOnly),
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
			if strings.Contains(obj.Workload.GetUid(), "Node/") {
				c.queue.Add(typedNamespace{
					NamespacedName: types.NamespacedName{
						Namespace: obj.Cluster,
						Name:      obj.ResourceName(),
					},
					kind: NodeWorkloadEntry,
				})
				return
			}
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
func (c *NetworkWatcher) reconcileClusterSync(cid string) {
	// Get all current workload entries. This appears "racy" to look at one time and act on it, but
	// since we are cleaning up stale resources this is not a problem.
	currentWorkloadEntries := c.workloadEntries.List(PeeringNamespace, klabels.SelectorFromSet(map[string]string{
		SourceClusterLabel: cid,
	}))

	seen := sets.New[types.NamespacedName]()
	for _, w := range currentWorkloadEntries {
		nn := types.NamespacedName{
			Namespace: cid,
			Name:      w.Labels[ParentServiceNamespaceLabel] + "/" + w.Labels[ParentServiceLabel],
		}
		// Enqueue it; if it no longer exists it will be removed
		c.queue.Add(typedNamespace{
			NamespacedName: nn,
			kind:           WorkloadEntry,
		})
		seen.Insert(nn)
	}

	// also queue federatedServices to handle workloadEntries we didn't create before this Cluster event
	pc := c.getClusterByID(cluster.ID(cid))
	if pc == nil {
		return
	}

	for _, fs := range pc.federatedServices.List() {
		nn := types.NamespacedName{
			Namespace: cid,
			Name:      fs.Service.Namespace + "/" + fs.Service.Name,
		}
		if seen.Contains(nn) {
			continue
		}
		c.queue.Add(typedNamespace{
			NamespacedName: nn,
			kind:           WorkloadEntry,
		})
	}
}

func (c *NetworkWatcher) genericReconcileWorkloadEntries(want []*clientnetworking.WorkloadEntry, groupLabel string) error {
	var errs []error

	req, err := klabels.NewRequirement(SourceWorkloadLabel, selection.Equals, []string{groupLabel})
	if err != nil {
		log.Errorf("failed to create label requirement: %v", err)
		return err
	}
	entries := c.workloadEntries.List(PeeringNamespace, klabels.NewSelector().Add(*req))

	// cleanup existing workload entries that match the group label but were not generated
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

	// create or update the workload entries that we generated
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

	segment := c.getSegmentForCluster(clusterID)
	segmentDomain, err := c.getSegmentDomain(segment)
	if err != nil {
		log.Errorf("cannot get domain for segment %s: %v, deleting WorkloadEntry", segment, err)
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(tn.Name, PeeringNamespace))
	}

	// because a single RemoteWorkload results in multiple WorkloadEntries (when it needs to be selected by more than one Service)
	// we add a common label to all of the WorkloadEntries so we can reliably clean them up.
	weGroupLabel := fmt.Sprintf("autogenflat.%v.%v.%v", clusterID, uidNamespace, uidName)
	if len(weGroupLabel) > 63 {
		// Use a hash to ensure uniqueness when truncating
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(weGroupLabel)))[:8]
		weGroupLabel = "autogenflat." + hash
	}

	cluster := c.getClusterByID(cluster.ID(clusterID))
	if cluster == nil {
		log.Infof("workload entry will be deleted (network not found for key %v)", tn.String())
		return c.genericReconcileWorkloadEntries(nil, weGroupLabel)
	}
	if !cluster.HasSynced() {
		log.Debugf("skipping, cluster not yet synced")
		return nil
	}
	workload := cluster.workloads.GetKey(tn.Name)
	if workload == nil {
		log.Infof("workload entry will be deleted (key %v not found)", tn.String())
		return c.genericReconcileWorkloadEntries(nil, weGroupLabel)
	}
	if len(workload.Addresses) == 0 {
		// workload entry is not valid for cross-cluster
		return c.genericReconcileWorkloadEntries(nil, weGroupLabel)
	}
	address, ok := netip.AddrFromSlice(workload.Addresses[0])
	if !ok {
		// workload entry is not valid for cross-cluster
		return c.genericReconcileWorkloadEntries(nil, weGroupLabel)
	}

	mergedServices := c.mergedServicesForWorkload(workload, segment, segmentDomain)

	// potentially write multiple workloadEntries so we can be selected by multiple services
	wantEntries := slices.MapFilter(mergedServices, func(servicesWithMergedSelector servicesForWorkload) **clientnetworking.WorkloadEntry {
		// the selector may use peering labels, or may use the local service labels
		// so that this WE can be seelcted alongside local pods
		// TODO right now we never re-use this map, so it's not cloned
		labels := servicesWithMergedSelector.selector
		annos := map[string]string{}

		annos[PeeredWorkloadUIDAnnotation] = tn.Name
		// label to let us list these for cleanup
		labels[SourceWorkloadLabel] = weGroupLabel
		// always write the source cluster
		labels[SourceClusterLabel] = clusterID
		// always write segment and domain
		labels[SegmentLabel] = segment
		// Indicate we support tunneling. This is for Envoy dataplanes mostly.
		if workload.TunnelProtocol == workloadapi.TunnelProtocol_HBONE {
			labels[model.TunnelLabel] = model.TunnelHTTP
			annos[annotation.AmbientRedirection.Name] = constants.AmbientRedirectionEnabled
		}
		if workload.GetCanonicalName() != "" {
			labels[label.ServiceWorkloadName.Name] = workload.GetCanonicalName()
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
				Network:  string(c.GetLocalNetwork()),
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
	return c.genericReconcileWorkloadEntries(wantEntries, weGroupLabel)
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
	pc := c.getClusterByID(cluster.ID(clusterID))
	if pc == nil {
		log.Infof("workload entry will be deleted (cluster not found for key %v)", tn.String())
		// Actually delete the WorkloadEntry since the cluster is gone
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}
	if !pc.HasSynced() {
		log.Debugf("skipping, cluster not yet synced")
		return nil
	}

	segmentInfo := pc.GetSegmentInfo()
	if segmentInfo == nil {
		// this should never happen, we should set segmentInfo to the default elsewhere
		log.Debugf("cluster %s has no segment info, using default segment", clusterID)
		segmentInfo = &workloadapi.Segment{
			Name:   DefaultSegmentName,
			Domain: strings.TrimPrefix(DomainSuffix, "."),
		}
	}
	segmentName := ptr.NonEmptyOrDefault(segmentInfo.GetName(), DefaultSegmentName)
	segmentDomain := segmentInfo.GetDomain()
	if !c.validatePeerSegment(cluster.ID(clusterID), segmentInfo) {
		log.Debugf("cluster %s has invalid segment configuration, removing workload entry", clusterID)
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}

	fs = pc.federatedServices.GetKey(tn.String())
	locality = pc.locality
	networkName = pc.networkName

	if fs == nil {
		// No federated service exists, remove
		log.Infof("workload entry will be deleted (key %v not found)", tn.String())
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}

	// HACK: don't peer waypoint services for remote networks, let the gateway route to the waypoint
	if fs.Service.WaypointFor != "" && networkName != c.GetLocalNetwork() {
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}

	localService := c.services.Get(name, ns)
	if segmentName != c.GetLocalSegment() {
		localService = nil // we are not in the same segment, ignore local service
	}

	weScope := model.SoloServiceScopeFromProto(fs.Service.Scope)
	if !weScope.IsPeered() {
		// Not peered, remove
		// This should NEVER happen, FederatedServices shouldn't be sending local scoped services
		log.Warnf("federated service %v/%v is not peered, should not have received local scoped service, removing workload entry", ns, name)
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}
	if weScope == model.SoloServiceScopeSegment && segmentName != c.GetLocalSegment() {
		// Segment scoped, but not our segment, removed
		log.Infof("federated service %v/%v is segment scoped for segment %q, not local segment %q, "+
			"removing workload entry", ns, name, segmentName, c.GetLocalSegment())
		return controllers.IgnoreNotFound(c.workloadEntries.Delete(weName, PeeringNamespace))
	}

	var localPorts []corev1.ServicePort
	if localService != nil {
		var nsLabels map[string]string
		if ns := c.namespaces.Get(ns, ""); ns != nil {
			nsLabels = ns.GetLabels()
		}
		scope := CalculateScope(localService.GetLabels(), nsLabels)
		if !scope.IsPeered() {
			// It's not peered, so ignore it
			localService = nil
		} else {
			weScope = scope
			// If the local service exists, the SE is going to set the label selectors to the service's so we can select the Pod
			// We need to include those
			labels = maps.Clone(localService.Spec.Selector)
			if labels == nil {
				labels = map[string]string{}
			}
			localPorts = localService.Spec.Ports
		}
	}

	// Add our identification labels always. If there is no labels this will be used as the selector in the SE
	labels[SegmentLabel] = segmentName
	labels[ParentServiceLabel] = name
	labels[ServiceScopeLabel] = string(weScope)

	// Compute takeover with proper priority: explicit label > global-only, local > ns > remote
	var localSvcLabels, nsLabels, remoteLabels map[string]string

	// Get remote takeover status from FederatedService
	if fs.Service.Takeover {
		remoteLabels = map[string]string{ServiceTakeoverLabel: "true"}
	}

	// Get labels from local service and namespace
	if localService != nil {
		localSvcLabels = localService.GetLabels()
		nsObj := ptr.OrEmpty(kclient.New[*corev1.Namespace](c.client).Get(localService.GetNamespace(), ""))
		nsLabels = nsObj.GetLabels()
	}

	// Use shared function with proper priority
	if shouldTakeoverInternal(localSvcLabels, remoteLabels, nsLabels) {
		labels[ServiceTakeoverLabel] = "true"
	}
	labels[ParentServiceNamespaceLabel] = ns
	labels[SourceClusterLabel] = clusterID
	// Indicate we support tunneling. This is for Envoy dataplanes mostly.
	labels[model.TunnelLabel] = model.TunnelHTTP
	fss := fs.Service
	if fss.Waypoint != nil && fss.Waypoint.Name != "" && fss.Waypoint.Namespace != "" {
		waypointDomain := segmentDomain
		if waypointDomain == "" {
			waypointDomain = strings.TrimPrefix(DomainSuffix, ".")
		}
		labels[UseGlobalWaypointLabel] = AutogenHostname(fss.Waypoint.Name, fss.Waypoint.Namespace, waypointDomain)
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

	// store protocols for use on the ServiceEntry
	if protocolsStr := serializeProtocolsByPort(fss.ProtocolsByPort); protocolsStr != "" {
		annos[ServiceProtocolsAnnotation] = protocolsStr
	}

	weight := uint32(0)
	if fss.Capacity != nil {
		weight = fss.Capacity.GetValue()
		if weight == 0 {
			// WorkloadEntry cannot distinguish 0 and unset.
			annos[ServiceEndpointStatus] = ServiceEndpointStatusUnhealthy
		}

	}

	if EnableFlatNetworks && c.GetLocalNetwork() == networkName {
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

// reconcileServiceEntry converts the WorkloadEntry from reconcileGatewayWorkloadEntry
// into a ServiceEntry. Avoid querying anything that comes from xDS here, instead prefer
// information serialized to the reconcileGatewayWorkloadEntry annotations/labels.
// This should prevent race conditions between WE and SE processing, and better handle
// restarts or outage scenarios.
func (c *NetworkWatcher) reconcileServiceEntry(name types.NamespacedName) error {
	serviceNs := name.Namespace
	serviceName := name.Name

	// Group WorkloadEntries by segment
	remoteServicesBySegment := make(map[string][]*clientnetworking.WorkloadEntry)
	allRemoteServices := c.workloadEntryServiceIndex.Lookup(name)
	for _, we := range allRemoteServices {
		segmentLabel := we.Labels[SegmentLabel]
		if segmentLabel == "" {
			segmentLabel = DefaultSegmentName
		}
		remoteServicesBySegment[segmentLabel] = append(remoteServicesBySegment[segmentLabel], we)
	}

	// Check if local service exists and get its segment
	localService := c.services.Get(serviceName, serviceNs)
	localSegment := ""
	var nsLabels map[string]string
	if localService != nil {
		ns := ptr.OrEmpty(kclient.New[*corev1.Namespace](c.client).Get(serviceNs, ""))
		nsLabels = ns.GetLabels()
		scope := CalculateScope(localService.Labels, nsLabels)
		if scope.IsPeered() || c.isGlobalWaypoint(localService) {
			localSegment = c.GetLocalSegment()
			// Add local segment to the map even if no remote services exist for it
			// Lets us decide whether to cleanup below
			if _, exists := remoteServicesBySegment[localSegment]; !exists {
				remoteServicesBySegment[localSegment] = []*clientnetworking.WorkloadEntry{}
			}
		} else {
			// Not a global service or waypoint, clear it
			localService = nil
			localSegment = ""
			nsLabels = nil
		}
	}

	// Now reconcile a ServiceEntry for each active segment
	var errs []error
	for segmentName, remoteServices := range remoteServicesBySegment {
		segmentLocal := segmentName == localSegment

		var localServiceForSegment *corev1.Service
		var nsLabelsForSegment map[string]string
		if segmentLocal {
			localServiceForSegment = localService
			nsLabelsForSegment = nsLabels
		}
		if err := c.reconcileServiceEntryForSegment(
			serviceName,
			serviceNs,
			segmentName,
			localServiceForSegment,
			nsLabelsForSegment,
			remoteServices,
		); err != nil {
			errs = append(errs, err)
		}
	}

	// Cleanup serviceEntries for segments where this Service no longer exists
	// in any of the segments where we have a generated WorkloadEntry.
	// This must be done last so that migrating from segment-less to segment-aware
	// control plane doesn't cause a blip.
	existingServiceEntries := c.serviceEntries.List(PeeringNamespace, klabels.Everything()) // lister is cached, using a label selector is actually slower
	for _, se := range existingServiceEntries {
		if se.Labels[ParentServiceLabel] != serviceName || se.Labels[ParentServiceNamespaceLabel] != serviceNs {
			continue // not the current service
		}
		segment := se.Labels[SegmentLabel]
		if _, f := remoteServicesBySegment[segment]; f {
			continue // don't cleanup
		}
		log.WithLabels(
			"serviceentry",
			se.Name,
			"segment",
			segment,
		).Info("service entry will be deleted (no longer needed for any segment)")
		// This SE is for a segment that no longer exists, delete it
		if err := controllers.IgnoreNotFound(c.serviceEntries.Delete(se.Name, PeeringNamespace)); err != nil {
			log.Errorf("failed to delete ServiceEntry %s: %v", se.Name, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// reconcileServiceEntryForSegment handles a single segment for a service
func (c *NetworkWatcher) reconcileServiceEntryForSegment(
	serviceName,
	serviceNs,
	segmentName string,
	localService *corev1.Service,
	nsLabels map[string]string,
	remoteServices []*clientnetworking.WorkloadEntry,
) error {
	log := log.WithLabels("segment", segmentName, "service", serviceName, "namespace", serviceNs)

	// Get the segment domain
	segmentDomain, err := c.getSegmentDomain(segmentName)
	if err != nil {
		log.Errorf("cannot get domain for segment %s: %v, deleting ServiceEntry", segmentName, err)
		seName := ServiceEntryName(serviceName, serviceNs, segmentName)
		return controllers.IgnoreNotFound(c.serviceEntries.Delete(seName, PeeringNamespace))
	}

	hostname := AutogenHostname(serviceName, serviceNs, segmentDomain)
	seName := ServiceEntryName(serviceName, serviceNs, segmentName)
	log.Debugf("reconciling ServiceEntry %s with hostname %s", seName, hostname)

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

	// Compute takeover with proper priority: explicit label > global-only, local svc > ns > remote
	takeover := false
	if c.GetLocalSegment() == segmentName {
		// We can only do takeover for our local segment
		var localLabels map[string]string
		if localService != nil {
			localLabels = localService.GetLabels()
		}
		// Aggregate remote labels - check if ANY remote WE indicates takeover
		remoteLabels := map[string]string{}
		for _, remote := range remoteServices {
			// Check for takeover label or global-only scope on any remote
			if takeoverLabel, ok := remote.Labels[ServiceTakeoverLabel]; ok && takeoverLabel == "true" {
				remoteLabels[ServiceTakeoverLabel] = "true"
			}
			if scope, ok := remote.Labels[ServiceScopeLabel]; ok && scope == string(model.SoloServiceScopeGlobalOnly) {
				remoteLabels[ServiceScopeLabel] = scope
			}
		}
		takeover = shouldTakeoverInternal(localLabels, remoteLabels, nsLabels)
	}

	if localService == nil && takeover {
		// Normally we rely on the local service to exist to program DNS for standard
		// k8s hostnames, but if a takeover service is only on the remote side, we can put those hostnames
		// into the hosts lists on the SE.
		// TODO: until OSS https://github.com/istio/istio/pull/57782 merges, this case BREAKS when using
		// Segments as when we transition SEs from the default segment to their autogen.segment.name.ns copies
		// we will momentarily have overlapping hostnames which is something istiod does not recover from
		// even after the duplicate is cleaned up! We have that specific test case currently skipped in ITs.
		// We don't need to add the variants here, zTunnel DNS will handle that.
		se.Spec.Hosts = append(se.Spec.Hosts,
			fmt.Sprintf("%s.%s.svc.%s", serviceName, serviceNs, c.clusterDomain),
		)
	}
	scope := ServiceScopeCluster
	if localService != nil {
		scope = CalculateScope(localService.Labels, nsLabels)
		if !scope.IsPeered() && !c.isGlobalWaypoint(localService) {
			// It's not global, so ignore it
			localService = nil
		}
	}
	// Note: When there's no local service, we keep scope as cluster.
	// We don't inherit the remote's scope because that's for THEIR cluster, not ours.

	if scope == model.SoloServiceScopeSegment && segmentName != c.GetLocalSegment() {
		// Segment scopes, but the segment we're processing is not our local segment
		// we need to delete the SE
		log.Infof("service entry will be deleted (local service is segment scoped for segment %q, not %q)", c.GetLocalSegment(), segmentName)
		return controllers.IgnoreNotFound(c.serviceEntries.Delete(se.Name, PeeringNamespace))
	}

	if localService == nil && len(remoteServices) == 0 {
		log.Infof("service entry will be deleted (no local or remote services)")
		return controllers.IgnoreNotFound(c.serviceEntries.Delete(se.Name, PeeringNamespace))
	}

	labels := make(map[string]string)
	if localService != nil && localService.Labels != nil {
		labels = maps.Clone(localService.Labels)
	}
	labels[ParentServiceLabel] = serviceName
	labels[ParentServiceNamespaceLabel] = serviceNs
	labels[ServiceScopeLabel] = string(scope)
	if takeover {
		labels[ServiceTakeoverLabel] = "true"
	}
	labels[SegmentLabel] = segmentName
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

	if localService != nil {
		se.Spec.Ports = convertPorts(localService.Spec.Ports)
		se.Spec.WorkloadSelector = &networking.WorkloadSelector{Labels: localService.Spec.Selector}
	} else {
		se.Spec.Ports = mergeRemotePorts(remoteServices)
		se.Spec.WorkloadSelector = &networking.WorkloadSelector{Labels: map[string]string{
			ParentServiceLabel:          serviceName,
			ParentServiceNamespaceLabel: serviceNs,
			SegmentLabel:                segmentName,
		}}
	}

	globalWaypointHost := ""
	globalWaypointDomain := ""
	haveLocalNet := localService != nil
	localNetwork := c.GetLocalNetwork()
	for _, remote := range remoteServices {
		haveLocalNet = haveLocalNet || remote.Spec.Network == string(localNetwork)
		if rw, f := remote.Labels[UseGlobalWaypointLabel]; f && rw != "" {
			globalWaypointHost = rw

			// The waypoint must be in the same segment as the service
			remoteSegment := remote.Labels[SegmentLabel]
			if d, err := c.getSegmentDomain(remoteSegment); err == nil {
				globalWaypointDomain = d
			} else {
				globalWaypointDomain = strings.TrimPrefix(DomainSuffix, ".")
				log.Warnf("cannot get domain for segment %s, default to %q: %v", remoteSegment, globalWaypointDomain, err)
			}
			break // TODO handle the case where two remote clusters disagree...
		}
	}

	if globalWaypointHost != "" {
		// Semantics: sidecar will skip processing if there is a local waypoint (determined by standard waypoint codepath)
		// OR if any cluster has a remote waypoint (determined by remote waypoint label).
		// This has a few awkward edge cases...
		// * If there are 2 remote clusters, and only 1 has a remote waypoint, then the sidecar will skip processing regardless.
		// * If there is a local service but no local waypoint, the sidecar will do processing
		labels[RemoteWaypointLabel] = "true"

		// Making the ServiceEntry use the global version of the Waypoint
		// is separate from deciding to skip processing. We only do it if the service is at least partially
		// network local. We're effectively overriding istio.io/use-waypoint here.
		if haveLocalNet {
			// Check if we have a valid hostname for global waypoint
			remoteWaypointName, remoteWaypointNs, useRemote := ParseAutogenHostname(globalWaypointHost, globalWaypointDomain)
			localWaypointName, localWaypointNs, explicitNone := c.waypointForService(localService)
			localSpecifiesWaypoint := localWaypointName != "" || explicitNone

			// if we have local service, and it sets use-waypoint in any way, that takes precedence
			if useRemote && localSpecifiesWaypoint {
				if explicitNone {
					// local service explicitly says "none", do not use the global waypoint
					useRemote = false
				} else if localWaypointName != remoteWaypointName || localWaypointNs != remoteWaypointNs {
					// we have a local waypoint that is different than the remote one, do not use the global waypoint
					useRemote = false
				}
			}

			// Set the label to use the global copy of the waypoint.
			// For flat network, use the implicitly peered copy of the waypoint.
			// NOTE: RemoteWaypointLabel is slightly different!
			if useRemote {
				labels[UseGlobalWaypointLabel] = globalWaypointHost
			}
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

// reconcileGatewayServiceEntry creates a ServiceEntry from a gateway resource for nodeport peering
func (c *NetworkWatcher) reconcileGatewayServiceEntry(name types.NamespacedName) error {
	log := log.WithLabels("gateway serviceentry", name)

	var (
		gwName = name.Name
		gwNs   = name.Namespace
		seNs   = PeeringNamespace
	)

	gw := c.gateways.Get(gwName, gwNs)
	if gw == nil {
		// gw delete event, look up SE by parent gw label
		gwNameReq, err := klabels.NewRequirement(ParentGatewayLabel, selection.Equals, []string{gwName})
		if err != nil {
			return err
		}
		gwNsReq, err := klabels.NewRequirement(ParentGatewayNamespaceLabel, selection.Equals, []string{gwNs})
		if err != nil {
			return err
		}
		seList := c.serviceEntries.List(seNs, klabels.NewSelector().Add(*gwNameReq, *gwNsReq))
		if len(seList) == 0 {
			// no SE found, ignore
			return nil
		}
		// only one SE should be found
		if len(seList) > 1 {
			log.Warnf(
				"multiple gateway service entries found for parent gateway %s/%s delete event, but should only be one. deleting the first.",
				gwNs,
				gwName,
			)
		}
		return controllers.IgnoreNotFound(c.serviceEntries.Delete(seList[0].Name, seNs))
	}

	cluster := gw.GetLabels()[label.TopologyCluster.Name]
	serviceAccount := gw.GetAnnotations()[constants.GatewayServiceAccountAnnotation]
	network := gw.GetLabels()[label.TopologyNetwork.Name]

	se := &clientnetworking.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				ParentServiceLabel:          serviceAccount,
				ParentServiceNamespaceLabel: gwNs,
				// used to direct SE events to this reconciler
				NodePeerClusterLabel: cluster,
				// used to determine gw this SE is derived from when SE events are triggered
				ParentGatewayNamespaceLabel: gwNs,
				ParentGatewayLabel:          gwName,
			},
			Name:      fmt.Sprintf("autogen.peering.%v.%v", serviceAccount, network),
			Namespace: seNs,
		},
		Spec: networking.ServiceEntry{
			Hosts: []string{NodePortPeeringWellKnownHost(serviceAccount, network)},
			Ports: []*networking.ServicePort{
				{
					// use the wellknown hbone port
					Number:   model.HBoneInboundListenPort,
					Protocol: "TCP",
					Name:     fmt.Sprintf("%d", model.HBoneInboundListenPort),
				},
			},
			Addresses:  nil,
			Resolution: networking.ServiceEntry_STATIC,
			WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{
				NodePeerClusterLabel: cluster,
			}},
		},
	}

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
	log.WithLabels("updated", changed).Infof("gateway service entry processed")
	return nil
}

// parseNodeUID parses the node uid into cluster and node name.
// if the uid's Kind is not Node, it returns an error.
// format of uid: <cluster id>//Node/<node name>
func parseNodeUID(uid string) (string, string, error) {
	parts := strings.Split(uid, "/")
	if len(parts) < 4 {
		return "", "", fmt.Errorf("received invalid resource name %v, expected <cluster>//Node/<node name>", uid)
	}
	clusterID := parts[0]
	kind := parts[2]
	nodeName := parts[3]
	if kind != "Node" {
		return "", "", fmt.Errorf("received invalid resource kind %s, expected Node", kind)
	}
	return clusterID, nodeName, nil
}

func (c *NetworkWatcher) reconcileNodeWorkloadEntry(tn types.NamespacedName) error {
	log := log.WithLabels("node workloadentry", tn)

	clusterID, uidName, err := parseNodeUID(tn.Name)
	if err != nil {
		// not a node workload entry, ignore instead of returning an error so we don't reconcile again
		log.Warn(err.Error())
		return nil
	}

	// a Node can have multiple addresses, so we create a separate WorkloadEntry for each address
	// add a common label to all of the WorkloadEntries to reliably clean them up.
	weGroupLabel := fmt.Sprintf("autogen.node.%v.%v", clusterID, uidName)
	if len(weGroupLabel) > 63 {
		// use a hash to ensure uniqueness when truncating
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(weGroupLabel)))[:8]
		weGroupLabel = "autogen.node." + hash
	}

	cluster := c.getClusterByID(cluster.ID(clusterID))
	if cluster == nil {
		log.Infof("node workload entry will be deleted (cluster not found for key %v)", tn.String())
		return c.genericReconcileWorkloadEntries(nil, weGroupLabel)
	}
	if !cluster.HasSynced() {
		log.Debugf("skipping, cluster not yet synced")
		return nil
	}
	// get the Node RemoteWorkload object
	workload := cluster.workloads.GetKey(tn.Name)
	if workload == nil {
		log.Infof("node workload entry will be deleted (key %v not found)", tn.String())
		return c.genericReconcileWorkloadEntries(nil, weGroupLabel)
	}
	if len(workload.Addresses) == 0 {
		// node workload entry is not valid for cross-cluster
		return c.genericReconcileWorkloadEntries(nil, weGroupLabel)
	}

	// pull out the ports for the node, mapping target port to service port (i.e. hbone nodeport)
	// target port is wellknown hbone port so SE port matches
	ports := map[string]uint32{}
	for _, portList := range workload.Services {
		for _, port := range portList.Ports {
			ports[fmt.Sprintf("%d", port.GetTargetPort())] = port.GetServicePort()
		}
	}

	// find the gateway-based SE that selects this node WE by matching the node peer cluster label workload selector
	// so that we can copy the parent service labels to the node WE
	var seParentServiceLabel, seParentServiceNamespaceLabel, seParentGatewayLabel, seParentGatewayNamespaceLabel string

	req, err := klabels.NewRequirement(NodePeerClusterLabel, selection.Equals, []string{clusterID})
	if err != nil {
		log.Errorf("failed to create label requirement: %v", err)
		return err
	}
	for _, serviceEntry := range c.serviceEntries.List(PeeringNamespace, klabels.NewSelector().Add(*req)) {
		if serviceEntry.Spec.WorkloadSelector.Labels[NodePeerClusterLabel] == clusterID {
			// need parent svc info to form svc key hostname for svc lookup in wds (to associate NodePort SE with Node IP WE)
			seParentServiceLabel = serviceEntry.Labels[ParentServiceLabel]
			seParentServiceNamespaceLabel = serviceEntry.Labels[ParentServiceNamespaceLabel]
			// set parent gw labels on node WE for gateway lookup during envoy config generation
			seParentGatewayLabel = serviceEntry.Labels[ParentGatewayLabel]
			seParentGatewayNamespaceLabel = serviceEntry.Labels[ParentGatewayNamespaceLabel]
			break
		}
	}
	// we should always find the gw SE for node WE
	// but if we don't, return an error so we requeue/reconcile again
	if seParentServiceLabel == "" || seParentServiceNamespaceLabel == "" || seParentGatewayLabel == "" || seParentGatewayNamespaceLabel == "" {
		return fmt.Errorf(
			"failed to apply parent svc and parent gw labels on node WE %s; either no SE found or SE does not have necessary parent labels",
			tn.String(),
		)
	}

	// write a separate WorkloadEntry for each address
	wantEntries := make([]*clientnetworking.WorkloadEntry, 0, len(workload.Addresses))
	for _, address := range workload.Addresses {
		address, ok := netip.AddrFromSlice(address)
		if !ok {
			continue
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
		annos := map[string]string{}
		labels := map[string]string{}

		annos[PeeredWorkloadUIDAnnotation] = tn.Name
		// label to let us list these for cleanup
		labels[SourceWorkloadLabel] = weGroupLabel
		// always write the source cluster
		labels[SourceClusterLabel] = clusterID
		// label to indicate this is a node of a specific cluster
		labels[NodePeerClusterLabel] = clusterID
		// set a prefix on parent service label so this WE isn't included in lookups for generating SEs
		labels[ParentServiceLabel] = "node." + seParentServiceLabel
		labels[ParentServiceNamespaceLabel] = seParentServiceNamespaceLabel
		labels[ParentGatewayLabel] = seParentGatewayLabel
		labels[ParentGatewayNamespaceLabel] = seParentGatewayNamespaceLabel

		// hash the address to ensure uniqueness and valid k8s name when truncating
		addressHash := fmt.Sprintf("%x", sha256.Sum256([]byte(address.String())))[:8]
		weName := fmt.Sprintf("autogen.node.%v.%v.%v", clusterID, uidName, addressHash)
		if len(weName) > 253 {
			weName = weName[:253]
		}

		wantEntries = append(wantEntries, &clientnetworking.WorkloadEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:        weName,
				Namespace:   PeeringNamespace,
				Labels:      labels,
				Annotations: annos,
			},
			Spec: networking.WorkloadEntry{
				Address:        address.String(),
				Network:        clusterID,
				Weight:         1,
				Locality:       locality,
				Ports:          ports,
				ServiceAccount: workload.GetServiceAccount(),
			},
		})
	}

	return c.genericReconcileWorkloadEntries(wantEntries, weGroupLabel)
}

// waypointForService returns name and ns for the waypoint for this service, if any.
// We first check the Service labels, then the Namespace labels
func (c *NetworkWatcher) waypointForService(svc *corev1.Service) (name string, ns string, explicitNone bool) {
	if svc == nil {
		return "", "", false
	}
	name = svc.GetLabels()[label.IoIstioUseWaypoint.Name]
	ns = ptr.NonEmptyOrDefault(svc.GetLabels()[label.IoIstioUseWaypointNamespace.Name], svc.Namespace)
	if name == "" {
		// check the namespace
		namespace := ptr.OrEmpty(c.namespaces.Get(svc.GetNamespace(), ""))
		name = namespace.GetLabels()[label.IoIstioUseWaypoint.Name]
		ns = ptr.NonEmptyOrDefault(namespace.GetLabels()[label.IoIstioUseWaypointNamespace.Name], svc.Namespace)
	}
	if name == "none" {
		return "", "", true
	}
	return name, ns, false
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
		// need to reprocess on annotation changes for service-type
		if !maps.Equal(desired.Annotations, live.Annotations) {
			return true
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
			Message:            fmt.Sprintf("no network started, encountered error validating gateway: %v", err.Error()),
			LastTransitionTime: metav1.Now(),
		}, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeerConnected,
			Status:             metav1.ConditionFalse,
			Reason:             string(k8s.GatewayReasonInvalid),
			Message:            fmt.Sprintf("no network started, encountered error validating gateway: %v", err.Error()),
			LastTransitionTime: metav1.Now(),
		}, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeerDataPlaneProgrammed,
			Status:             metav1.ConditionFalse,
			Reason:             string(k8s.GatewayReasonInvalid),
			Message: fmt.Sprintf(
				"peering data plane not programmed for cross-network traffic, encountered error validating gateway: %v",
				err.Error(),
			),
			LastTransitionTime: metav1.Now(),
		})
		return c.setGatewayStatus(log, gw, gatewayConditions)
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

	if peerCluster.HasSynced() {
		gatewayConditions = append(gatewayConditions, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeeringSucceeded,
			Status:             metav1.ConditionTrue,
			Reason:             string(k8s.GatewayReasonProgrammed),
			LastTransitionTime: metav1.Now(),
		})
	} else {
		gatewayConditions = append(gatewayConditions, metav1.Condition{
			ObservedGeneration: gw.Generation,
			Type:               constants.SoloConditionPeeringSucceeded,
			Status:             metav1.ConditionUnknown,
			Reason:             string(k8s.GatewayReasonPending),
			Message:            "network not yet synced check istiod log for more details",
			LastTransitionTime: metav1.Now(),
		})
	}

	if strings.EqualFold(gw.GetAnnotations()[constants.SoloAnnotationPeeringPreferredDataPlaneServiceType], "nodeport") {
		gwLabels := gw.GetLabels()
		seName := fmt.Sprintf("autogen.peering.%s.%s", gw.GetAnnotations()[constants.GatewayServiceAccountAnnotation], gwLabels[label.TopologyNetwork.Name])
		// check SE exists, then check node WEs exist, then give success status condition
		if se := c.serviceEntries.Get(seName, PeeringNamespace); se != nil {
			labelRequirement, err := klabels.NewRequirement(NodePeerClusterLabel, selection.Equals, []string{gwLabels[label.TopologyCluster.Name]})
			if err != nil {
				log.Errorf("failed to create label requirement: %v", err)
				return err
			}
			if weList := c.workloadEntries.List(PeeringNamespace, klabels.NewSelector().Add(*labelRequirement)); len(weList) > 0 {
				// success
				gatewayConditions = append(gatewayConditions, metav1.Condition{
					ObservedGeneration: gw.Generation,
					Type:               constants.SoloConditionPeerDataPlaneProgrammed,
					Status:             metav1.ConditionTrue,
					Reason:             string(k8s.GatewayReasonProgrammed),
					Message:            "peering data plane programmed for cross-network traffic with NodePort service type",
					LastTransitionTime: metav1.Now(),
				})
			} else {
				// node WEs have not been generated yet
				gatewayConditions = append(gatewayConditions, metav1.Condition{
					ObservedGeneration: gw.Generation,
					Type:               constants.SoloConditionPeerDataPlaneProgrammed,
					Status:             metav1.ConditionFalse,
					Reason:             string(k8s.GatewayReasonPending),
					Message: "peering data plane not programmed for cross-network traffic, " +
						"no node workload entries generated yet for NodePort peering",
				})
			}
		} else {
			// SE has not been generated yet
			gatewayConditions = append(gatewayConditions, metav1.Condition{
				ObservedGeneration: gw.Generation,
				Type:               constants.SoloConditionPeerDataPlaneProgrammed,
				Status:             metav1.ConditionFalse,
				Reason:             string(k8s.GatewayReasonPending),
				Message: "peering data plane not programmed for cross-network traffic, " +
					"gateway-based service entry has not been generated yet for NodePort peering",
			})
		}
	} else {
		// when defaulting to LoadBalancer, verify the gateway has the hbone listener to be confident dataplane is programmed
		hboneListenerExists := false
		for _, listener := range gw.Spec.Listeners {
			if listener.Protocol == "HBONE" {
				hboneListenerExists = true
				break
			}
		}
		if hboneListenerExists {
			gatewayConditions = append(gatewayConditions, metav1.Condition{
				ObservedGeneration: gw.Generation,
				Type:               constants.SoloConditionPeerDataPlaneProgrammed,
				Status:             metav1.ConditionTrue,
				Reason:             string(k8s.GatewayReasonProgrammed),
				Message:            "peering data plane programmed for cross-network traffic with LoadBalancer service type",
				LastTransitionTime: metav1.Now(),
			})
		} else {
			gatewayConditions = append(gatewayConditions, metav1.Condition{
				ObservedGeneration: gw.Generation,
				Type:               constants.SoloConditionPeerDataPlaneProgrammed,
				Status:             metav1.ConditionFalse,
				Reason:             string(k8s.GatewayReasonInvalid),
				Message:            "peering data plane not programmed for cross-network traffic, missing HBONE listener",
				LastTransitionTime: metav1.Now(),
			})
		}
	}
	return c.setGatewayStatus(log, gw, gatewayConditions)
}

func (c *NetworkWatcher) setGatewayStatus(log *istiolog.Scope, gw *gateway.Gateway, conditions []metav1.Condition) error {
	gw = gw.DeepCopy()
	if gw.Status.Conditions == nil {
		gw.Status.Conditions = []metav1.Condition{}
	}
	for _, condition := range conditions {
		meta.SetStatusCondition(&gw.Status.Conditions, condition)
	}

	_, err := c.gateways.UpdateStatus(gw)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		log.Errorf("Encountered unexpected error updating status: %s", err)
	}
	return err
}

func (c *NetworkWatcher) enqueueStatusUpdate(name types.NamespacedName) {
	c.statusQueue.Add(typedNamespace{
		NamespacedName: name,
		kind:           Gateway,
	})
}

// getSegmentDomain returns the domain for the given segment
func (c *NetworkWatcher) getSegmentDomain(segmentName string) (string, error) {
	if segmentName == DefaultSegmentName {
		return DefaultSegment.Spec.Domain, nil
	}

	// Check if it's our local segment first (avoid extra lookup)
	if segmentName == c.GetLocalSegment() {
		return c.GetLocalSegmentDomain(), nil
	}

	segment := c.segments.Get(segmentName, PeeringNamespace)
	if segment == nil {
		return "", fmt.Errorf("segment %s not found", segmentName)
	}
	if segment.Spec.Domain == "" {
		return "", fmt.Errorf("segment %s has no domain configured", segmentName)
	}

	return segment.Spec.Domain, nil
}
