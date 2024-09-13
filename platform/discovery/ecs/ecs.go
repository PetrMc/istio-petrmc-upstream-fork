// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"reflect"
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
	EcsCluster     = env.Register("ECS_CLUSTER", "", "Name of the ecs cluster").Get()
	EcsRole        = env.Register("ECS_ROLE", "", "AWS IAM role to assume when making requests via the client").Get()
	EcsMaxInterval = env.Register("ECS_DISCOVERY_MAX_INTERVAL", time.Minute*2, "Max interval between ECS polls").Get()
	EcsMinInterval = env.Register("ECS_DISCOVERY_MIN_INTERVAL", time.Second*5, "Min interval between ECS polls").Get()
)

type ECSDiscovery struct {
	client     ECSClient
	ecsCluster string

	lookupNetwork LookupNetwork

	poller *DynamicPoller

	workloadEntries kclient.Client[*clientnetworking.WorkloadEntry]
	serviceEntries  kclient.Client[*clientnetworking.ServiceEntry]
	queue           controllers.Queue

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
		client:        ecsClient,
		ecsCluster:    EcsCluster,
		queue:         controllers.Queue{},
		snapshot:      atomic.NewPointer(ptr.Of(map[string]EcsDiscovered{})),
		poller:        poller,
		lookupNetwork: lookupNetwork,
	}
	c.serviceEntries = kclient.New[*clientnetworking.ServiceEntry](kubeClient)
	c.workloadEntries = kclient.New[*clientnetworking.WorkloadEntry](kubeClient)
	c.queue = controllers.NewQueue("ecs",
		controllers.WithReconciler(c.Reconcile),
		controllers.WithMaxAttempts(25))

	return c
}

func (d *ECSDiscovery) Snapshot() map[string]EcsDiscovered {
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

const ecsNamespace = "ecs"

func (d *ECSDiscovery) Run(stop <-chan struct{}) {
	kube.WaitForCacheSync("ecs", stop, d.serviceEntries.HasSynced, d.workloadEntries.HasSynced)
	log.Infof("running")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.PollDiscovery(ctx)
	d.queue.Run(stop)
	controllers.ShutdownAll(d.serviceEntries, d.workloadEntries)
}

// PollDiscovery runs a loop collecting and reconciling service discovery information from ECS.
func (d *ECSDiscovery) PollDiscovery(ctx context.Context) {
	for {
		log.Debugf("running discovery...")
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
func (d *ECSDiscovery) HandleDiff(state []EcsDiscovered) bool {
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

func (d *ECSDiscovery) HasSynced() bool {
	return d.queue.HasSynced()
}

// MarkIncomingXDS marks that we established an XDS connection from the specified task ARN.
// This is used to trigger an on-demand sync when we don't yet know about the task, to speed up reconciliation.
// TODO: this has a fundamental flaw: it only works if the thing running the ECS syncer is the same as the instance connected over XDS.
func (d *ECSDiscovery) MarkIncomingXDS(cluster string, arn string) {
	if d.ecsCluster != cluster {
		log.Warnf("got connection from ECS cluster %v, watching %v (task %v)",
			cluster, d.ecsCluster, arn)
	}
	allEcsWorkloadEntries := d.workloadEntries.List(metav1.NamespaceAll, serviceLabelSelector)
	for _, we := range allEcsWorkloadEntries {
		if we.Annotations[ARNAnnotation] == arn {
			// Found it: no action needed
			return
		}
	}
	log.Infof("ECS Task %q connected, triggering a sync", arn)
	d.poller.TriggerNow()
}

var ServiceSelector = func() klabels.Selector {
	lbl, err := klabels.Parse(ServiceLabel)
	if err != nil {
		panic("failed to parse label")
	}
	return lbl
}()
