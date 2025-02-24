// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package autowaypoint

import (
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gatewayapi "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/api/label"
	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/gvr"
	kubelib "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	istiolog "istio.io/istio/pkg/log"
)

var log = istiolog.RegisterScope("auto waypoint", "Solo.io auto waypoint controller")

var matchNamespaceLabels = labels.SelectorFromSet(map[string]string{
	label.IoIstioUseWaypoint.Name: "auto",
})

func requestsAutoWaypoint(l labels.Set) bool {
	if _, xNamespace := l[label.IoIstioUseWaypointNamespace.Name]; xNamespace {
		return false
	}
	return matchNamespaceLabels.Matches(l)
}

// AutoWaypoint manages automatic waypoint creation
type AutoWaypoint struct {
	client kubelib.Client

	// this client writes gateway resources to the cluster, it cannot take advantage of DelayedInformer
	// so it's got a custom mechanism to delay use until the CRD is known
	gatewaysClient kclient.Client[*gatewayapi.Gateway]

	queue controllers.Queue

	namespacesNeedingWaypoint krt.Collection[namespace]

	internalStop chan struct{}
	krtDebugger  *krt.DebugHandler
}

// Run starts the auto waypoint controller
func (a *AutoWaypoint) Run(stop <-chan struct{}) {
	a.queue.Run(stop)
	close(a.internalStop)
	controllers.ShutdownAll(a.gatewaysClient)
}

func NewAutoWaypoint(kubeClient kubelib.Client, krtDebugger *krt.DebugHandler) *AutoWaypoint {
	log.Debugf("starting auto waypoint controller")

	aw := &AutoWaypoint{
		client:         kubeClient,
		internalStop:   make(chan struct{}),
		krtDebugger:    krtDebugger,
		gatewaysClient: kclient.New[*gatewayapi.Gateway](kubeClient),
	}

	aw.setup()
	return aw
}

type namespace struct {
	name string
}

var _ krt.ResourceNamer = &namespace{}

func (n namespace) ResourceName() string {
	return n.name
}

func (a *AutoWaypoint) setup() {
	filter := kclient.Filter{
		ObjectFilter: a.client.ObjectFilter(),
	}

	opts := krt.NewOptionsBuilder(a.internalStop, "autowaypoint", a.krtDebugger)

	namespaces := krt.NewInformerFiltered[*corev1.Namespace](a.client, filter, opts.WithName("informer/namespaces")...)

	gateways := krt.NewInformerFiltered[*gatewayapi.Gateway](a.client, filter, opts.WithName("informer/gateways")...)

	services := krt.NewInformerFiltered[*corev1.Service](a.client, filter, opts.WithName("informer/services")...)

	// We want no hard dependencies on any CRD, use a delayted informer
	serviceEntriesClient := kclient.NewDelayedInformer[*networkingv1.ServiceEntry](a.client, gvr.ServiceEntry, kubetypes.StandardInformer, filter)
	serviceEntries := krt.WrapClient(serviceEntriesClient, opts.WithName("informer/serviceentries")...)

	gatewaysOfInterestByNamespace := krt.NewIndex(gateways, func(o *gatewayapi.Gateway) []string {
		if o.Name == "auto" {
			return []string{o.Namespace}
		}
		return nil
	})

	serviceXesUsingAutoWaypoint, serviceXesUsingAutoWaypointByNamespace := serviceXesUsingAutoWaypoint(services, serviceEntries, opts)

	needsWaypoints := krt.NewCollection(namespaces, func(ctx krt.HandlerContext, ns *corev1.Namespace) *namespace {
		if ns.DeletionTimestamp != nil {
			log.Debugf("namespace %s is being deleted, waypoint is not needed", ns.Name)
			return nil
		}
		w := krt.Fetch(ctx, gateways, krt.FilterIndex(gatewaysOfInterestByNamespace, ns.Name))
		if len(w) > 0 {
			log.Debugf("namespace %s has a gateway named auto, waypoint will not be deployed", ns.Name)
			return nil
		}
		serviceXes := krt.Fetch(ctx, serviceXesUsingAutoWaypoint, krt.FilterIndex(serviceXesUsingAutoWaypointByNamespace, ns.Name))
		if len(serviceXes) > 0 {
			kind := serviceXes[0].resourceType.Resource
			log.Debugf("namespace %s has %s using auto waypoint, deploying waypoint", ns.Name, kind)
			return &namespace{
				name: ns.Name,
			}
		}
		namespaceLabels := labels.Set(ns.GetLabels())
		if requestsAutoWaypoint(namespaceLabels) {
			log.Debugf("namespace %s requests auto waypoint, deploying waypoint", ns.Name)
			return &namespace{
				name: ns.Name,
			}
		}
		return nil
	}, opts.WithName("namespacesNeedingWaypoint")...)

	a.queue = controllers.NewQueue("auto waypoint",
		controllers.WithGenericReconciler(a.deployWaypoint),
		controllers.WithMaxAttempts(5))

	needsWaypoints.Register(func(o krt.Event[namespace]) {
		a.queue.Add(o.New)
	})

	a.namespacesNeedingWaypoint = needsWaypoints
}

func (a *AutoWaypoint) deployWaypoint(key any) error {
	namespace := key.(*namespace)
	if namespace == nil {
		return nil
	}

	wp := gatewayapi.Gateway{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.KubernetesGateway_v1.Kind,
			APIVersion: gvk.KubernetesGateway_v1.GroupVersion(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "auto",
			Namespace: namespace.name,
			Annotations: map[string]string{
				"solo.io/auto-waypoint": "true",
			},
		},
		Spec: gatewayapi.GatewaySpec{
			GatewayClassName: constants.WaypointGatewayClassName,
			Listeners: []gatewayapi.Listener{{
				Name:     "mesh",
				Port:     15008,
				Protocol: gatewayapi.ProtocolType(protocol.HBONE),
			}},
		},
	}

	res, err := a.gatewaysClient.Create(&wp)
	if err != nil {
		if kerrors.IsAlreadyExists(err) {
			return nil
		}
		log.Errorf("failed to create waypoint: %v", err)
		return err
	}
	log.Infof("waypoint created in %s", res.Namespace)
	return nil
}
