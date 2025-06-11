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
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/workloadapi"
)

type network struct {
	queue controllers.Queue

	cluster    string
	xds        *adsc.Client
	mu         sync.Mutex
	stop       chan struct{}
	stopped    bool
	collection krt.StaticCollection[RemoteFederatedService]
	synced     *atomic.Bool
}

func (s *network) run() {
	// TODO: wait for this to sync
	ctx := status.NewIstioContext(s.stop)
	s.xds.Run(ctx)
	<-s.stop
	s.resync()
}

func (s *network) shutdownNow() {
	s.mu.Lock()
	if s.stopped {
		return
	}
	close(s.stop)
	s.stopped = true
	s.mu.Unlock()
	s.resync()
}

func (s *network) resync() {
	s.queue.Add(typedNamespace{
		NamespacedName: types.NamespacedName{Name: s.cluster},
		kind:           kind.MeshNetworks,
	})
}

func (s *network) HasSynced() bool {
	return s.synced.Load()
}

func newNetwork(
	networkName string,
	address string,
	cfg *adsc.DeltaADSConfig,
	debugger *krt.DebugHandler,
	queue controllers.Queue,
	handler func(o krt.Event[RemoteFederatedService]),
	statusUpdateFunc func(),
) *network {
	stop := make(chan struct{})
	opts := krt.NewOptionsBuilder(stop, "peering", debugger)
	name := "RemoteFederatedService/" + networkName
	collection := krt.NewStaticCollection[RemoteFederatedService](nil, nil, opts.WithName(name)...)
	log := log.WithLabels("network", networkName)
	log.Infof("starting network watch to %v", address)
	c := &network{
		cluster:    networkName,
		stop:       stop,
		queue:      queue,
		collection: collection,
		synced:     atomic.NewBool(false),
	}
	federatedServiceHandler := adsc.Register(func(
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
			c.collection.UpdateObject(RemoteFederatedService{
				FederatedService: val,
				Cluster:          networkName,
			})
		case adsc.EventDelete:
			// FSDS key is ns/hostname. We are currently only supporting Service, and use ns/name in the rest of the code.
			// Translate it.
			ns, hostname, _ := strings.Cut(resourceName, "/")
			name, _, _ := strings.Cut(hostname, ".")
			c.collection.DeleteObject(fmt.Sprintf("%s/%s/%s", networkName, ns, name))
		}
	})

	handlers := []adsc.Option{
		federatedServiceHandler,
		adsc.Watch[*workloadapi.FederatedService]("*"),
	}

	c.xds = adsc.NewDelta(address, cfg, handlers...)
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
		c.collection.Register(handler)
		// Trigger a sync to cleanup any stale resources
		c.resync()
	}()
	return c
}
