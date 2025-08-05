// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

// nolint: gocritic
package ambient

import (
	wrappers "google.golang.org/protobuf/types/known/wrapperspb"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/spiffe"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/workloadapi"
)

func (a *index) FederatedServicesCollection(
	MeshConfig krt.Singleton[MeshConfig],
	Services krt.Collection[model.ServiceInfo],
	Workloads krt.Collection[model.WorkloadInfo],
	Waypoints krt.Collection[Waypoint],
	WorkloadsByService krt.Index[string, model.WorkloadInfo],
	opts krt.OptionsBuilder,
) (
	krt.Collection[model.FederatedService],
	krt.Collection[krt.Named],
) {
	globalServiceByWaypoint := krt.NewIndex(Services, "globalServiceByWaypoint", func(s model.ServiceInfo) []string {
		if s.GlobalService && s.Waypoint.ResourceName != "" {
			return []string{s.Waypoint.ResourceName}
		}
		return nil
	})
	// check if the given service is a waypoint, and any of the services using it are global
	isGlobalWaypoint := func(ctx krt.HandlerContext, name, ns string) bool {
		n := len(krt.Fetch(ctx, Services, krt.FilterIndex(globalServiceByWaypoint, ns+"/"+name)))
		return n > 0
	}

	federatedServices := krt.NewCollection(Services, func(ctx krt.HandlerContext, svc model.ServiceInfo) *model.FederatedService {
		if svc.Source.Kind != kind.Service {
			// Currently we only export Kubernetes services
			return nil
		}

		isGlobalWaypoint := isGlobalWaypoint(ctx, svc.Service.GetName(), svc.GetNamespace())
		if !svc.GlobalService && !isGlobalWaypoint {
			return nil
		}

		waypointFor := ""
		if isGlobalWaypoint {
			waypointFor = "service"
		}

		workloads := krt.Fetch(ctx, Workloads, krt.FilterIndex(WorkloadsByService, svc.ResourceName()))
		workloadCount := len(workloads)
		uniqueSANS := sets.New[string]()
		for _, w := range workloads {
			w := w.Workload
			if w.ServiceAccount == "" {
				continue
			}
			td := w.TrustDomain
			if td == "" {
				// Workload elides when it is default; we don't here since it is the full URI
				td = "cluster.local"
			}
			uniqueSANS.Insert(spiffe.MustGenSpiffeURIForTrustDomain(td, w.Namespace, w.ServiceAccount))
		}

		// we may also hit the waypoint, so mark that as allowed as well
		waypoint := krt.FetchOne(ctx, Waypoints, krt.FilterKey(svc.Waypoint.ResourceName))
		if waypoint != nil {
			meshCfg := krt.FetchOne(ctx, MeshConfig.AsCollection())
			sas := waypoint.ServiceAccounts
			if len(sas) == 0 {
				// This is an approximation that doesn't account for overrides. TODO: get the real value
				sas = []string{waypoint.Name}
			}
			uniqueSANS.InsertAll(slices.Map(sas, func(sa string) string {
				return spiffe.MustGenSpiffeURIForTrustDomain(meshCfg.GetTrustDomain(), waypoint.Namespace, sa)
			})...)
		}

		s := svc.Service

		protocolsByPort := make(map[uint32]string, len(svc.ProtocolsByPort))
		for port, protocol := range svc.ProtocolsByPort {
			if !protocol.IsUnsupported() {
				protocolsByPort[uint32(port)] = protocol.String()
			}
		}

		fs := &workloadapi.FederatedService{
			Name:                s.Name,
			Namespace:           s.Namespace,
			Hostname:            s.Hostname,
			SubjectAltNames:     sets.SortedList(uniqueSANS),
			Ports:               s.Ports,
			Capacity:            wrappers.UInt32(bucket(workloadCount)),
			TrafficDistribution: svc.TrafficDistribution.String(),
			WaypointFor:         waypointFor,
			ProtocolsByPort:     protocolsByPort,
		}
		if s.Waypoint != nil {
			fs.Waypoint = &workloadapi.RemoteWaypoint{
				Name:      waypoint.GetName(),
				Namespace: waypoint.GetNamespace(),
			}
		}
		return &model.FederatedService{FederatedService: fs}
	}, opts.WithName("FederatedServices")...)

	// HACK: we want to get notified of any Global Waypoints in the peering controller
	federatedWaypoints := krt.NewCollection(federatedServices, func(ctx krt.HandlerContext, fs model.FederatedService) *krt.Named {
		if !isGlobalWaypoint(ctx, fs.Name, fs.Namespace) {
			return nil
		}
		return &krt.Named{Name: fs.Name, Namespace: fs.Namespace}
	})
	return federatedServices, federatedWaypoints
}

var buckets = []int{0, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048}

// bucket reduces an exact endpoint count to a bucketed count.
// Capacity for remote services doesn't need 100 precision, and doing so introduces churn at scale.
// Instead, we use a bucketing strategy to reduce changes
func bucket(count int) uint32 {
	idx, found := slices.BinarySearch(buckets, count)
	if found {
		return uint32(buckets[idx])
	}
	if idx == 0 {
		return 0
	}
	return uint32(buckets[idx-1])
}
