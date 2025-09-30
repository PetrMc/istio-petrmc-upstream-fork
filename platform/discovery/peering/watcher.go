// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "sigs.k8s.io/gateway-api/apis/v1"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/api/label"
	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/backoff"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	kubeconfig "istio.io/istio/pkg/config/kube"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/env"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/maps"
	networkid "istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/sleep"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/workloadapi"
)

var log = istiolog.RegisterScope("peering", "")

// NetworkWatcher is currently a misnomer; it's actually a ClusterWatcher.
type NetworkWatcher struct {
	client kube.Client

	namespaces                kclient.Client[*corev1.Namespace]
	gateways                  kclient.Client[*gateway.Gateway]
	workloadEntries           kclient.Client[*clientnetworking.WorkloadEntry]
	workloadEntryServiceIndex kclient.Index[types.NamespacedName, *clientnetworking.WorkloadEntry]
	serviceEntries            kclient.Client[*clientnetworking.ServiceEntry]
	services                  kclient.Client[*corev1.Service]
	sd                        model.ServiceDiscovery
	remoteWaypointSync        krt.Syncer

	gatewaysProcessed bool

	queue       controllers.Queue
	statusQueue controllers.Queue

	buildConfig func(clientName string) *adsc.DeltaADSConfig

	meshConfigWatcher mesh.Watcher

	clustersMu sync.RWMutex
	// cluster name -> *network
	remoteClusters map[cluster.ID]*peerCluster
	// gateway -> cluster name
	gatewaysToCluster map[types.NamespacedName]cluster.ID

	// our local cluster
	localCluster cluster.ID
	localNetwork networkid.ID

	clusterDomain   string
	systemNamespace string

	debugger *krt.DebugHandler
}

func New(
	client kube.Client,
	systemNamespace string,
	clusterID cluster.ID,
	clusterDomain string,
	buildConfig func(clientName string) *adsc.DeltaADSConfig,
	debugger *krt.DebugHandler,
	meshConfigWatcher mesh.Watcher,
	sd model.ServiceDiscovery,
	statusManager *status.Manager,
) *NetworkWatcher {
	c := &NetworkWatcher{
		remoteClusters:    make(map[cluster.ID]*peerCluster),
		gatewaysToCluster: make(map[types.NamespacedName]cluster.ID),
		systemNamespace:   systemNamespace,
		buildConfig:       buildConfig,
		client:            client,
		clusterDomain:     clusterDomain,
		localCluster:      clusterID,
		debugger:          debugger,
		meshConfigWatcher: meshConfigWatcher,
		sd:                sd,
	}
	c.gateways = kclient.New[*gateway.Gateway](client)
	c.serviceEntries = kclient.New[*clientnetworking.ServiceEntry](client)
	c.workloadEntries = kclient.New[*clientnetworking.WorkloadEntry](client)
	c.workloadEntryServiceIndex = kclient.CreateIndex[types.NamespacedName](c.workloadEntries, "peering-watcher",
		func(o *clientnetworking.WorkloadEntry) []types.NamespacedName {
			if o.Namespace != PeeringNamespace {
				return nil
			}
			svc, f := o.Labels[ParentServiceLabel]
			if !f {
				return nil
			}
			ns, f := o.Labels[ParentServiceNamespaceLabel]
			if !f {
				return nil
			}
			return []types.NamespacedName{{
				Namespace: ns,
				Name:      svc,
			}}
		})
	c.services = kclient.New[*corev1.Service](client)
	c.queue = controllers.NewQueue("peering",
		controllers.WithGenericReconciler(c.Reconcile),
		controllers.WithMaxAttempts(25))
	c.statusQueue = controllers.NewQueue("peering-status",
		controllers.WithGenericReconciler(c.ReconcileStatus),
		controllers.WithMaxAttempts(25))

	c.gateways.AddEventHandler(controllers.ObjectHandler(func(o controllers.Object) {
		c.queue.Add(typedNamespace{
			NamespacedName: config.NamespacedName(o),
			kind:           Gateway,
		})
	}))

	// update ServiceEntry and associated WorkloadEntry when a service changes
	// triggered by local Service object changes and federatedWaypoints changes
	commonServiceHandler := func(name types.NamespacedName) {
		c.queue.Add(typedNamespace{
			NamespacedName: name,
			kind:           ServiceEntry,
		})
		for _, remoteService := range c.workloadEntryServiceIndex.Lookup(name) {
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{
					Namespace: remoteService.Labels[SourceClusterLabel],
					Name:      remoteService.Labels[ParentServiceNamespaceLabel] + "/" + remoteService.Labels[ParentServiceLabel],
				},
				kind: WorkloadEntry,
			})
		}

		// new: use the federatedServices collections from each cluster to enqueue
		// this handles the case where we haven't created the WE yet
		c.clustersMu.RLock()
		defer c.clustersMu.RUnlock()
		for clusterID, pc := range c.remoteClusters {
			// Check if this remote cluster has a federated service for this service
			fsKey := fmt.Sprintf("%s/%s", name.Namespace, name.Name)
			if pc.federatedServices.GetKey(fsKey) != nil {
				c.queue.Add(typedNamespace{
					NamespacedName: types.NamespacedName{
						Namespace: string(clusterID),
						Name:      fsKey,
					},
					kind: WorkloadEntry,
				})
			}
		}
	}

	if federatedWaypoints := sd.FederatedWaypoints(); federatedWaypoints != nil {
		c.remoteWaypointSync = federatedWaypoints.RegisterBatch(func(events []krt.Event[krt.Named]) {
			for _, e := range events {
				o := e.Latest()
				commonServiceHandler(types.NamespacedName{
					Name:      o.Name,
					Namespace: o.Namespace,
				})
			}
		}, true)
	} else {
		// should never happen; can only happpen if we init peering after ambientindexes
		log.Error("failed to watch FederatedWaypoints from the local cluster")
	}

	// this namespace event handler reprocesses all services in a namespace if there is an
	// update event. this allows handling service-scope namespace label changes
	nsChanged := func(o controllers.Object) {
		name := o.GetName()
		services := c.services.List(name, klabels.Everything())
		for _, service := range services {
			// common services handler locks so we don't lock here
			commonServiceHandler(config.NamespacedName(service))
		}
	}
	c.namespaces = kclient.NewFiltered[*corev1.Namespace](client, kclient.Filter{})
	c.namespaces.AddEventHandler(controllers.EventHandler[controllers.Object]{
		UpdateFunc: func(oldObj, newObj controllers.Object) {
			// TO-DO: should we do some label comparison here
			// in the future to only reprocess if we absolutely
			// have to?

			// arbitrarily using the oldobj here since all we
			// from it is the name
			nsChanged(oldObj)
		},
	})

	c.services.AddEventHandler(controllers.FilteredObjectHandler(func(o controllers.Object) {
		name := config.NamespacedName(o)
		commonServiceHandler(name)
	}, func(o controllers.Object) bool {
		ns := ptr.OrEmpty[corev1.Namespace](kclient.New[*corev1.Namespace](client).Get(o.GetNamespace(), ""))
		return IsGlobal(CalculateScope(o.GetLabels(), ns.GetLabels()))
	}))
	c.workloadEntries.AddEventHandler(controllers.FilteredObjectHandler(func(o controllers.Object) {
		svc := o.GetLabels()[ParentServiceLabel]
		ns := o.GetLabels()[ParentServiceNamespaceLabel]

		c.queue.Add(typedNamespace{
			NamespacedName: types.NamespacedName{Name: svc, Namespace: ns},
			kind:           ServiceEntry,
		})
	}, func(o controllers.Object) bool {
		return o.GetLabels()[ParentServiceLabel] != "" &&
			o.GetLabels()[ParentServiceNamespaceLabel] != "" &&
			o.GetLabels()[SourceClusterLabel] != ""
	}))

	// Changes to any autogenerated resource should reconcile themselves to ensure we recover
	// from external changes
	workloadEntryChangedHandler := func(o controllers.Object) {
		if strings.HasPrefix(o.GetName(), "autogen.") {
			svc := o.GetLabels()[ParentServiceLabel]
			ns := o.GetLabels()[ParentServiceNamespaceLabel]
			cluster := o.GetLabels()[SourceClusterLabel]
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{
					Namespace: cluster,
					Name:      ns + "/" + svc,
				},
				kind: WorkloadEntry,
			})
			return
		}
		if strings.HasPrefix(o.GetName(), "autogenflat.") {
			cluster := o.GetLabels()[SourceClusterLabel]
			uid := o.GetAnnotations()[PeeredWorkloadUIDAnnotation]
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{
					Namespace: cluster,
					Name:      uid,
				},
				kind: FlatWorkloadEntry,
			})
			return
		}
	}
	c.workloadEntries.AddEventHandler(controllers.EventHandler[controllers.Object]{
		UpdateFunc: func(oldObj, newObj controllers.Object) {
			// use oldObj just in case the labels were modified
			workloadEntryChangedHandler(oldObj)
		},
		DeleteFunc: workloadEntryChangedHandler,
	})
	serviceChangedHandler := func(o controllers.Object) {
		name := o.GetLabels()[ParentServiceLabel]
		ns := o.GetLabels()[ParentServiceNamespaceLabel]
		if name != "" && ns != "" {
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
				kind:           ServiceEntry,
			})
		}
	}
	c.serviceEntries.AddEventHandler(controllers.EventHandler[controllers.Object]{
		UpdateFunc: func(oldObj, newObj controllers.Object) {
			// use oldObj just in case the labels were modified
			serviceChangedHandler(oldObj)
		},
		DeleteFunc: serviceChangedHandler,
	})

	return c
}

// isGlobalWaypoint checks whether a Service should be treated
// as implicitly global, because it is a waypoint for global services.
// Anything that calls this isGlobalWaypoint func must also be enqueued
// by FederatedWaypoints events.
func (c *NetworkWatcher) isGlobalWaypoint(o controllers.Object) bool {
	fw := c.sd.FederatedWaypoints()
	if fw == nil {
		// should never happen
		log.Error("failed to check FederatedWaypoints")
		return false
	}
	return fw.GetKey(o.GetNamespace()+string(types.Separator)+o.GetName()) != nil
}

type cachesSyncedMarker struct{}

func (c *NetworkWatcher) Run(stop <-chan struct{}) {
	kube.WaitForCacheSync(
		"peering",
		stop,
		c.gateways.HasSynced,
		c.serviceEntries.HasSynced,
		c.workloadEntries.HasSynced,
		c.services.HasSynced,
		c.remoteWaypointSync.HasSynced,
		c.namespaces.HasSynced,
	)
	c.queue.Add(cachesSyncedMarker{})

	c.localNetwork = tryFetchLocalNetworkForever(c.client, c.systemNamespace, stop)
	c.pruneRemovedGateways()
	log.Infof("informers synced, starting processing")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.queue.Run(stop)
	}()
	go func() {
		defer wg.Done()
		c.statusQueue.Run(stop)
	}()
	wg.Wait()
	// Wait for the queue to finish draining (needed to ensure we don't hit a race when we trigger shutdowns later)
	<-c.queue.Closed()
	c.clustersMu.Lock()
	defer c.clustersMu.Unlock()
	log.Infof("shutting down")
	controllers.ShutdownAll(c.gateways, c.serviceEntries, c.workloadEntries, c.services)
	for _, n := range c.remoteClusters {
		n.shutdownNow()
	}
	<-c.statusQueue.Closed()
}

// TODO watch the namespace and re-trigger everything to support network change without istiod restart
func tryFetchLocalNetworkForever(client kube.Client, systemNamespace string, stop <-chan struct{}) networkid.ID {
	bo := backoff.NewExponentialBackOff(backoff.DefaultOption())
	for {
		nextSleep := bo.NextBackOff()
		ns, err := client.Kube().CoreV1().Namespaces().Get(context.Background(), systemNamespace, metav1.GetOptions{})
		if err != nil {
			log.Errorf("failed to fetch namespace %s, will retry in %v: %v", systemNamespace, nextSleep, err)
		} else {
			if net, f := ns.Labels[label.TopologyNetwork.Name]; f {
				return networkid.ID(net)
			}
			log.Errorf("fetched namespace %s but 'topology.istio.io/network' is not set; will retry in %v", systemNamespace, nextSleep)
		}

		if !sleep.Until(stop, nextSleep) {
			return ""
		}
	}
}

var sourceClusterWorkloadEntries = func() klabels.Selector {
	l, _ := klabels.Parse(SourceClusterLabel)
	return l
}()

var networkGatewaysSelector = func() klabels.Selector {
	l, _ := klabels.Parse(label.TopologyNetwork.Name)
	return l
}()

func (c *NetworkWatcher) pruneRemovedGateways() {
	clustersInWorkloadEntries := sets.New[string]()
	currentWorkloadEntries := c.workloadEntries.List(metav1.NamespaceAll, sourceClusterWorkloadEntries)
	for _, we := range currentWorkloadEntries {
		clustersInWorkloadEntries.Insert(we.Labels[SourceClusterLabel])
	}
	clustersInGateways := sets.New[string]()
	for _, gw := range c.gateways.List(metav1.NamespaceAll, networkGatewaysSelector) {
		p, err := TranslatePeerGateway(gw)
		if err != nil {
			continue
		}
		if clustersInGateways.InsertContains(p.Cluster.String()) {
			log.Warnf("multiple gateways for cluster %s", p.Cluster)
		}
	}
	staleClusters := clustersInWorkloadEntries.Difference(clustersInGateways)
	for stale := range staleClusters {
		log.Infof("found stale network %v", stale)
		c.queue.Add(typedNamespace{
			NamespacedName: types.NamespacedName{Name: stale},
			kind:           Cluster,
		})
	}
}

func mergeRemotePorts(remoteServices []*clientnetworking.WorkloadEntry) []*networking.ServicePort {
	pmap := map[uint32]uint32{}
	protocolMap := map[uint32]string{}

	for _, remote := range remoteServices {
		if protocolsStr, ok := remote.Annotations[ServiceProtocolsAnnotation]; ok && protocolsStr != "" {
			protocols := deserializeProtocolsByPort(protocolsStr)
			for port, protocol := range protocols {
				if existing, ok := protocolMap[port]; ok && existing != protocol {
					// Protocol conflict detected, force TCP
					protocolMap[port] = "TCP"
					log.Warnf("Protocol conflict from WE annotation for port %d, forcing TCP", port)
				} else {
					protocolMap[port] = protocol
				}
			}
		}
	}

	// Merge all remote service port number mappings based on the generated WorkloadEntries
	for _, remote := range remoteServices {
		for _, t := range remote.Spec.Ports {
			pmap[t] = t
		}
	}

	res := []*networking.ServicePort{}
	for k, v := range pmap {
		protocol := protocolMap[k]
		if protocol == "" {
			protocol = "TCP" // Default to TCP if no protocol is specified
		}
		res = append(res, &networking.ServicePort{
			Number:     k,
			Protocol:   protocol,
			Name:       fmt.Sprintf("port-%d", k),
			TargetPort: v,
		})
	}
	slices.SortBy(res, func(a *networking.ServicePort) uint32 {
		return a.Number
	})
	return res
}

func convertPorts(ports []corev1.ServicePort) []*networking.ServicePort {
	return slices.MapFilter(ports, func(e corev1.ServicePort) **networking.ServicePort {
		if e.Protocol != corev1.ProtocolTCP {
			return nil
		}
		name := e.Name
		if e.TargetPort.Type == intstr.String {
			name = e.TargetPort.StrVal
		}
		p := kubeconfig.ConvertProtocol(e.Port, e.Name, e.Protocol, e.AppProtocol)
		proto := p.String()
		if p.IsUnsupported() {
			proto = ""
		}
		return ptr.Of(&networking.ServicePort{
			Number:     uint32(e.Port),
			Protocol:   proto,
			Name:       name,
			TargetPort: uint32(e.TargetPort.IntVal),
		})
	})
}

type typedNamespace struct {
	types.NamespacedName
	kind Kind
}

const (
	// RemoteWaypointLabel if set indicates there is a waypoint for the service (even if that's opaque to us)
	// due to that waypoint only being on a remote cluster. We use it to make sidecars defer processing.
	RemoteWaypointLabel = "solo.io/remote-waypoint"

	// UseGlobalWaypointLabel indicates that this generated ServiceEntry can have its
	// Waypoint attachment overidden to point to a global waypoint that is peered like other global services.
	// This should only be set when the service exists in at least one cluster on the same network as the local cluster.
	UseGlobalWaypointLabel = "solo.io/use-global-waypoint"

	ServiceScopeLabel      = "solo.io/service-scope"
	ServiceScopeGlobal     = "global"
	ServiceScopeGlobalOnly = "global-only"
	ServiceScopeCluster    = "cluster"

	ServiceSubjectAltNamesAnnotation = "solo.io/subject-alt-names"
	ParentServiceLabel               = "solo.io/parent-service"
	ParentServiceNamespaceLabel      = "solo.io/parent-service-namespace"
	SourceClusterLabel               = "solo.io/source-cluster"
	SourceWorkloadLabel              = "solo.io/source-workload"
	PeeredWorkloadUIDAnnotation      = "solo.io/peered-workload-uid"

	// ServiceEndpointStatus is a workaround WorkloadEntry not being able to encode "0 weight" or "unhealthy".
	// This lets us
	ServiceEndpointStatus          = "solo.io/endpoint-status"
	ServiceEndpointStatusUnhealthy = "unheathy"

	// ServiceProtocolsAnnotation stores the port-to-protocol mapping from FederatedService
	// Format: "80:HTTP,443:HTTPS,9090:GRPC"
	ServiceProtocolsAnnotation = "solo.io/service-protocols"
)

// serializeProtocolsByPort converts a map of port->protocol to annotation string format
func serializeProtocolsByPort(protocolsByPort map[uint32]string) string {
	if len(protocolsByPort) == 0 {
		return ""
	}
	var protocolPairs []string
	for port, protocol := range protocolsByPort {
		protocolPairs = append(protocolPairs, fmt.Sprintf("%d:%s", port, protocol))
	}
	return strings.Join(slices.Sort(protocolPairs), ",")
}

// deserializeProtocolsByPort parses annotation string format to map of port->protocol
func deserializeProtocolsByPort(annotation string) map[uint32]string {
	if annotation == "" {
		return nil
	}
	result := make(map[uint32]string)
	pairs := strings.Split(annotation, ",")
	for _, pair := range pairs {
		parts := strings.Split(pair, ":")
		if len(parts) == 2 {
			port, err := strconv.ParseUint(parts[0], 10, 32)
			if err == nil {
				result[uint32(port)] = parts[1]
			}
		}
	}
	return result
}

func HasGlobalLabel(labels map[string]string) bool {
	v := labels[ServiceScopeLabel]
	return IsGlobal(v)
}

func CalculateScope(svcl, nsl map[string]string) string {
	if val, ok := svcl[ServiceScopeLabel]; ok {
		return val
	}

	if val, ok := nsl[ServiceScopeLabel]; ok {
		return val
	}

	return ServiceScopeCluster
}

func IsGlobal(scope string) bool {
	return scope == ServiceScopeGlobal || scope == ServiceScopeGlobalOnly
}

func ConvertScope(scope string) workloadapi.ServiceScope {
	if scope == ServiceScopeGlobalOnly {
		return workloadapi.ServiceScope_GLOBAL_ONLY
	}
	return workloadapi.ServiceScope_GLOBAL
}

func ConvertScopeFromWorkloadAPI(scope workloadapi.ServiceScope) string {
	if scope == workloadapi.ServiceScope_GLOBAL_ONLY {
		return ServiceScopeGlobalOnly
	}
	return ServiceScopeGlobal
}

var (
	defaultNamespace = func() string {
		// Like  bootstrap.PodNamespace but avoiding circular dep
		if ns, f := os.LookupEnv("POD_NAMESPACE"); f {
			return ns
		}
		return constants.IstioSystemNamespace
	}()
	PeeringNamespace = env.Register("PEERING_DISCOVERY_NAMESPACE", defaultNamespace,
		"The namespace to write peering resources into.").Get()
	EnableAutomaticGatewayCreation = env.Register("PEERING_AUTOMATIC_LOCAL_GATEWAY", false,
		"If enabled, a local 'istio-remote' Gateway will be created from any 'istio-eastwest' Gateways. This facilitates copying to other clusters.").Get()
	DomainSuffix = "." + env.Register("PEERING_DISCOVERY_SUFFIX", "mesh.internal",
		"The domain suffix for generate services").Get()
	DefaultTrafficDistribution = env.Register("PEERING_DISCOVERY_DEFAULT_TRAFFIC_DISTRIBUTION", "PreferNetwork",
		"The default trafficDistribution for services, if none set.").Get()
	EnableFlatNetworks = env.Register("PEERING_ENABLE_FLAT_NETWORKS", true,
		"If enabled, clusters that have the same network name will be reached directly, skipping the gateway.").Get()
)

// IsPeerObject checks if an object is a peer object which should be logically considered a part of another namespace.
func IsPeerObject(c *config.Config) bool {
	if c.Namespace != PeeringNamespace {
		return false
	}
	if _, f := c.Labels[ParentServiceLabel]; !f {
		return false
	}
	if _, f := c.Labels[ParentServiceNamespaceLabel]; !f {
		return false
	}
	return true
}

// WasPeerObject checks if an object *was* a peer object, post-translation. This is *not* verified information, since
// anyone could put these labels on an object.
func WasPeerObject(c *config.Config) bool {
	if _, f := c.Labels[ParentServiceLabel]; !f {
		return false
	}
	if _, f := c.Labels[ParentServiceNamespaceLabel]; !f {
		return false
	}
	return true
}

type RemoteFederatedService struct {
	Service *workloadapi.FederatedService
	Cluster string
}

func (s RemoteFederatedService) ResourceName() string {
	return s.Cluster + "/" + s.Service.Namespace + "/" + s.Service.Name
}

func (s RemoteFederatedService) Equals(other RemoteFederatedService) bool {
	return s.Cluster == other.Cluster && proto.Equal(s.Service, other.Service)
}

type RemoteWorkload struct {
	*workloadapi.Workload
	Cluster string
}

func (s RemoteWorkload) ResourceName() string {
	return s.Uid
}

func (s RemoteWorkload) Equals(other RemoteWorkload) bool {
	return s.Cluster == other.Cluster && proto.Equal(s.Workload, other.Workload)
}

// CreateOrUpdateIfNeeded will create an object if does not exist. If it does exist, it will update it if there are changes.
func CreateOrUpdateIfNeeded[T controllers.ComparableObject](c kclient.ReadWriter[T], object T, equal func(desired T, live T) bool) (bool, error) {
	res := c.Get(object.GetName(), object.GetNamespace())
	if controllers.IsNil(res) {
		_, err := c.Create(object)
		return true, err
	}
	if equal(res, object) {
		return false, nil
	}
	object.SetResourceVersion(res.GetResourceVersion())
	// Already exist, update
	_, err := c.Update(object)
	return true, err
}

type PeerGateway struct {
	Network  networkid.ID
	Cluster  cluster.ID
	Address  string
	Locality string
}

func TranslatePeerGateway(gw *gateway.Gateway) (PeerGateway, error) {
	peer := PeerGateway{}
	if gw == nil {
		return peer, fmt.Errorf("gateway was deleted")
	}
	err := ValidatePeerGateway(*gw)
	if err != nil {
		return peer, err
	}
	peer.Network = networkid.ID(gw.Labels[label.TopologyNetwork.Name])
	if peer.Network == "" {
		return peer, fmt.Errorf("no network label found in gateway")
	}
	peer.Cluster = cluster.ID(gw.Labels[label.TopologyCluster.Name])
	if peer.Cluster == "" {
		log.Debugf(
			"no %s label on peer gateway, infer cluster name from network %s",
			label.TopologyCluster.Name, peer.Network,
		)
		// Fallback to cluster == network
		peer.Cluster = cluster.ID(peer.Network)
	}
	if len(gw.Spec.Addresses) == 0 {
		if len(gw.Status.Addresses) > 0 {
			// We could support this, but right now it's an error and doesn't have a use case
			return peer, fmt.Errorf("no spec.addresses found in gateway (but found status.addresses)")
		}
		return peer, fmt.Errorf("no addresses found in gateway")
	}
	// We can use hostname or IP, so don't need to check the type
	host := gw.Spec.Addresses[0].Value
	// Default
	port := "15012"
	for _, l := range gw.Spec.Listeners {
		if l.Name == "xds-tls" {
			port = fmt.Sprint(l.Port)
			break
		}
	}
	peer.Address = net.JoinHostPort(host, port)

	region, rf := gw.Labels["topology.kubernetes.io/region"]
	zone, zf := gw.Labels["topology.kubernetes.io/zone"]
	if rf && zf {
		peer.Locality = region + "/" + zone
	} else if rf {
		peer.Locality = region
	}
	return peer, nil
}

func ValidatePeerGateway(gw gateway.Gateway) error {
	if gw.Spec.GatewayClassName != "istio-remote" {
		return fmt.Errorf("gateway is not an istio-remote gateway (was %q)", gw.Spec.GatewayClassName)
	}
	if gw.Labels["topology.istio.io/network"] == "" {
		return fmt.Errorf("no network label found in gateway")
	}
	if len(gw.Spec.Addresses) == 0 {
		if len(gw.Status.Addresses) > 0 {
			// We could support this, but right now it's an error and doesn't have a use case
			return fmt.Errorf("no spec.addresses found in gateway (but found status.addresses)")
		}
		return fmt.Errorf("no addresses found in gateway")
	}
	return nil
}

// TranslateEastWestGateway translates an east-west gateway with the `istio.io/expose-istiod` label
// into an `istio-remote` gateway using the gateway address in the status. This `istio-remote` gateway
// is intended to be distributed to remote clusters using GMEs agent-mgmt relay connection. No network
// will be created for the gateway as it points to the local cluster network.
func TranslateEastWestGateway(gw *gateway.Gateway, trustDomain string) (*gateway.Gateway, error) {
	if _, f := gw.Labels[constants.ExposeIstiodLabel]; !f {
		return nil, fmt.Errorf("gateway does not have the %q label", constants.ExposeIstiodLabel)
	}
	istioCluster, f := gw.Labels[label.TopologyCluster.Name]
	if !f {
		return nil, fmt.Errorf("gateway does not have the %q label", label.TopologyCluster.Name)
	}
	_, f = gw.Labels[label.TopologyNetwork.Name]
	if !f {
		return nil, fmt.Errorf("gateway does not have the %q label", label.TopologyNetwork.Name)
	}
	// get cross-network and xds-tls ports
	var crossNetworkPort, xdsTLSPort *gateway.PortNumber
	for _, l := range gw.Spec.Listeners {
		if l.Name == "cross-network" {
			crossNetworkPort = &l.Port
		}
		if l.Name == "xds-tls" {
			xdsTLSPort = &l.Port
		}
	}
	if crossNetworkPort == nil {
		return nil, fmt.Errorf("no cross-network listener found in istio-eastwest gateway %s.%s", gw.Name, gw.Namespace)
	}
	if xdsTLSPort == nil {
		return nil, fmt.Errorf("no xds-tls listener found in istio-eastwest gateway %s.%s", gw.Name, gw.Namespace)
	}
	if len(gw.Status.Addresses) == 0 {
		return nil, fmt.Errorf("no addresses found in istio-eastwest gateway %s.%s", gw.Name, gw.Namespace)
	}
	var gwAddresses []gateway.GatewaySpecAddress
	for _, addr := range gw.Status.Addresses {
		gwAddresses = append(gwAddresses, gateway.GatewaySpecAddress(addr))
	}
	annotations := make(map[string]string)
	if gw.GetAnnotations() != nil {
		annotations = maps.Clone(gw.GetAnnotations())
	}
	annotations[constants.TrustDomainAnnotation] = trustDomain
	annotations[constants.GatewayServiceAccountAnnotation] = gw.GetName()
	return &gateway.Gateway{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.KubernetesGateway_v1.Kind,
			APIVersion: gvk.KubernetesGateway.GroupVersion(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("istio-remote-peer-%s", istioCluster),
			Namespace:   gw.GetNamespace(),
			Annotations: annotations,
			Labels:      gw.GetLabels(),
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       gvk.KubernetesGateway_v1.Kind,
					APIVersion: gvk.KubernetesGateway_v1.GroupVersion(),
					Name:       gw.GetName(),
					UID:        gw.GetUID(),
				},
			},
		},
		Spec: gateway.GatewaySpec{
			GatewayClassName: constants.RemoteGatewayClassName,
			Addresses:        gwAddresses,
			Listeners: []gateway.Listener{
				{
					Name:     "cross-network",
					Port:     *crossNetworkPort,
					Protocol: "HBONE",
					TLS:      &gateway.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
				{
					Name:     "xds-tls",
					Port:     *xdsTLSPort,
					Protocol: k8s.TLSProtocolType,
					TLS:      &gateway.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
			},
		},
	}, nil
}

func (c *NetworkWatcher) getClusterByID(cid cluster.ID) *peerCluster {
	c.clustersMu.RLock()
	defer c.clustersMu.RUnlock()
	cluster, ok := c.remoteClusters[cid]
	if !ok {
		return nil
	}
	return cluster
}

func (c *NetworkWatcher) getClusterByGateway(gw types.NamespacedName) *peerCluster {
	c.clustersMu.RLock()
	defer c.clustersMu.RUnlock()
	cid, ok := c.gatewaysToCluster[gw]
	if !ok {
		return nil
	}
	cluster, ok := c.remoteClusters[cid]
	if !ok {
		return nil
	}
	return cluster
}
