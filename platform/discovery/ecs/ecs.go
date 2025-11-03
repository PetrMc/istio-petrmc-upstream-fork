// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/time/rate"
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
	"istio.io/istio/pkg/slices"
)

var log = istiolog.RegisterScope("ecs", "Ambient ECS")

var (
	// TODO: this UX is pretty clunky, need to consider a CRD for this (and other platforms?)
	ECSAccounts = func() []ecsAccount {
		v := env.Register("ECS_ACCOUNTS", "", "key/value list of domains to AWS Role ARNs to assume when discovering ECS clusters").Get()
		if len(v) == 0 {
			return []ecsAccount{}
		}
		segs := strings.Split(v, ",")
		if len(segs)%2 != 0 {
			log.Fatal("invalid ECS_ACCOUNTS formatting (must be of form 'domain1,role1,domain2,role2...)")
		}
		accounts := make([]ecsAccount, len(segs)/2)
		for i := 0; i < len(segs)-1; i += 2 {
			accounts[i/2] = ecsAccount{
				domain: segs[i],
				role:   segs[i+1],
			}
		}
		log.Infof("configured ecs accounts: %v", accounts)
		return accounts
	}()
	// AWS normal limit is 50 per account, so defaulting to half that to be safe
	ECSRateLimit = env.Register("ECS_DISCOVERY_RATE_LIMIT", rate.Limit(25),
		"Rate limit in requests per second for polling ECS API (applied per role/account)").Get()
	// AWS normal limit is 20 per account, so defaulting to half that to be safe
	ECSBurstLimit = env.Register("ECS_DISCOVERY_BURST_LIMIT", 10,
		"Burst limit for polling ECS API (applied per role/account)").Get()
)

type ECSDiscovery struct {
	accounts   map[string]*ecsAccountDiscovery
	kubeClient kube.Client
}

type ecsAccountDiscovery struct {
	account       ecsAccount
	client        ECSClient
	lookupNetwork LookupNetwork

	// map of cluster ARN to discovered ECS resources: map[arn.ARN]ecsClusterDiscovery
	clusters sync.Map

	workloadEntries kclient.Client[*clientnetworking.WorkloadEntry]
	serviceEntries  kclient.Client[*clientnetworking.ServiceEntry]
	queue           controllers.Queue
}

type ecsClusterDiscovery struct {
	cancel context.CancelFunc
	// map of resource name to workload/service
	snapshot map[string]EcsDiscovered
}

type ecsAccount struct {
	domain string
	role   string
}

type (
	LookupNetwork func(endpointIP string, labels labels.Instance) network.ID
)

func NewECS(client kube.Client, lookupNetwork LookupNetwork) *ECSDiscovery {
	baseConfig, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatalf("failed to load default ECS config: %v", err)
	}

	// create clients for each configured role
	d := &ECSDiscovery{
		accounts:   map[string]*ecsAccountDiscovery{},
		kubeClient: client,
	}
	for _, account := range ECSAccounts {
		log.Debugf("creating client for role %s", account.role)

		// configure role assumption
		optFns := []func(*config.LoadOptions) error{
			config.WithCredentialsProvider(aws.NewCredentialsCache(
				stscreds.NewAssumeRoleProvider(
					sts.NewFromConfig(baseConfig),
					account.role,
				),
			)),
		}

		// "default" is a special case to preserve the legacy way of using the base istiod IAM role
		// instead of assuming a different role
		if account.role == "default" {
			optFns = []func(*config.LoadOptions) error{}
		}

		cfg, err := config.LoadDefaultConfig(
			context.TODO(),
			optFns...,
		)
		if err != nil {
			log.Fatalf("failed to load AWS client config for role %s: %v", account.role, err)
		}

		r := ecsAccountDiscovery{
			account: account,
			client: &ecsClient{
				client:  ecs.NewFromConfig(cfg),
				limiter: rate.NewLimiter(ECSRateLimit, ECSBurstLimit),
			},
			lookupNetwork:   lookupNetwork,
			clusters:        sync.Map{},
			serviceEntries:  kclient.New[*clientnetworking.ServiceEntry](client),
			workloadEntries: kclient.New[*clientnetworking.WorkloadEntry](client),
		}
		// multiple accounts could have the same domain so use role for uniqueness
		r.queue = controllers.NewQueue(fmt.Sprintf("ecs-%s", account.role),
			controllers.WithReconciler(r.Reconcile),
			controllers.WithMaxAttempts(25))
		d.accounts[account.role] = &r
	}

	return d
}

// Snapshot retrieves the current Snapshot of the cluster state. Safe to call concurrently.
func (a *ecsAccountDiscovery) Snapshot(cluster string) map[string]EcsDiscovered {
	clusterARN, err := arn.Parse(cluster)
	if err != nil {
		log.Errorf("failed to parse cluster ARN %s: %v", cluster, err)
	}

	v, found := a.clusters.Load(clusterARN)
	if !found {
		return map[string]EcsDiscovered{}
	}
	return v.(ecsClusterDiscovery).snapshot
}

// Store saves the current snapshot of the cluster state. Safe to call concurrently.
func (a *ecsAccountDiscovery) Store(cluster arn.ARN, state map[string]EcsDiscovered) {
	v, found := a.clusters.Load(cluster)
	if !found {
		log.Errorf("cluster not found: %s", cluster)
		return
	}
	c := v.(ecsClusterDiscovery)
	c.snapshot = state
	a.clusters.Store(cluster, c)
}

// name.Namespace = ARN of the cluster
// name.Name = internal resource name
func (a *ecsAccountDiscovery) Reconcile(name types.NamespacedName) error {
	resourceType, _, _, err := ParseResourceName(name.Name)
	if err != nil {
		return err
	}
	switch resourceType {
	case "task":
		return a.reconcileWorkloadEntry(name)
	case "service":
		return a.reconcileServiceEntry(name)
	}
	return nil
}

func (d *ECSDiscovery) Run(stop <-chan struct{}) {
	var wg sync.WaitGroup
	for _, a := range d.accounts {
		kube.WaitForCacheSync(a.account.role, stop, a.serviceEntries.HasSynced, a.workloadEntries.HasSynced)
		wg.Add(1)
		go func() {
			defer wg.Done()
			go a.PollClusterDiscovery(stop)
			a.queue.Run(stop)
			controllers.ShutdownAll(a.serviceEntries, a.workloadEntries)
		}()
	}
	wg.Wait()
}

func (a *ecsAccountDiscovery) PollClusterDiscovery(stop <-chan struct{}) {
	var wg sync.WaitGroup
	for {
		select {
		case <-stop:
			log.Infof("stopping ecs discovery for role: %v", a.account.role)
			wg.Wait()
			return
		default:
			discoveredClusters, err := a.DiscoverClusters(context.TODO())
			if err != nil {
				// probably a network/rate limit error: log and try again
				log.Warnf("failed to discover clusters: %v", err)
				continue
			}

			for _, discoveredCluster := range discoveredClusters {
				_, found := a.clusters.Load(discoveredCluster)
				if found {
					// discovery for cluster is already running, nothing to do
					continue
				}

				// start discovery for cluster
				ctx, cancel := context.WithCancel(context.Background())
				a.clusters.Store(discoveredCluster, ecsClusterDiscovery{
					cancel:   cancel,
					snapshot: map[string]EcsDiscovered{},
				})
				wg.Add(1)
				go func() {
					defer wg.Done()
					a.PollDiscovery(ctx, discoveredCluster)
				}()
			}

			// stop discovery for removed clusters
			a.clusters.Range(func(key, value any) bool {
				existingCluster := key.(arn.ARN)
				if !slices.Contains(discoveredClusters, existingCluster) {
					c := value.(ecsClusterDiscovery)
					// shutdown background discovery
					c.cancel()
					a.clusters.Delete(existingCluster)
				}
				return true
			})
		}
	}
}

// PollDiscovery runs a loop collecting and reconciling service discovery information from ECS.
func (a *ecsAccountDiscovery) PollDiscovery(ctx context.Context, cluster arn.ARN) {
	log.Infof("starting discovery for cluster %s", cluster)
	for {
		if ctx.Err() != nil {
			// We are done
			log.Infof("shutting down discovery for cluster %s", cluster)
			// trigger diff with empty state to cleanup all removed resources
			a.HandleDiff(cluster, []EcsDiscovered{})
			return
		}

		state, err := a.DiscoverResources(ctx, cluster)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Warnf("failed to run discovery: %v", err)
			}
		} else {
			log.Debugf("discovered %v resources", len(state))
			a.HandleDiff(cluster, state)
		}
	}
}

// HandleDiff reconciles the current state based on a new snapshot. It returns if there were any changes
func (a *ecsAccountDiscovery) HandleDiff(cluster arn.ARN, state []EcsDiscovered) {
	nextState := map[string]EcsDiscovered{}
	lastSnapshot := a.Snapshot(cluster.String())
	var toAdd []types.NamespacedName
	for _, r := range state {
		rn := r.ResourceName()
		nextState[rn] = r
		if old, f := lastSnapshot[rn]; f && reflect.DeepEqual(r, old) {
			// No changes to this object, skip
			continue
		}
		// Object is changed, prepare to enqueue it (but need to wait until we store the nextState as the snapshot)
		toAdd = append(toAdd, types.NamespacedName{
			Name:      r.ResourceName(),
			Namespace: cluster.String(),
		})
	}
	a.Store(cluster, nextState)
	for _, r := range toAdd {
		a.queue.Add(r)
	}

	// trigger deletion for removed ECS resources
	for _, o := range a.serviceEntries.List(metav1.NamespaceAll, ServiceSelector) {
		a.filterEntries(cluster, nextState, o)
	}
	for _, o := range a.workloadEntries.List(metav1.NamespaceAll, ServiceSelector) {
		a.filterEntries(cluster, nextState, o)
	}
}

// filterEntries will delete objects which no longer exist in ECS
func (a *ecsAccountDiscovery) filterEntries(cluster arn.ARN, nextState map[string]EcsDiscovered, o controllers.Object) {
	// don't delete objects discovered by different roles or in different clusters
	if a.account.role != o.GetAnnotations()[DiscoveredByAnnotation] || strings.SplitN(cluster.Resource, "/", 2)[1] != o.GetAnnotations()[ClusterAnnotation] {
		return
	}
	if _, found := nextState[o.GetAnnotations()[ResourceAnnotation]]; !found {
		a.queue.Add(types.NamespacedName{
			Name:      o.GetAnnotations()[ResourceAnnotation],
			Namespace: cluster.String(),
		})
	}
}

func (a *ecsAccountDiscovery) HasSynced() bool {
	return a.queue.HasSynced()
}

var ServiceSelector = func() klabels.Selector {
	lbl, err := klabels.Parse(ServiceLabel)
	if err != nil {
		log.Errorf("failed to parse label %s", ServiceLabel)
	}
	return lbl
}()
