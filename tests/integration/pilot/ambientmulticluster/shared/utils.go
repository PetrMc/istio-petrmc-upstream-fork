//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package shared

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiannotation "istio.io/api/annotation"
	"istio.io/api/label"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/ambient"
	"istio.io/istio/pkg/test/framework/components/cluster"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/common/ports"
	"istio.io/istio/pkg/test/framework/components/echo/deployment"
	"istio.io/istio/pkg/test/framework/components/echo/match"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/framework/resource/config/apply"
	"istio.io/istio/pkg/test/scopes"
	"istio.io/istio/platform/discovery/peering"
)

const (
	LocalCluster         = "primary"
	RemoteFlatCluster    = "remote"
	RemoteNetworkCluster = "cross-network-primary"

	// used by most services in all clusters
	WaypointDefault = "waypoint"
	// only used by ServiceCrossNetworkOnlyWaypoint in the RemoteNetworkCluster
	WaypointXNet = "x-net-waypoint"
)

type EchoDeployments struct {
	// Namespace echo apps will be deployed
	Namespace namespace.Instance
	// App echo services
	LocalApp      echo.Instances
	Sidecar       echo.Instances
	LocalWaypoint echo.Instances
}

// SetupApps sets up a single workload. We will make multiple distinct services all selecting this workload
// nsServiceScope: use the namespace level label for configuring global service scope and set service-scope cluster
// on services that do not have a global scope; otherwise set service-scoep global on each service that requires it
func SetupApps(t resource.Context, apps *EchoDeployments, nsServiceScope bool) error {
	var err error
	if _, err := namespace.Claim(t, namespace.Config{Prefix: peering.PeeringNamespace}); err != nil {
		return err
	}

	labels := map[string]string{
		label.IoIstioDataplaneMode.Name: "ambient",
	}
	if nsServiceScope {
		labels[peering.ServiceScopeLabel] = string(peering.ServiceScopeGlobal)
	}

	apps.Namespace, err = namespace.New(t, namespace.Config{
		Prefix: "echo",
		Inject: false,
		Labels: labels,
	})
	if err != nil {
		return err
	}

	localBuilder := deployment.New(t).DeployServicesOnlyToCluster().WithClusters(
		t.Clusters().GetByName(LocalCluster),
	)
	remoteFlatBuilder := deployment.New(t).DeployServicesOnlyToCluster().WithClusters(
		t.Clusters().GetByName(RemoteFlatCluster),
	)
	remoteBuilder := deployment.New(t).DeployServicesOnlyToCluster().WithClusters(
		t.Clusters().GetByName(RemoteFlatCluster),
		t.Clusters().GetByName(RemoteNetworkCluster),
	)

	deployToAllClusters := func(
		name string,
		localSettings ServiceSettings,
		remoteSettings ServiceSettings,
	) {
		localSettings.Name = name
		localSettings.Namespace = apps.Namespace
		remoteSettings.Name = name
		remoteSettings.Namespace = apps.Namespace
		localBuilder.WithConfig(localSettings.ToConfig())
		remoteBuilder.WithConfig(remoteSettings.ToConfig())
	}

	switch nsServiceScope {
	case true:
		lcl := string(peering.ServiceScopeCluster)
		deployToAllClusters(ServiceLocal, ServiceSettings{Scope: lcl}, ServiceSettings{Scope: lcl})
		deployToAllClusters(ServiceRemoteGlobal, ServiceSettings{Scope: lcl}, ServiceSettings{})
		deployToAllClusters(ServiceLocalGlobal, ServiceSettings{}, ServiceSettings{Scope: lcl})
		deployToAllClusters(ServiceAllGlobal, ServiceSettings{}, ServiceSettings{})
		deployToAllClusters(ServiceSidecar, ServiceSettings{Sidecar: true}, ServiceSettings{Sidecar: true})

		deployToAllClusters(ServiceLocalWaypoint, ServiceSettings{Waypoint: true}, ServiceSettings{})
		deployToAllClusters(ServiceRemoteWaypoint, ServiceSettings{}, ServiceSettings{Waypoint: true})
		deployToAllClusters(ServiceAllWaypoint, ServiceSettings{Waypoint: true}, ServiceSettings{Waypoint: true})
		deployToAllClusters(ServiceGlobalTakeover,
			ServiceSettings{Scope: string(peering.ServiceScopeGlobalOnly)},
			ServiceSettings{Scope: string(peering.ServiceScopeGlobalOnly)})
	default:
		gbl := string(peering.ServiceScopeGlobal)
		deployToAllClusters(ServiceLocal, ServiceSettings{}, ServiceSettings{})
		deployToAllClusters(ServiceRemoteGlobal, ServiceSettings{}, ServiceSettings{Scope: gbl})
		deployToAllClusters(ServiceLocalGlobal, ServiceSettings{Scope: gbl}, ServiceSettings{})
		deployToAllClusters(ServiceAllGlobal, ServiceSettings{Scope: gbl}, ServiceSettings{Scope: gbl})
		deployToAllClusters(ServiceSidecar, ServiceSettings{Scope: gbl, Sidecar: true}, ServiceSettings{Scope: gbl, Sidecar: true})

		deployToAllClusters(ServiceLocalWaypoint, ServiceSettings{Scope: gbl, Waypoint: true}, ServiceSettings{Scope: gbl})
		deployToAllClusters(ServiceRemoteWaypoint, ServiceSettings{Scope: gbl}, ServiceSettings{Scope: gbl, Waypoint: true})
		deployToAllClusters(ServiceAllWaypoint, ServiceSettings{Scope: gbl, Waypoint: true}, ServiceSettings{Scope: gbl, Waypoint: true})
		deployToAllClusters(ServiceGlobalTakeover,
			ServiceSettings{Scope: string(peering.ServiceScopeGlobalOnly)},
			ServiceSettings{Scope: string(peering.ServiceScopeGlobalOnly)})
	}

	remoteFlatBuilder.WithConfig(ServiceSettings{
		Name:      ServiceRemoteFlatOnlyWaypoint,
		Namespace: apps.Namespace,
		Scope:     string(peering.ServiceScopeGlobal),
		Waypoint:  true,
	}.ToConfig())

	remoteBuilder.WithConfig(ServiceSettings{
		Name:      ServiceRemoteOnlyTakeover,
		Namespace: apps.Namespace,
		Scope:     string(peering.ServiceScopeGlobalOnly),
	}.ToConfig())

	remoteNetworkBuilder := deployment.New(t).DeployServicesOnlyToCluster().WithClusters(
		t.Clusters().GetByName(RemoteNetworkCluster),
	)

	remoteNetworkBuilder.WithConfig(ServiceSettings{
		Name:      ServiceCrossNetworkOnlyWaypoint,
		Namespace: apps.Namespace,
		Scope:     string(peering.ServiceScopeGlobal),
		Waypoint:  true,
		// HACK: right now, the way remote-waypoints are implemented will merge all of this same Waypoint service
		// meaning we have waypoint instances in the flat-network that route to pods that only exist for this service
		// on the other network. To test the "waypoint only in the remote network case" we need a distinct waypoint here.
		WaypointName: WaypointXNet,
	}.ToConfig())

	scopes.Framework.Infof("deploying to local cluster...")
	// Build the applications
	localApps, err := localBuilder.Build()
	if err != nil {
		return err
	}
	scopes.Framework.Infof("deploying to all remote clusters...")
	if _, err := remoteBuilder.Build(); err != nil {
		return err
	}
	scopes.Framework.Infof("deploying to remote cross-network cluster...")
	if _, err := remoteNetworkBuilder.Build(); err != nil {
		return err
	}
	scopes.Framework.Infof("deploying to remote flat-network cluster...")
	if _, err := remoteFlatBuilder.Build(); err != nil {
		return err
	}
	apps.LocalApp = match.ServiceName(echo.NamespacedName{Name: ServiceLocal, Namespace: apps.Namespace}).GetMatches(localApps)
	apps.Sidecar = match.ServiceName(echo.NamespacedName{Name: ServiceSidecar, Namespace: apps.Namespace}).GetMatches(localApps)
	apps.LocalWaypoint = match.ServiceName(echo.NamespacedName{Name: ServiceLocalWaypoint, Namespace: apps.Namespace}).GetMatches(localApps)

	if _, err := ambient.NewWaypointProxyForCluster(t, apps.Namespace, WaypointXNet, t.Clusters().GetByName(RemoteNetworkCluster)); err != nil {
		return err
	}

	for _, c := range t.Clusters() {
		if _, err := ambient.NewWaypointProxyForCluster(t, apps.Namespace, WaypointDefault, c); err != nil {
			return err
		}
	}

	for _, c := range t.Clusters() {
		for _, svc := range []string{
			ServiceLocalWaypoint,
			ServiceRemoteWaypoint,
			ServiceCrossNetworkOnlyWaypoint,
			ServiceAllWaypoint,
			ServiceRemoteFlatOnlyWaypoint,
		} {

			err := t.ConfigKube(c).Eval(
				apps.Namespace.Name(),
				map[string]string{
					"service":   svc,
					"cluster":   c.Name(),
					"namespace": apps.Namespace.Name(),
					"segment":   "default",
				},
				`apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: mark-header-{{.service}}
spec:
  parentRefs:
  - group: "networking.istio.io"
    kind: ServiceEntry
    name: {{if eq .segment "default"}}autogen.{{.namespace}}.{{.service}}{{else}}autogen.{{.segment}}.{{.namespace}}.{{.service}}{{end}}
    sectionName: "80"
  rules:
  - filters:
    - type: RequestHeaderModifier
      requestHeaderModifier:
        add:
        - name: x-istio-clusters
          value: {{.cluster}}
        - name: x-istio-workload
          value: "%ENVIRONMENT(HOSTNAME)%"
    backendRefs:
    - name: {{.service}}.{{.namespace}}.mesh.internal
      kind: Hostname
      group: networking.istio.io
      port: 80`,
			).Apply(apply.CleanupConditionally)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// SetupNodeLocality labels nodes in each cluster with different topology.kubernetes.io/region and zone labels.
// This must be called before deploying workloads so pods pick up locality from their nodes.
func SetupNodeLocality(t resource.Context) error {
	ctx := context.Background()

	// Map cluster names to their locality (region.zone format)
	clusterLocalities := map[string]struct{ region, zone string }{
		LocalCluster:         {region: "region-local", zone: "zone-local"},
		RemoteFlatCluster:    {region: "region-flat", zone: "zone-flat"},
		RemoteNetworkCluster: {region: "region-x-net", zone: "zone-x-net"},
	}

	for _, c := range t.Clusters() {
		locality, ok := clusterLocalities[c.Name()]
		if !ok {
			scopes.Framework.Infof("Skipping locality setup for unknown cluster %s", c.Name())
			continue
		}

		// Get all nodes in the cluster
		nodes, err := c.Kube().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list nodes in cluster %s: %w", c.Name(), err)
		}

		// Label each node with region and zone
		for _, node := range nodes.Items {
			labels := node.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			labels["topology.kubernetes.io/region"] = locality.region
			labels["topology.kubernetes.io/zone"] = locality.zone

			node.SetLabels(labels)
			_, err := c.Kube().CoreV1().Nodes().Update(ctx, &node, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to label node %s in cluster %s: %w", node.Name, c.Name(), err)
			}
			scopes.Framework.Infof("Labeled node %s in cluster %s with region=%s, zone=%s",
				node.Name, c.Name(), locality.region, locality.zone)
		}
	}

	return nil
}

func SetLabelForTest(
	t framework.TestContext,
	namespace string,
	label string,
	value string,
	clusters ...cluster.Cluster,
) {
	targetClusters := clusters
	if len(targetClusters) == 0 {
		targetClusters = t.Clusters()
	}

	for _, c := range targetClusters {
		// The segment label needs to be on the istio-system namespace
		// Use kubectl patch to add the label without creating a new namespace resource
		_, err := c.Kube().CoreV1().Namespaces().Patch(
			t.Context(),
			namespace,
			types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}}}`, label, value)),
			metav1.PatchOptions{})
		if err != nil {
			t.Fatalf("Failed to label istio-system namespace with segment %s on cluster %s: %v",
				value, c.Name(), err)
		}

		// Add cleanup to remove the label
		t.Cleanup(func() {
			_, err := c.Kube().CoreV1().Namespaces().Patch(
				t.Context(),
				namespace,
				types.MergePatchType,
				[]byte(fmt.Sprintf(`{"metadata":{"labels":{"%s":null}}}`, label)),
				metav1.PatchOptions{})
			if err != nil {
				t.Logf("Failed to remove segment label from istio-system namespace on cluster %s: %v",
					c.Name(), err)
			}
		})
	}
}

// SetupHTTPRoutesForSegment creates HTTPRoutes for waypoint services with the given segment and domain
func SetupHTTPRoutesForSegment(t resource.Context, namespace namespace.Instance, segment, domain string, clusters ...cluster.Cluster) error {
	if segment == "" {
		segment = "default"
	}
	if domain == "" {
		domain = peering.DomainSuffix[1:] // Remove leading dot
	}

	targetClusters := clusters
	if len(targetClusters) == 0 {
		targetClusters = t.Clusters()
	}

	for _, c := range targetClusters {
		for _, svc := range []string{
			ServiceLocalWaypoint,
			ServiceRemoteWaypoint,
			ServiceCrossNetworkOnlyWaypoint,
			ServiceAllWaypoint,
			ServiceRemoteFlatOnlyWaypoint,
		} {
			err := t.ConfigKube(c).Eval(
				namespace.Name(),
				map[string]string{
					"service":   svc,
					"cluster":   c.Name(),
					"namespace": namespace.Name(),
					"segment":   segment,
					"domain":    domain,
				},
				`apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: mark-header-{{.service}}-{{.segment}}
spec:
  parentRefs:
  - group: "networking.istio.io"
    kind: ServiceEntry
    name: {{if eq .segment "default"}}autogen.{{.namespace}}.{{.service}}{{else}}autogen.{{.segment}}.{{.namespace}}.{{.service}}{{end}}
    sectionName: "80"
  rules:
  - filters:
    - type: RequestHeaderModifier
      requestHeaderModifier:
        add:
        - name: x-istio-clusters
          value: {{.cluster}}
        - name: x-istio-workload
          value: "%ENVIRONMENT(HOSTNAME)%"
    backendRefs:
    - name: {{.service}}.{{.namespace}}.{{.domain}}
      kind: Hostname
      group: networking.istio.io
      port: 80`,
			).Apply(apply.CleanupConditionally)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

const (
	// ServiceLocal is a service that is not marked as global at all
	ServiceLocal = "local"
	// ServiceRemoteGlobal is a service that is marked as global only on the remote side
	ServiceRemoteGlobal = "remote-global"
	// ServiceLocalGlobal is a service that is marked as global only on the local side
	ServiceLocalGlobal = "local-global"
	// ServiceAllGlobal is a service that is marked as global
	ServiceAllGlobal = "all-global"

	// ServiceSidecar is a service that has a sidecar
	ServiceSidecar = "sidecar"
	// ServiceLocalWaypoint is a service that has a waypoint locally
	ServiceLocalWaypoint = "local-waypoint"
	// ServiceRemoteWaypoint is a service that has a waypoint remotely
	ServiceRemoteWaypoint = "remote-waypoint"
	// ServiceRemoteFlatOnlyWaypoint is a service that has a waypoint in the remote flat network cluster and does not exist
	// in either of the local network or cross-network clusters.
	ServiceRemoteFlatOnlyWaypoint = "remote-flat-only-waypoint"
	// ServiceCrossNetworkOnlyWaypoint is a service that has a waypoint in the cross-network cluster and does not exist
	// in either of the flat network or local network clusters.
	ServiceCrossNetworkOnlyWaypoint = "cross-net-only-waypoint"
	// ServiceAllWaypoint is a service that has a waypoint in all clusters
	ServiceAllWaypoint = "all-waypoint"
	// ServiceGlobalTakeover is a service that is marked as global-only, taking over the .cluster.local DNS name
	ServiceGlobalTakeover = "global-takeover"

	// ServiceRemoteOnlyTakeover is a service that is marked as global-only, taking over the .cluster.local DNS name
	// This service only exists in the remote clusters (not LocalCluster), but calling from the LocalCluster can still
	// resolve the standard Kubernetes Service DNS
	ServiceRemoteOnlyTakeover = "remote-takeover"
)

var AllServices = []string{
	ServiceLocal,
	ServiceRemoteGlobal,
	ServiceLocalGlobal,
	ServiceAllGlobal,

	ServiceSidecar,
	ServiceLocalWaypoint,
	ServiceRemoteWaypoint,
	ServiceCrossNetworkOnlyWaypoint,
	ServiceAllWaypoint,
	ServiceGlobalTakeover,
	ServiceRemoteOnlyTakeover,
}

type ServiceSettings struct {
	Name      string
	Namespace namespace.Instance
	// Scope, if set, will mark this as solo.io/service-scope=<scope>
	Scope string
	// PreferClose, if true, will mark this as trafficDistribution=PreferClose
	PreferClose bool
	// UnhealthyEndpoints, if true, will not have any healthy endpoints
	UnhealthyEndpoints bool
	// Sidecar, if true, will deploy with a sidecar
	Sidecar bool
	// Waypoint, if true, will attach the service to a waypoint
	Waypoint bool
	// WaypointName, if non-empty when Waypoint is true, overrides the name of the waypoint for this service
	WaypointName string
}

func (s ServiceSettings) ToConfig() echo.Config {
	replicas := 1
	labels := map[string]string{}
	annos := map[string]string{}
	if s.UnhealthyEndpoints {
		replicas = 0
	}
	if s.Scope != "" {
		labels[peering.ServiceScopeLabel] = s.Scope
	}
	if s.PreferClose {
		annos[apiannotation.NetworkingTrafficDistribution.Name] = "PreferClose"
	} else {
		// We default to PreferNetwork which makes testing harder.
		// Default to "Any" for our tests
		annos[apiannotation.NetworkingTrafficDistribution.Name] = "Any"
	}
	if s.Sidecar {
		labels["sidecar.istio.io/inject"] = "true"
	}
	var svcWaypoint string
	if s.Waypoint {
		waypointName := WaypointDefault
		if s.WaypointName != "" {
			waypointName = s.WaypointName
		}
		labels[label.IoIstioUseWaypoint.Name] = waypointName
		svcWaypoint = waypointName
	}
	return echo.Config{
		ServiceWaypointProxy: svcWaypoint,
		Service:              s.Name,
		Namespace:            s.Namespace,
		ServiceLabels:        labels,
		ServiceAnnotations:   annos,
		Ports:                ports.All(),
		Subsets:              []echo.SubsetConfig{{Replicas: replicas, Labels: labels}},
	}
}

// SetTrafficDistributionOnService sets the networking.istio.io/traffic-distribution annotation on a Service resource.
func SetTrafficDistributionOnService(t framework.TestContext, name, ns string, distribution string) {
	for _, c := range t.Clusters() {
		// Fetch the current value
		svc, err := c.Kube().CoreV1().Services(ns).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		oldAnnotation := ""
		if svc.Annotations != nil {
			oldAnnotation = svc.Annotations["networking.istio.io/traffic-distribution"]
		}

		set := func(dist string) error {
			var annotation string
			if dist != "" {
				annotation = fmt.Sprintf(`"networking.istio.io/traffic-distribution":%q`, dist)
			} else {
				annotation = `"networking.istio.io/traffic-distribution":null`
			}

			patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%s}}}`, annotation))
			_, err := c.Kube().CoreV1().Services(ns).Patch(context.TODO(), name, types.MergePatchType, patch, metav1.PatchOptions{})
			return controllers.IgnoreNotFound(err)
		}

		if err := set(distribution); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := set(oldAnnotation); err != nil {
				scopes.Framework.Errorf("failed resetting traffic distribution for Service %s", name)
			}
		})
	}
}

// SetTrafficDistributionOnGateway sets the networking.istio.io/traffic-distribution annotation in spec.infrastructure.annotations
// on a Gateway resource so it propagates to the Gateway proxy.
func SetTrafficDistributionOnGateway(t framework.TestContext, name, ns string, distribution string) {
	for _, c := range t.Clusters() {
		// Fetch the current value
		gw, err := c.GatewayAPI().GatewayV1().Gateways(ns).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		oldAnnotation := ""
		if gw.Spec.Infrastructure != nil && gw.Spec.Infrastructure.Annotations != nil {
			if val, ok := gw.Spec.Infrastructure.Annotations["networking.istio.io/traffic-distribution"]; ok {
				oldAnnotation = string(val)
			}
		}

		set := func(dist string) error {
			var annotation string
			if dist != "" {
				annotation = fmt.Sprintf(`"networking.istio.io/traffic-distribution":%q`, dist)
			} else {
				annotation = `"networking.istio.io/traffic-distribution":null`
			}

			// Set annotation in spec.infrastructure.annotations for Gateway
			patch := []byte(fmt.Sprintf(`{"spec":{"infrastructure":{"annotations":{%s}}}}`, annotation))
			_, err := c.GatewayAPI().GatewayV1().Gateways(ns).Patch(context.TODO(), name, types.MergePatchType, patch, metav1.PatchOptions{})
			return controllers.IgnoreNotFound(err)
		}

		if err := set(distribution); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := set(oldAnnotation); err != nil {
				scopes.Framework.Errorf("failed resetting traffic distribution for Gateway %s", name)
			}
		})
	}
}
