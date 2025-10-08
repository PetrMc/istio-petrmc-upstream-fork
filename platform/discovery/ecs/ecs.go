// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"go.uber.org/atomic"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/env"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
)

var log = istiolog.RegisterScope("ecs", "Ambient ECS")

var (
	EcsClusters = func() []string {
		v := env.Register("ECS_CLUSTERS", "", "Names of ECS clusters to monitor for discovery (comma-separated list)").Get()
		if len(v) > 0 {
			return strings.Split(v, ",")
		}
		return []string{}
	}()
	EcsRole        = env.Register("ECS_ROLE", "", "AWS IAM role to assume when making requests via the client").Get()
	EcsMaxInterval = env.Register("ECS_DISCOVERY_MAX_INTERVAL", time.Minute*2, "Max interval between ECS polls").Get()
	EcsMinInterval = env.Register("ECS_DISCOVERY_MIN_INTERVAL", time.Second*5, "Min interval between ECS polls").Get()
)

type ECSDiscovery struct {
	clusters map[string]ECSClusterDiscovery

	lookupNetwork LookupNetwork
}

type ECSClusterDiscovery struct {
	client         ECSClient
	ecsClusterName string

	poller *DynamicPoller

	workloadEntries kclient.Client[*clientnetworking.WorkloadEntry]
	serviceEntries  kclient.Client[*clientnetworking.ServiceEntry]
	queue           controllers.Queue

	// map of resource name to workload/service
	snapshot *atomic.Pointer[map[string]EcsDiscovered]
}

type (
	LookupNetwork func(endpointIP string, labels labels.Instance) network.ID
)

func NewECS(client kube.Client, lookupNetwork LookupNetwork) *ECSDiscovery {
	// Load the Shared AWS Configuration (~/.aws/config)
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if EcsRole != "" {
		stsclient := sts.NewFromConfig(cfg)
		cfg, err = config.LoadDefaultConfig(
			context.TODO(),
			config.WithCredentialsProvider(aws.NewCredentialsCache(
				stscreds.NewAssumeRoleProvider(
					stsclient,
					EcsRole,
				),
			)),
		)
	}
	if err != nil {
		log.Fatalf("failed to initialize ECS client: %v", err)
	}

	return newECS(client, ecs.NewFromConfig(cfg), NewDynamicPoller(EcsMinInterval, EcsMaxInterval), lookupNetwork)
}

func newECS(kubeClient kube.Client, ecsClient ECSClient, poller *DynamicPoller, lookupNetwork LookupNetwork) *ECSDiscovery {
	c := &ECSDiscovery{
		clusters:      map[string]ECSClusterDiscovery{},
		lookupNetwork: lookupNetwork,
	}
	log.Infof("registering ecs clusters: %v", EcsClusters)
	for _, clusterName := range EcsClusters {
		c.clusters[clusterName] = ECSClusterDiscovery{
			client:         ecsClient,
			ecsClusterName: clusterName,
			queue: controllers.NewQueue(fmt.Sprintf("ecs-%s", clusterName),
				controllers.WithReconciler(c.Reconcile),
				controllers.WithMaxAttempts(25)),
			snapshot:        atomic.NewPointer(ptr.Of(map[string]EcsDiscovered{})),
			poller:          poller,
			serviceEntries:  kclient.New[*clientnetworking.ServiceEntry](kubeClient),
			workloadEntries: kclient.New[*clientnetworking.WorkloadEntry](kubeClient),
		}
	}

	return c
}

func (d *ECSClusterDiscovery) Snapshot() map[string]EcsDiscovered {
	return *d.snapshot.Load()
}

func (d *ECSDiscovery) Reconcile(raw types.NamespacedName) error {
	switch raw.Namespace {
	case "workload":
		return d.reconcileWorkloadEntry(raw.Name)
	case "service":
		return d.reconcileServiceEntry(raw.Name)
	}
	return nil
}

func (d *ECSDiscovery) Run(stop <-chan struct{}) {
	var wg sync.WaitGroup
	for _, c := range d.clusters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kube.WaitForCacheSync(c.ecsClusterName, stop, c.serviceEntries.HasSynced, c.workloadEntries.HasSynced)
			log.Infof("starting discovery for cluster %s", c.ecsClusterName)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go c.PollDiscovery(ctx)
			c.queue.Run(stop)
			controllers.ShutdownAll(c.serviceEntries, c.workloadEntries)
		}()
	}
	wg.Wait()
}

// PollDiscovery runs a loop collecting and reconciling service discovery information from ECS.
func (d *ECSClusterDiscovery) PollDiscovery(ctx context.Context) {
	for {
		changesFound := false
		state, err := d.Discovery(ctx)
		if err != nil {
			log.Warnf("failed to run discovery: %v", err)
		} else {
			log.Debugf("discovered %v resources", len(state))
			changesFound = d.HandleDiff(state)
		}
		if !d.poller.Wait(ctx, changesFound) {
			// We are done
			return
		}
	}
}

// HandleDiff reconciles the current state based on a new snapshot. It returns if there were any changes
func (d *ECSClusterDiscovery) HandleDiff(state []EcsDiscovered) bool {
	changes := false
	nextState := map[string]EcsDiscovered{}
	lastSnapshot := d.Snapshot()
	var toAdd []types.NamespacedName
	for _, r := range state {
		rn := r.ResourceName()
		nextState[rn] = r
		if old, f := lastSnapshot[rn]; f && reflect.DeepEqual(r, old) {
			// No changes to this object, skip
			continue
		}
		changes = true
		// Object is changed, prepare to enqueue it (but need to wait until we store the nextState as the snapshot)
		toAdd = append(toAdd, types.NamespacedName{
			Name:      r.ResourceName(),
			Namespace: r.Type(),
		})
	}
	d.snapshot.Store(&nextState)
	for _, r := range toAdd {
		d.queue.Add(r)
	}
	for _, o := range d.serviceEntries.List(metav1.NamespaceAll, ServiceSelector) {
		// skip objects from different ecs cluster
		if _, cluster, _, _, _ := ParseResourceName(o.Annotations[ResourceAnnotation]); cluster != d.ecsClusterName {
			continue
		}
		if _, f := nextState[o.Annotations[ResourceAnnotation]]; !f {
			// Object exists in cluster but not in ECS anymore... trigger a deletion
			d.queue.Add(types.NamespacedName{
				Name:      o.Annotations[ResourceAnnotation],
				Namespace: "service",
			})
			changes = true
		}
	}
	for _, o := range d.workloadEntries.List(metav1.NamespaceAll, ServiceSelector) {
		// skip objects from different ecs cluster
		if _, cluster, _, _, _ := ParseResourceName(o.Annotations[ResourceAnnotation]); cluster != d.ecsClusterName {
			continue
		}
		if _, f := nextState[o.Annotations[ResourceAnnotation]]; !f {
			// Object exists in cluster but not in ECS anymore... trigger a deletion
			d.queue.Add(types.NamespacedName{
				Name:      o.Annotations[ResourceAnnotation],
				Namespace: "workload",
			})
			changes = true
		}
	}
	return changes
}

var serviceLabelSelector = func() klabels.Selector {
	sel, _ := klabels.Parse(ServiceLabel)
	return sel
}()

func (d *ECSClusterDiscovery) HasSynced() bool {
	return d.queue.HasSynced()
}

// MarkIncomingXDS marks that we established an XDS connection from the specified task ARN.
// This is used to trigger an on-demand sync when we don't yet know about the task, to speed up reconciliation.
// TODO: this has a fundamental flaw: it only works if the thing running the ECS syncer is the same as the instance connected over XDS.
func (d *ECSDiscovery) MarkIncomingXDS(clusterName string, arn string) {
	cluster, known := d.clusters[clusterName]
	if !known {
		log.Warnf("got connection from ECS cluster %v, watching %v (task %v)", cluster, EcsClusters, arn)
		return
	}
	allEcsWorkloadEntries := cluster.workloadEntries.List(metav1.NamespaceAll, serviceLabelSelector)
	for _, we := range allEcsWorkloadEntries {
		if we.Annotations[ARNAnnotation] == arn {
			// Found it: no action needed
			return
		}
	}
	log.Infof("ECS Task %q connected, triggering a sync", arn)
	cluster.poller.TriggerNow()
}

var ServiceSelector = func() klabels.Selector {
	lbl, err := klabels.Parse(ServiceLabel)
	if err != nil {
		log.Errorf("failed to parse label %s", ServiceLabel)
	}
	return lbl
}()
