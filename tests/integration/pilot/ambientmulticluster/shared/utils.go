//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package shared

import (
	apiannotation "istio.io/api/annotation"
	"istio.io/api/label"
	"istio.io/istio/pkg/test/framework/components/ambient"
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
	LocalCluster  = "primary"
	RemoteCluster = "cross-network-primary"
)

type EchoDeployments struct {
	// Namespace echo apps will be deployed
	Namespace namespace.Instance
	// App echo services
	LocalApp echo.Instances
	Sidecar  echo.Instances
}

// SetupApps sets up a single workload. We will make multiple distinct services all selecting this workload
func SetupApps(t resource.Context, apps *EchoDeployments) error {
	var err error
	if _, err := namespace.Claim(t, namespace.Config{Prefix: peering.PeeringNamespace}); err != nil {
		return err
	}
	apps.Namespace, err = namespace.New(t, namespace.Config{
		Prefix: "echo",
		Inject: false,
		Labels: map[string]string{
			label.IoIstioDataplaneMode.Name: "ambient",
		},
	})
	if err != nil {
		return err
	}

	localBuilder := deployment.New(t).DeployServicesOnlyToCluster().WithClusters(t.Clusters().GetByName(LocalCluster))
	remoteBuilder := deployment.New(t).DeployServicesOnlyToCluster().WithClusters(t.Clusters().GetByName(RemoteCluster))

	deployToBothClusters := func(name string, localSettings ServiceSettings, remoteSettings ServiceSettings) {
		localSettings.Name = name
		localSettings.Namespace = apps.Namespace
		remoteSettings.Name = name
		remoteSettings.Namespace = apps.Namespace
		localBuilder.WithConfig(localSettings.ToConfig())
		remoteBuilder.WithConfig(remoteSettings.ToConfig())
	}

	gbl := peering.ServiceScopeGlobal
	deployToBothClusters(ServiceLocal, ServiceSettings{}, ServiceSettings{})
	deployToBothClusters(ServiceRemoteGlobal, ServiceSettings{}, ServiceSettings{Scope: gbl})
	deployToBothClusters(ServiceLocalGlobal, ServiceSettings{Scope: gbl}, ServiceSettings{})
	deployToBothClusters(ServiceBothGlobal, ServiceSettings{Scope: gbl}, ServiceSettings{Scope: gbl})
	deployToBothClusters(ServiceSidecar, ServiceSettings{Scope: gbl, Sidecar: true}, ServiceSettings{Scope: gbl, Sidecar: true})
	deployToBothClusters(ServiceLocalWaypoint, ServiceSettings{Scope: gbl, Waypoint: true}, ServiceSettings{Scope: gbl})
	deployToBothClusters(ServiceRemoteWaypoint, ServiceSettings{Scope: gbl}, ServiceSettings{Scope: gbl, Waypoint: true})
	deployToBothClusters(ServiceBothWaypoint, ServiceSettings{Scope: gbl, Waypoint: true}, ServiceSettings{Scope: gbl, Waypoint: true})
	deployToBothClusters(ServiceGlobalTakeover, ServiceSettings{Scope: peering.ServiceScopeGlobalOnly}, ServiceSettings{Scope: peering.ServiceScopeGlobalOnly})
	remoteBuilder.WithConfig(ServiceSettings{
		Name:      ServiceRemoteOnlyWaypoint,
		Namespace: apps.Namespace,
		Scope:     gbl,
		Waypoint:  true,
	}.ToConfig())

	scopes.Framework.Infof("deploying to local cluster...")
	// Build the applications
	localApps, err := localBuilder.Build()
	if err != nil {
		return err
	}
	scopes.Framework.Infof("deploying to remote cluster...")
	if _, err := remoteBuilder.Build(); err != nil {
		return err
	}
	apps.LocalApp = match.ServiceName(echo.NamespacedName{Name: ServiceLocal, Namespace: apps.Namespace}).GetMatches(localApps)
	apps.Sidecar = match.ServiceName(echo.NamespacedName{Name: ServiceSidecar, Namespace: apps.Namespace}).GetMatches(localApps)

	for _, c := range t.Clusters() {
		if _, err := ambient.NewWaypointProxyCluster(t, apps.Namespace, "waypoint", c); err != nil {
			return err
		}
		for _, svc := range []string{ServiceLocalWaypoint, ServiceRemoteWaypoint, ServiceRemoteOnlyWaypoint, ServiceBothWaypoint} {

			err := t.ConfigKube(c).Eval(
				apps.Namespace.Name(),
				map[string]string{"service": svc, "cluster": c.Name(), "namespace": apps.Namespace.Name()},
				`apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: mark-header-{{.service}}
spec:
  parentRefs:
  - group: "networking.istio.io"
    kind: ServiceEntry
    name: autogen.{{.namespace}}.{{.service}}
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

const (
	// ServiceLocal is a service that is not marked as global at all
	ServiceLocal = "local"
	// ServiceRemoteGlobal is a service that is marked as global only on the remote side
	ServiceRemoteGlobal = "remote-global"
	// ServiceLocalGlobal is a service that is marked as global only on the local side
	ServiceLocalGlobal = "local-global"
	// ServiceBothGlobal is a service that is marked as global
	ServiceBothGlobal = "both-global"
	// ServiceSidecar is a service that has a sidecar
	ServiceSidecar = "sidecar"
	// ServiceLocalWaypoint is a service that has a waypoint locally
	ServiceLocalWaypoint = "local-waypoint"
	// ServiceRemoteWaypoint is a service that has a waypoint remotely
	ServiceRemoteWaypoint = "remote-waypoint"
	// ServiceRemoteOnlyWaypoint is a service that has a waypoint remotely and does not exist locally
	ServiceRemoteOnlyWaypoint = "remote-only-waypoint"
	// ServiceBothWaypoint is a service that has a waypoint both locally and remote
	ServiceBothWaypoint = "both-waypoint"
	// ServiceGlobalTakeover is a service that is marked as global-only, taking over the .cluster.local DNS name
	ServiceGlobalTakeover = "global-takeover"
)

var AllServices = []string{
	ServiceLocal,
	ServiceLocalGlobal,
	ServiceRemoteGlobal,
	ServiceBothGlobal,
	ServiceLocalWaypoint,
	ServiceRemoteWaypoint,
	ServiceRemoteOnlyWaypoint,
	ServiceBothWaypoint,
	ServiceSidecar,
	ServiceGlobalTakeover,
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
		labels[label.IoIstioUseWaypoint.Name] = "waypoint"
		svcWaypoint = "waypoint"
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
