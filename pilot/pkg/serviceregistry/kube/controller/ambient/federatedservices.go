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
) krt.Collection[model.FederatedService] {
	return krt.NewCollection(Services, func(ctx krt.HandlerContext, svc model.ServiceInfo) *model.FederatedService {
		if svc.Source.Kind != kind.Service {
			// Currently we only export Kubernetes services
			return nil
		}
		if !svc.GlobalService {
			return nil
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
		if waypoint := krt.FetchOne(ctx, Waypoints, krt.FilterKey(svc.Waypoint.ResourceName)); waypoint != nil {
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
		fs := &workloadapi.FederatedService{
			Name:            s.Name,
			Namespace:       s.Namespace,
			Hostname:        s.Hostname,
			SubjectAltNames: sets.SortedList(uniqueSANS),
			Ports:           s.Ports,
			Capacity:        wrappers.UInt32(bucket(workloadCount)),
		}
		if s.Waypoint != nil {
			fs.Waypoint = &workloadapi.RemoteWaypoint{}
		}
		return &model.FederatedService{FederatedService: fs}
	}, opts.WithName("FederatedServices")...)
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
