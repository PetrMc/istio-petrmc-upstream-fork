// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
)

func (a *ecsAccountDiscovery) DiscoverResources(ctx context.Context, cluster arn.ARN) ([]EcsDiscovered, error) {
	log.Debugf("calling discovery")
	ecsTasks, err := a.getTasks(ctx, cluster)
	if err != nil {
		return nil, err
	}
	ecsServices, err := a.getServices(ctx, cluster)
	if err != nil {
		return nil, err
	}
	res := []EcsDiscovered{}

	// process services
	for _, s := range ecsServices {
		res = append(res, EcsDiscovered{
			Domain:  a.account.domain,
			Cluster: strings.SplitN(cluster.Resource, "/", 2)[1],
			ARN:     *s.ServiceArn,
			Tags:    ParseTags(s.Tags),
			Service: &EcsService{
				Name: *s.ServiceName,
			},
		})
	}

	// process tasks
	for _, task := range ecsTasks {
		var ip netip.Addr
		for _, at := range task.Attachments {
			// TODO: always look at ElasticNetworkInterface attachment maybe?
			for _, v := range at.Details {
				if *v.Name == "privateIPv4Address" {
					tip, err := netip.ParseAddr(*v.Value)
					if err != nil {
						continue
					}
					ip = tip
				}
			}
		}
		if !ip.IsValid() {
			// No IP, ignore this for now
			continue
		}

		// mesh is determined by whether or not there is a ztunnel sidecar
		// TODO: needs a rework to handle sidecarless deployments like on EC2 nodes with ztunnel in daemon mode
		mesh := false
		for _, c := range task.Containers {
			if c.Name != nil && *c.Name == "ztunnel" {
				mesh = true
				break
			}
		}

		res = append(res, EcsDiscovered{
			Domain:  a.account.domain,
			Cluster: strings.SplitN(cluster.Resource, "/", 2)[1],
			ARN:     *task.TaskArn,
			Tags:    ParseTags(task.Tags),
			Task: &EcsTask{
				Group:   *task.Group,
				Zone:    *task.AvailabilityZone,
				Address: ip,
				Mesh:    mesh,
			},
		})

	}
	return res, nil
}

func (a *ecsAccountDiscovery) getTasks(ctx context.Context, cluster arn.ARN) ([]ecstypes.Task, error) {
	req := &ecs.ListTasksInput{Cluster: ptr.Of(cluster.String())}
	res := []ecstypes.Task{}
	pg := ecs.NewListTasksPaginator(a.client, req)
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		arns := page.TaskArns
		// We can only describe 100 at a time...
		for i := 0; i < len(arns); i += 100 {
			end := min(100, len(arns)-i)
			seq := arns[i : i+end]
			desc := &ecs.DescribeTasksInput{
				Tasks:   seq,
				Cluster: ptr.Of(cluster.String()),
				// Mark we want to include tags
				Include: []ecstypes.TaskField{ecstypes.TaskFieldTags},
			}
			o, err := a.client.DescribeTasks(ctx, desc)
			if err != nil {
				return nil, err
			}
			res = append(res, o.Tasks...)
		}
	}
	return res, nil
}

func (a *ecsAccountDiscovery) getServices(ctx context.Context, cluster arn.ARN) ([]ecstypes.Service, error) {
	req := &ecs.ListServicesInput{Cluster: ptr.Of(cluster.String())}
	res := []ecstypes.Service{}
	pg := ecs.NewListServicesPaginator(a.client, req)
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		arns := page.ServiceArns
		// We can only describe 10 at a time
		for i := 0; i < len(arns); i += 10 {
			end := min(10, len(arns)-i)
			seq := arns[i : i+end]
			desc := &ecs.DescribeServicesInput{
				Services: seq,
				Cluster:  ptr.Of(cluster.String()),
				// Mark we want to include tags
				Include: []ecstypes.ServiceField{ecstypes.ServiceFieldTags},
			}
			o, err := a.client.DescribeServices(ctx, desc)
			if err != nil {
				return nil, err
			}
			res = append(res, o.Services...)
		}
	}
	return res, nil
}

func (a *ecsAccountDiscovery) DiscoverClusters(ctx context.Context) ([]arn.ARN, error) {
	clusters, err := a.getClusters(ctx)
	if err != nil {
		return nil, err
	}

	res := make([]arn.ARN, len(clusters))
	for i, c := range clusters {
		a, err := arn.Parse(*c.ClusterArn)
		if err != nil {
			return nil, fmt.Errorf("invalid ARN %s: %v", *c.ClusterArn, err)
		}
		res[i] = a
	}

	return res, nil
}

func (a *ecsAccountDiscovery) getClusters(ctx context.Context) ([]ecstypes.Cluster, error) {
	req := &ecs.ListClustersInput{}
	res := []ecstypes.Cluster{}
	pg := ecs.NewListClustersPaginator(a.client, req)
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		arns := page.ClusterArns
		// We can only describe 100 at a time
		for i := 0; i < len(arns); i += 100 {
			end := min(100, len(arns)-i)
			seq := arns[i : i+end]
			desc := &ecs.DescribeClustersInput{
				Clusters: seq,
				// Mark we want to include tags
				Include: []ecstypes.ClusterField{ecstypes.ClusterFieldTags},
			}
			o, err := a.client.DescribeClusters(ctx, desc)
			if err != nil {
				return nil, err
			}

			// filter out untagged cluster
			o.Clusters = slices.Filter(o.Clusters, func(c ecstypes.Cluster) bool {
				return ParseTags(c.Tags).DiscoveryTag
			})

			res = append(res, o.Clusters...)
		}
	}
	return res, nil
}
