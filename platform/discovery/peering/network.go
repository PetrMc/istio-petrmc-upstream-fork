// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering

import (
	"fmt"
	"strings"
	"sync"

	"go.uber.org/atomic"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	networkid "istio.io/istio/pkg/network"
	"istio.io/istio/pkg/workloadapi"
)

type peerCluster struct {
	queue controllers.Queue

	networkName networkid.ID
	clusterID   cluster.ID
	xds         *adsc.Client
	mu          sync.Mutex
	stop        chan struct{}
	stopped     bool

	federatedServices krt.StaticCollection[RemoteFederatedService]
	workloads         krt.StaticCollection[RemoteWorkload]

	synced   *atomic.Bool
	locality string
}

func (s *peerCluster) run() {
	// TODO: wait for this to sync
	ctx := status.NewIstioContext(s.stop)
	s.xds.Run(ctx)
	<-s.stop
	s.resync()
}

func (s *peerCluster) shutdownNow() {
	s.mu.Lock()
	if s.stopped {
		return
	}
	close(s.stop)
	s.stopped = true
	s.mu.Unlock()
	s.resync()
}

func (s *peerCluster) resync() {
	s.queue.Add(typedNamespace{
		NamespacedName: types.NamespacedName{Name: s.clusterID.String()},
		kind:           Cluster,
	})
}

func (s *peerCluster) HasSynced() bool {
	return s.synced.Load()
}

func newPeerCluster(
	localNetwork networkid.ID,
	gateway PeerGateway,
	cfg *adsc.DeltaADSConfig,
	debugger *krt.DebugHandler,
	queue controllers.Queue,
	serviceHandler func(o krt.Event[RemoteFederatedService]),
	workloadHandler func(o krt.Event[RemoteWorkload]),
	statusUpdateFunc func(),
) *peerCluster {
	networkName := gateway.Network.String()
	stop := make(chan struct{})
	opts := krt.NewOptionsBuilder(stop, "peering", debugger)

	svcCollection := krt.NewStaticCollection[RemoteFederatedService](nil, nil,
		opts.WithName("RemoteFederatedService/"+gateway.Cluster.String())...)
	workloadCollection := krt.NewStaticCollection[RemoteWorkload](nil, nil,
		opts.WithName("RemoteWorkload/"+gateway.Cluster.String())...)

	mode := "gateway"
	if EnableFlatNetworks && gateway.Network == localNetwork {
		mode = "flat"
	}
	log := log.WithLabels("cluster", gateway.Cluster, "network", networkName, "mode", mode)
	log.Infof("starting cluster watch to %v", gateway.Address)

	c := &peerCluster{
		clusterID:         gateway.Cluster,
		networkName:       gateway.Network,
		stop:              stop,
		queue:             queue,
		federatedServices: svcCollection,
		workloads:         workloadCollection,
		synced:            atomic.NewBool(false),
		locality:          gateway.Locality,
	}

	federatedServiceAdscHandler := adsc.Register(func(
		ctx adsc.HandlerContext,
		resourceName string,
		resourceVersion string,
		val *workloadapi.FederatedService,
		event adsc.Event,
	) {
		c.mu.Lock()
		defer c.mu.Unlock()
		select {
		case <-stop:
			// We are stopped, do not process any events
			return
		default:
		}
		switch event {
		case adsc.EventAdd:
			c.federatedServices.UpdateObject(RemoteFederatedService{
				Service: val,
				Cluster: gateway.Cluster.String(),
			})
		case adsc.EventDelete:
			// FSDS key is ns/hostname. We are currently only supporting Service, and use ns/name in the rest of the code.
			// Translate it.
			ns, hostname, _ := strings.Cut(resourceName, "/")
			name, _, _ := strings.Cut(hostname, ".")
			c.federatedServices.DeleteObject(fmt.Sprintf("%s/%s/%s", networkName, ns, name))
		}
	})
	workloadsAdscHandler := adsc.Register(func(
		ctx adsc.HandlerContext,
		resourceName string,
		resourceVersion string,
		val *workloadapi.Workload,
		event adsc.Event,
	) {
		c.mu.Lock()
		defer c.mu.Unlock()
		select {
		case <-stop:
			// We are stopped, do not process any events
			return
		default:
		}
		switch event {
		case adsc.EventAdd:
			c.workloads.UpdateObject(RemoteWorkload{
				Workload: val,
				Cluster:  gateway.Cluster.String(),
			})
		case adsc.EventDelete:
			c.workloads.DeleteObject(resourceName)
		}
	})

	handlers := []adsc.Option{
		federatedServiceAdscHandler,
		adsc.Watch[*workloadapi.FederatedService]("*"),
	}

	if EnableFlatNetworks && gateway.Network == localNetwork {
		handlers = append(handlers,
			workloadsAdscHandler,
			adsc.Watch[*workloadapi.Workload]("*"))
	}

	c.xds = adsc.NewDelta(gateway.Address, cfg, handlers...)
	go c.run()
	go func() {
		select {
		case <-c.xds.Synced():
		case <-stop:
			return
		}
		log.Infof("sync complete")
		c.synced.Store(true)
		statusUpdateFunc()
		c.federatedServices.Register(serviceHandler)
		c.workloads.Register(workloadHandler)
		// Trigger a sync to cleanup any stale resources
		c.resync()
	}()
	return c
}
