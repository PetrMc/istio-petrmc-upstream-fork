// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"golang.org/x/time/rate"
)

type (
	// ECSClient abstracts the portions of ECS client we use for testing purposes.
	ECSClient interface {
		ecs.ListTasksAPIClient
		ecs.DescribeTasksAPIClient
		ecs.ListServicesAPIClient
		ecs.DescribeServicesAPIClient
		ecs.ListClustersAPIClient
		DescribeClustersAPIClient
	}

	// this API client interface is not included in the generated AWS SDK for "reasons": https://github.com/aws/aws-sdk-go-v2/issues/2264
	// including it here to make the above interface easier to mock when testing
	DescribeClustersAPIClient interface {
		DescribeClusters(context.Context, *ecs.DescribeClustersInput, ...func(*ecs.Options)) (*ecs.DescribeClustersOutput, error)
	}
)

// ecsClient is a simple wrapper around ecs.Client with a rate limiter
type ecsClient struct {
	limiter *rate.Limiter
	client  *ecs.Client
}

func (c *ecsClient) ListTasks(ctx context.Context, params *ecs.ListTasksInput, optFns ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.client.ListTasks(ctx, params, optFns...)
}

func (c *ecsClient) DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.client.DescribeTasks(ctx, params, optFns...)
}

func (c *ecsClient) ListServices(ctx context.Context, params *ecs.ListServicesInput, optFns ...func(*ecs.Options)) (*ecs.ListServicesOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.client.ListServices(ctx, params, optFns...)
}

func (c *ecsClient) DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput,
	optFns ...func(*ecs.Options),
) (*ecs.DescribeServicesOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.client.DescribeServices(ctx, params, optFns...)
}

func (c *ecsClient) DescribeClusters(ctx context.Context, params *ecs.DescribeClustersInput,
	optFns ...func(*ecs.Options),
) (*ecs.DescribeClustersOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.client.DescribeClusters(ctx, params, optFns...)
}

func (c *ecsClient) ListClusters(ctx context.Context, params *ecs.ListClustersInput, optFns ...func(*ecs.Options)) (*ecs.ListClustersOutput, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.client.ListClusters(ctx, params, optFns...)
}
