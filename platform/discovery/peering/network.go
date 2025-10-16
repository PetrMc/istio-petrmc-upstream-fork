// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering

import (
	"fmt"
	"strings"
	"sync"

	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	networkid "istio.io/istio/pkg/network"
	"istio.io/istio/pkg/workloadapi"
)

const ClusterSegmentResourceName = "cluster-segment"

type peerCluster struct {
	queue controllers.Queue

	// this peerCluster's view of the localNetwork at creation time
	// if it would change, we must re-init this peerCluster
	localNetwork networkid.ID

	networkName networkid.ID
	clusterID   cluster.ID
	xds         *adsc.Client
	mu          sync.Mutex
	stop        chan struct{}
	stopped     bool

	federatedServices krt.StaticCollection[RemoteFederatedService]
	workloads         krt.StaticCollection[RemoteWorkload]
	segmentInfo       *workloadapi.Segment
	segmentResolved   chan struct{}

	synced    *atomic.Bool
	connected *atomic.Bool
	locality  string
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

func (s *peerCluster) IsFlat() bool {
	return EnableFlatNetworks && s.networkName == s.localNetwork
}

func (s *peerCluster) HasSynced() bool {
	return s.synced.Load()
}

func (s *peerCluster) IsConnected() bool {
	return s.connected.Load()
}

func (s *peerCluster) GetSegmentInfo() *workloadapi.Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.segmentInfo
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
		localNetwork: localNetwork,

		clusterID:         gateway.Cluster,
		networkName:       gateway.Network,
		stop:              stop,
		queue:             queue,
		federatedServices: svcCollection,
		workloads:         workloadCollection,
		synced:            atomic.NewBool(false),
		connected:         atomic.NewBool(false),
		locality:          gateway.Locality,
		segmentResolved:   make(chan struct{}),
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
			c.federatedServices.DeleteObject(fmt.Sprintf("%s/%s/%s", gateway.Cluster.String(), ns, name))
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
	segmentHandler := adsc.Register(func(
		ctx adsc.HandlerContext,
		resourceName string,
		resourceVersion string,
		val *workloadapi.Segment,
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

		// Special case: If we get an EventDelete for the Segment watch itself (not a specific segment),
		// it means the server doesn't support Segment resources at all (backwards compat with older Istio versions).
		// This happens when we try to watch "cluster-segment" but the server doesn't support the Segment type.
		if event == adsc.EventDelete && (resourceName == "" || resourceName == ClusterSegmentResourceName) && val == nil {
			log.Warnf("cluster %s doesn't support Segment resources, using default segment", c.clusterID)
			if c.segmentInfo == nil {
				// Create default segment info
				c.segmentInfo = &workloadapi.Segment{
					Name:   "default",
					Domain: strings.TrimPrefix(DomainSuffix, "."),
				}
				// Mark as resolved
				select {
				case <-c.segmentResolved:
					// Already closed
				default:
					close(c.segmentResolved)
				}
				// Queue a Cluster event for reconciliation
				c.queue.Add(typedNamespace{
					NamespacedName: types.NamespacedName{Name: c.clusterID.String()},
					kind:           Cluster,
				})
			}
			return
		}

		if val == nil {
			return
		}
		switch event {
		case adsc.EventAdd:
			changed := false
			wasNil := c.segmentInfo == nil
			if wasNil || !proto.Equal(c.segmentInfo, val) {
				changed = true
			}
			c.segmentInfo = val

			// Mark segment as resolved on first receipt
			if wasNil {
				select {
				case <-c.segmentResolved:
					// Already closed
				default:
					close(c.segmentResolved)
					log.Infof("received segment info from cluster %s: segment=%s, domain=%s",
						c.clusterID, val.GetName(), val.GetDomain())
				}
			}

			// Queue a Cluster event for reconciliation
			if changed {
				c.queue.Add(typedNamespace{
					NamespacedName: types.NamespacedName{Name: c.clusterID.String()},
					kind:           Cluster,
				})
			}
		case adsc.EventDelete:
			// ignore deletes - if the remote segment changes we only care about the latest Add
		}
	})

	handlers := []adsc.Option{
		federatedServiceAdscHandler,
		adsc.Watch[*workloadapi.FederatedService]("*"),
		segmentHandler,
		adsc.Watch[*workloadapi.Segment](ClusterSegmentResourceName),
	}

	// Always watch workloads, as we need NodePort based workloads
	// even when using multi-network. Filtering is done server side,
	// so we won't get Pod workloads unless we're flat network.
	handlers = append(handlers,
		workloadsAdscHandler,
		adsc.Watch[*workloadapi.Workload]("*"))

	cfg.ConnectionEventHandler = func(event adsc.ConnectionEvent, reason string) {
		switch event {
		case adsc.Connected:
			c.connected.Store(true)
			statusUpdateFunc()
		case adsc.Disconnected:
			// only update status on repeated disconnected event to avoid transient disconnects
			if !c.connected.Load() {
				statusUpdateFunc()
			}
			c.connected.Store(false)
		}
	}

	c.xds = adsc.NewDelta(gateway.Address, cfg, handlers...)
	go c.run()
	go func() {
		// First, wait for initial XDS sync
		select {
		case <-c.xds.Synced():
		case <-stop:
			return
		}
		log.Infof("XDS sync complete, waiting for segment info...")

		// Wait for segment info to be resolved (either received or explicitly not supported)
		select {
		case <-c.segmentResolved:
			// Segment info has been resolved - either we received it or server doesn't support it
			c.mu.Lock()
			segmentInfo := c.segmentInfo
			c.mu.Unlock()
			if segmentInfo != nil && segmentInfo.Name != "" {
				log.Infof("segment info received for cluster %s: segment=%s", c.clusterID, segmentInfo.Name)
			} else {
				log.Infof("cluster %s using default segment", c.clusterID)
			}
		case <-stop:
			return
		}

		log.Infof("cluster %s sync complete", c.clusterID)
		c.synced.Store(true)
		statusUpdateFunc()
		c.federatedServices.Register(serviceHandler)
		c.workloads.Register(workloadHandler)
		// Trigger a sync to cleanup any stale resources
		c.resync()
	}()
	return c
}
