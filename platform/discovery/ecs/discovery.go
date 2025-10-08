// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"net/netip"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func (d *ECSClusterDiscovery) Discovery(ctx context.Context) ([]EcsDiscovered, error) {
	log.Debugf("calling discovery")
	ecsTasks, err := d.getTasks(ctx)
	if err != nil {
		return nil, err
	}
	ecsServices, err := d.getServices(ctx)
	if err != nil {
		return nil, err
	}
	res := []EcsDiscovered{}
	for _, ecsSvc := range ecsServices {
		res = append(res, EcsDiscovered{
			Cluster: d.ecsClusterName,
			Service: &EcsService{
				Name: *ecsSvc.ServiceName,
				Tags: ParseTags(ecsSvc.Tags),
			},
		})
	}
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

		mesh := false
		for _, c := range task.Containers {
			if c.Name != nil && *c.Name == "ztunnel" {
				mesh = true
			}
		}

		res = append(res, EcsDiscovered{
			Cluster: d.ecsClusterName,
			Workload: &EcsWorkload{
				ARN:     *task.TaskArn,
				Group:   *task.Group,
				Zone:    *task.AvailabilityZone,
				Address: ip,
				Mesh:    mesh,
				Tags:    ParseTags(task.Tags),
			},
		})

	}
	return res, nil
}

func (d *ECSClusterDiscovery) getTasks(ctx context.Context) ([]ecstypes.Task, error) {
	req := &ecs.ListTasksInput{Cluster: &d.ecsClusterName}
	res := []ecstypes.Task{}
	pg := ecs.NewListTasksPaginator(d.client, req)
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		arns := page.TaskArns
		// We can only describe 100 at a time...
		for i := 0; i < len(arns); i += 100 {
			end := min(100, len(arns[i:]))
			seq := arns[i : i+end]
			desc := &ecs.DescribeTasksInput{
				Tasks:   seq,
				Cluster: &d.ecsClusterName,
				// Mark we want to include tags
				Include: []ecstypes.TaskField{ecstypes.TaskFieldTags},
			}
			o, err := d.client.DescribeTasks(ctx, desc)
			if err != nil {
				return nil, err
			}
			res = append(res, o.Tasks...)
		}
	}
	return res, nil
}

func (d *ECSClusterDiscovery) getServices(ctx context.Context) ([]ecstypes.Service, error) {
	req := &ecs.ListServicesInput{Cluster: &d.ecsClusterName}
	res := []ecstypes.Service{}
	pg := ecs.NewListServicesPaginator(d.client, req)
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		arns := page.ServiceArns
		// We can only describe 10 at a time
		for i := 0; i < len(arns); i += 10 {
			end := min(10, len(arns[i:]))
			seq := arns[i : i+end]
			desc := &ecs.DescribeServicesInput{
				Services: seq,
				Cluster:  &d.ecsClusterName,
				// Mark we want to include tags
				Include: []ecstypes.ServiceField{ecstypes.ServiceFieldTags},
			}
			o, err := d.client.DescribeServices(ctx, desc)
			if err != nil {
				return nil, err
			}
			res = append(res, o.Services...)
		}
	}
	return res, nil
}
