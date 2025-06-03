// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering

import (
	"fmt"
	"net"
	"os"
	"sync"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "sigs.k8s.io/gateway-api/apis/v1"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"

	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	kubeconfig "istio.io/istio/pkg/config/kube"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/env"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/workloadapi"
)

var log = istiolog.RegisterScope("peering", "")

type NamedNetwork struct {
	networkName string
	network     *network
}

func (n *NamedNetwork) ShutdownNow() {
	if n.network != nil {
		n.network.shutdownNow()
	}
}

type NetworkWatcher struct {
	client kube.Client

	gateways                  kclient.Client[*gateway.Gateway]
	workloadEntries           kclient.Client[*clientnetworking.WorkloadEntry]
	workloadEntryServiceIndex kclient.Index[types.NamespacedName, *clientnetworking.WorkloadEntry]
	serviceEntries            kclient.Client[*clientnetworking.ServiceEntry]
	services                  kclient.Client[*corev1.Service]

	queue       controllers.Queue
	statusQueue controllers.Queue

	statusController *status.Controller

	buildConfig func(clientName string) *adsc.DeltaADSConfig

	meshConfigWatcher mesh.Watcher

	// gateway -> network
	gatewaysToNetwork map[types.NamespacedName]*NamedNetwork
	clusterDomain     string
	clusterID         string

	debugger *krt.DebugHandler
}

func New(
	client kube.Client,
	clusterID string,
	clusterDomain string,
	buildConfig func(clientName string) *adsc.DeltaADSConfig,
	debugger *krt.DebugHandler,
	meshConfigWatcher mesh.Watcher,
	statusManager *status.Manager,
) *NetworkWatcher {
	statusCtl := statusManager.CreateGenericController(func(status status.Manipulator, context any) {
		status.SetInner(context)
	})
	c := &NetworkWatcher{
		gatewaysToNetwork: make(map[types.NamespacedName]*NamedNetwork),
		buildConfig:       buildConfig,
		client:            client,
		clusterDomain:     clusterDomain,
		clusterID:         clusterID,
		debugger:          debugger,
		meshConfigWatcher: meshConfigWatcher,
		statusController:  statusCtl,
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
			kind:           kind.Gateway,
		})
	}))

	c.services.AddEventHandler(controllers.FilteredObjectHandler(func(o controllers.Object) {
		name := config.NamespacedName(o)
		c.queue.Add(typedNamespace{
			NamespacedName: name,
			kind:           kind.ServiceEntry,
		})
		for _, remoteService := range c.workloadEntryServiceIndex.Lookup(name) {
			c.queue.Add(typedNamespace{
				NamespacedName: types.NamespacedName{
					Namespace: remoteService.Labels[SourceClusterLabel],
					Name:      remoteService.Labels[ParentServiceNamespaceLabel] + "/" + remoteService.Labels[ParentServiceLabel],
				},
				kind: kind.WorkloadEntry,
			})
		}
	}, func(o controllers.Object) bool {
		return HasGlobalLabel(o.GetLabels())
	}))
	c.workloadEntries.AddEventHandler(controllers.FilteredObjectHandler(func(o controllers.Object) {
		svc := o.GetLabels()[ParentServiceLabel]
		ns := o.GetLabels()[ParentServiceNamespaceLabel]

		c.queue.Add(typedNamespace{
			NamespacedName: types.NamespacedName{Name: svc, Namespace: ns},
			kind:           kind.ServiceEntry,
		})
	}, func(o controllers.Object) bool {
		return o.GetLabels()[ParentServiceLabel] != "" &&
			o.GetLabels()[ParentServiceNamespaceLabel] != "" &&
			o.GetLabels()[SourceClusterLabel] != ""
	}))
	return c
}

func (c *NetworkWatcher) Run(stop <-chan struct{}) {
	kube.WaitForCacheSync(
		"peering",
		stop,
		c.gateways.HasSynced,
		c.serviceEntries.HasSynced,
		c.workloadEntries.HasSynced,
		c.services.HasSynced,
	)
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
	log.Infof("shutting down")
	controllers.ShutdownAll(c.gateways, c.serviceEntries, c.workloadEntries, c.services)
	for _, namedNet := range c.gatewaysToNetwork {
		namedNet.ShutdownNow()
	}
	<-c.statusQueue.Closed()
}

var sourceClusterWorkloadEntries = func() klabels.Selector {
	l, _ := klabels.Parse(SourceClusterLabel)
	return l
}()

var networkGatewaysSelector = func() klabels.Selector {
	l, _ := klabels.Parse("topology.istio.io/network")
	return l
}()

func (c *NetworkWatcher) pruneRemovedGateways() {
	networksInWorkloadEntries := sets.New[string]()
	currentWorkloadEntries := c.workloadEntries.List(metav1.NamespaceAll, sourceClusterWorkloadEntries)
	for _, we := range currentWorkloadEntries {
		networksInWorkloadEntries.Insert(we.Labels[SourceClusterLabel])
	}
	networksInGateways := sets.New[string]()
	for _, gw := range c.gateways.List(metav1.NamespaceAll, networkGatewaysSelector) {
		p, err := TranslatePeerGateway(gw)
		if err != nil {
			continue
		}
		networksInGateways.Insert(p.Network)
	}
	staleNetworks := networksInWorkloadEntries.Difference(networksInGateways)
	for stale := range staleNetworks {
		log.Infof("found stale network %v", stale)
		c.queue.Add(typedNamespace{
			NamespacedName: types.NamespacedName{Name: stale},
			kind:           kind.MeshNetworks,
		})
	}
}

func mergeRemotePorts(existing []*networking.ServicePort, incoming map[string]uint32) []*networking.ServicePort {
	pmap := map[uint32]uint32{}
	for _, t := range existing {
		pmap[t.Number] = t.TargetPort
	}
	// New one arbitrarily takes precedence
	for _, t := range incoming {
		pmap[t] = t
	}
	res := []*networking.ServicePort{}
	for k, v := range pmap {
		res = append(res, &networking.ServicePort{
			Number:     k,
			Protocol:   "TCP", // TODO? For L4 only it doesn't matter, but likely we will need to express the protocol on the wire.
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
		if e.TargetPort.Type == intstr.String {
			// TODO: report some status
			return nil
		}
		p := kubeconfig.ConvertProtocol(e.Port, e.Name, e.Protocol, e.AppProtocol)
		proto := p.String()
		if p.IsUnsupported() {
			proto = ""
		}
		return ptr.Of(&networking.ServicePort{
			Number:     uint32(e.Port),
			Protocol:   proto,
			Name:       e.Name,
			TargetPort: uint32(e.TargetPort.IntVal),
		})
	})
}

type typedNamespace struct {
	types.NamespacedName
	kind kind.Kind
}

const (
	RemoteWaypointLabel = "solo.io/remote-waypoint"

	ServiceScopeLabel      = "solo.io/service-scope"
	ServiceScopeGlobal     = "global"
	ServiceScopeGlobalOnly = "global-only"

	ServiceSubjectAltNamesAnnotation = "solo.io/subject-alt-names"
	ParentServiceLabel               = "solo.io/parent-service"
	ParentServiceNamespaceLabel      = "solo.io/parent-service-namespace"
	SourceClusterLabel               = "solo.io/source-cluster"

	// ServiceEndpointStatus is a workaround WorkloadEntry not being able to encode "0 weight" or "unhealthy".
	// This lets us
	ServiceEndpointStatus          = "solo.io/endpoint-status"
	ServiceEndpointStatusUnhealthy = "unheathy"
)

func HasGlobalLabel(labels map[string]string) bool {
	v := labels[ServiceScopeLabel]
	return v == ServiceScopeGlobal || v == ServiceScopeGlobalOnly
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
	*workloadapi.FederatedService
	Cluster string
}

func (s RemoteFederatedService) ResourceName() string {
	return s.Cluster + "/" + s.Namespace + "/" + s.Name
}

func (s RemoteFederatedService) Equals(other RemoteFederatedService) bool {
	return s.Cluster == other.Cluster && proto.Equal(s.FederatedService, other.FederatedService)
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
	Network string
	Address string
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
	peer.Network = gw.Labels["topology.istio.io/network"]
	if peer.Network == "" {
		return peer, fmt.Errorf("no network label found in gateway")
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
	istioNetwork, f := gw.Labels[constants.NetworkTopologyLabel]
	if !f {
		return nil, fmt.Errorf("gateway does not have the %q label", constants.NetworkTopologyLabel)
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
			Name:        fmt.Sprintf("istio-remote-peer-%s", istioNetwork),
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
