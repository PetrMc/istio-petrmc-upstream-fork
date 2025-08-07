// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant
package ambient

import (
	"istio.io/istio/pilot/pkg/model"
)

var _ Index = NoopIndex{}

type NoopIndex struct {
	model.NoopAmbientIndexes
}

func (n NoopIndex) Lookup(key string) []model.AddressInfo {
	return nil
}

func (n NoopIndex) All() []model.AddressInfo {
	return nil
}

func (n NoopIndex) AllLocalNetworkGlobalServices(key model.WaypointKey) []model.ServiceInfo {
	return nil
}

func (n NoopIndex) WorkloadsForWaypoint(key model.WaypointKey) []model.WorkloadInfo {
	return nil
}

func (n NoopIndex) ServicesForWaypoint(key model.WaypointKey) []model.ServiceInfo {
	return nil
}

func (n NoopIndex) Run(stop <-chan struct{}) {
}

func (n NoopIndex) HasSynced() bool {
	return true
}
