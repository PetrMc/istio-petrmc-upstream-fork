// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"golang.org/x/time/rate"

	"istio.io/istio/pkg/slices"
)

// fakeEcsClient implements the ECSClient interface for testing purposes
type fakeEcsClient struct {
	tasks    []types.Task
	services []types.Service
	clusters []types.Cluster
	limiter  *rate.Limiter
	mu       sync.RWMutex
}

func NewFakeClient(clusters []types.Cluster, tasks []types.Task, services []types.Service) ECSClient {
	return &fakeEcsClient{
		tasks:    tasks,
		services: services,
		clusters: clusters,
		limiter:  rate.NewLimiter(ECSRateLimit, ECSBurstLimit),
		mu:       sync.RWMutex{},
	}
}

func (c *fakeEcsClient) SetTasks(tasks []types.Task) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tasks = tasks
}

func (c *fakeEcsClient) SetServices(services []types.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.services = services
}

func (c *fakeEcsClient) SetClusters(clusters []types.Cluster) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusters = clusters
}

func (c *fakeEcsClient) ListTasks(ctx context.Context, params *ecs.ListTasksInput,
	optFns ...func(*ecs.Options),
) (*ecs.ListTasksOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &ecs.ListTasksOutput{
		NextToken: nil,
		TaskArns: slices.Map(slices.Filter(c.tasks, func(t types.Task) bool {
			return *t.ClusterArn == *params.Cluster
		}), func(t types.Task) string {
			return *t.TaskArn
		}),
	}, nil
}

func (c *fakeEcsClient) DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput,
	optFns ...func(*ecs.Options),
) (*ecs.DescribeTasksOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &ecs.DescribeTasksOutput{
		Tasks: slices.MapFilter(params.Tasks, func(e string) *types.Task {
			return slices.FindFunc(c.tasks, func(t types.Task) bool {
				return *t.TaskArn == e
			})
		}),
	}, nil
}

func (c *fakeEcsClient) ListServices(ctx context.Context, params *ecs.ListServicesInput,
	optFns ...func(*ecs.Options),
) (*ecs.ListServicesOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &ecs.ListServicesOutput{
		NextToken: nil,
		ServiceArns: slices.Map(slices.Filter(c.services, func(s types.Service) bool {
			return *s.ClusterArn == *params.Cluster
		}), func(s types.Service) string {
			return *s.ServiceArn
		}),
	}, nil
}

func (c *fakeEcsClient) DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput,
	optFns ...func(*ecs.Options),
) (*ecs.DescribeServicesOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &ecs.DescribeServicesOutput{
		Services: slices.MapFilter(params.Services, func(e string) *types.Service {
			return slices.FindFunc(c.services, func(s types.Service) bool {
				return *s.ServiceArn == e
			})
		}),
	}, nil
}

func (c *fakeEcsClient) ListClusters(ctx context.Context, params *ecs.ListClustersInput,
	optFns ...func(*ecs.Options),
) (*ecs.ListClustersOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	clusters := slices.Map(c.clusters, func(c types.Cluster) string {
		return *c.ClusterArn
	})
	return &ecs.ListClustersOutput{
		NextToken:   nil,
		ClusterArns: clusters,
	}, nil
}

func (c *fakeEcsClient) DescribeClusters(ctx context.Context, params *ecs.DescribeClustersInput,
	optFns ...func(*ecs.Options),
) (*ecs.DescribeClustersOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	clusters := slices.MapFilter(params.Clusters, func(e string) *types.Cluster {
		return slices.FindFunc(c.clusters, func(c types.Cluster) bool {
			return *c.ClusterArn == e
		})
	})
	return &ecs.DescribeClustersOutput{
		Clusters: clusters,
	}, nil
}
