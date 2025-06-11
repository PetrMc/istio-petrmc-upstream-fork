// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"

	"istio.io/api/annotation"
	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
)

var (
	ServiceA = types.Service{
		ServiceArn:  ptr.Of(testArn("service", "a")),
		ServiceName: ptr.Of("a"),
		Tags: []types.Tag{{
			Key:   ptr.Of(ServiceAccountTag),
			Value: ptr.Of("sa-a"),
		}},
	}
	ServiceAEntry = &clientnetworking.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ecs-a",
			Namespace: "ecs",
			Labels: map[string]string{
				ServiceLabel:      "a",
				ServiceAccountTag: "sa-a",
			},
			Annotations: map[string]string{
				ResourceAnnotation: "service/ecs/a",
			},
		},
		Spec: networking.ServiceEntry{
			Hosts:            []string{"a.ecs.local"},
			Ports:            defaultPorts,
			Resolution:       networking.ServiceEntry_STATIC,
			WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{ServiceLabel: "a"}},
		},
	}
	ServiceB = types.Service{
		ServiceArn:  ptr.Of(testArn("service", "b")),
		ServiceName: ptr.Of("b"),
		Tags: []types.Tag{{
			Key:   ptr.Of(ServicePortsTag),
			Value: ptr.Of("http:80:8080"),
		}},
	}
	ServiceBEntry = &clientnetworking.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ecs-b",
			Namespace: "ecs",
			Labels:    map[string]string{ServiceLabel: "b"},
			Annotations: map[string]string{
				ResourceAnnotation: "service/ecs/b",
			},
		},
		Spec: networking.ServiceEntry{
			Hosts: []string{"b.ecs.local"},
			Ports: []*networking.ServicePort{{
				Number:     80,
				Protocol:   "HTTP",
				Name:       "port-0",
				TargetPort: 8080,
			}},
			Resolution:       networking.ServiceEntry_STATIC,
			WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{ServiceLabel: "b"}},
		},
	}
	ServiceCrossNamespace = types.Service{
		ServiceArn:  ptr.Of(testArn("service", "cross-ns")),
		ServiceName: ptr.Of("cross-ns"),
		Tags: []types.Tag{{
			Key:   ptr.Of(ServiceNamespaceTag),
			Value: ptr.Of("other-ns"),
		}},
	}
	ServiceCrossNamespaceEntry = &clientnetworking.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ecs-cross-ns",
			Namespace: "other-ns",
			Labels: map[string]string{
				ServiceLabel:        "cross-ns",
				ServiceNamespaceTag: "other-ns",
			},
			Annotations: map[string]string{
				ResourceAnnotation: "service/other-ns/cross-ns",
			},
		},
		Spec: networking.ServiceEntry{
			Hosts:            []string{"cross-ns.ecs.local"},
			Ports:            defaultPorts,
			Resolution:       networking.ServiceEntry_STATIC,
			WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{ServiceLabel: "cross-ns"}},
		},
	}
	TaskA1 = types.Task{
		TaskArn: ptr.Of(testArn("task", "a1")),
		Group:   ptr.Of("service:a"),
		Containers: []types.Container{
			{},
			{Name: ptr.Of("ztunnel")},
		},
		Attachments: []types.Attachment{{
			Details: []types.KeyValuePair{{
				Name:  ptr.Of("privateIPv4Address"),
				Value: ptr.Of("240.240.240.1"),
			}},
		}},
		AvailabilityZone: ptr.Of("us-west-2a"),
	}
	TaskA1WorkloadEntry = &clientnetworking.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ecs-a-a1",
			Namespace: "ecs",
			Labels: map[string]string{
				ServiceLabel:                     "a",
				"service.istio.io/workload-name": "a",
			},
			Annotations: map[string]string{
				annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
				ARNAnnotation:                      testArn("task", "a1"),
				ResourceAnnotation:                 "workload/ecs/a/" + testArn("task", "a1"),
			},
		},
		Spec: networking.WorkloadEntry{
			Address:        "240.240.240.1",
			ServiceAccount: "sa-a",
			Locality:       "us-west-2/us-west-2a",
		},
	}
	TaskCross1 = types.Task{
		TaskArn: ptr.Of(testArn("task", "cross1")),
		Group:   ptr.Of("service:cross-ns"),
		Containers: []types.Container{
			{},
			{Name: ptr.Of("ztunnel")},
		},
		Attachments: []types.Attachment{{
			Details: []types.KeyValuePair{{
				Name:  ptr.Of("privateIPv4Address"),
				Value: ptr.Of("240.240.240.2"),
			}},
		}},
		AvailabilityZone: ptr.Of("us-west-2b"),
		Tags: []types.Tag{{
			Key:   ptr.Of(ServiceNamespaceTag),
			Value: ptr.Of("other-ns"),
		}},
	}
	TaskCross1WorkloadEntry = &clientnetworking.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ecs-cross-ns-cross1",
			Namespace: "other-ns",
			Labels: map[string]string{
				ServiceLabel:                     "cross-ns",
				ServiceNamespaceTag:              "other-ns",
				"service.istio.io/workload-name": "cross-ns",
			},
			Annotations: map[string]string{
				annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
				ARNAnnotation:                      testArn("task", "cross1"),
				ResourceAnnotation:                 "workload/other-ns/cross-ns/" + testArn("task", "cross1"),
			},
		},
		Spec: networking.WorkloadEntry{
			Address:  "240.240.240.2",
			Locality: "us-west-2/us-west-2b",
		},
	}
)

func TestECS(t *testing.T) {
	e := testSetup(t, []types.Task{TaskA1}, []types.Service{ServiceA, ServiceB})

	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{TaskA1WorkloadEntry})
	assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
		ServiceAEntry,
		ServiceBEntry,
	})
}

func TestCrossNamespace(t *testing.T) {
	e := testSetup(t, []types.Task{TaskCross1}, []types.Service{ServiceCrossNamespace})

	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{TaskCross1WorkloadEntry})
	assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
		ServiceCrossNamespaceEntry,
	})
}

func TestNonService(t *testing.T) {
	// Task that is not a service
	e := testSetup(t, []types.Task{{
		TaskArn: ptr.Of(testArn("task", "task123")),
		Group:   ptr.Of("family:taskgroup"),
		Containers: []types.Container{
			{},
			{Name: ptr.Of("ztunnel")},
		},
		Attachments: []types.Attachment{{
			Details: []types.KeyValuePair{{
				Name:  ptr.Of("privateIPv4Address"),
				Value: ptr.Of("240.240.240.1"),
			}},
		}},
		AvailabilityZone: ptr.Of("us-west-2a"),
	}}, []types.Service{})

	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ecs-taskgroup-task123",
			Namespace: "ecs",
			Labels: map[string]string{
				ServiceLabel:                     "",
				"service.istio.io/workload-name": "taskgroup",
			},
			Annotations: map[string]string{
				annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
				ARNAnnotation:                      testArn("task", "task123"),
				ResourceAnnotation:                 "workload/ecs/taskgroup/" + testArn("task", "task123"),
			},
		},
		Spec: networking.WorkloadEntry{
			Address:  "240.240.240.1",
			Locality: "us-west-2/us-west-2a",
		},
	}})
	assert.EventuallyEqual(t, serviceEntryFetcher(e), nil)
}

func networkIDCallback(endpointIP string, labels labels.Instance) network.ID {
	return ""
}

func TestECSDowntime(t *testing.T) {
	fk := kube.NewFakeClient()
	overallStop := test.NewStop(t)

	// Use a persistent kube client, but create new ECS clients various times. This simulates Istiod restarting, etc.
	t.Run("initial state", func(t *testing.T) {
		fe := fakeEcsClient{
			Services: []types.Service{ServiceA, ServiceB},
		}
		e := newECS(fk, fe, NewDynamicPoller(time.Second, time.Second), networkIDCallback)
		fk.RunAndWait(overallStop)
		go e.Run(test.NewStop(t))
		assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{ServiceAEntry, ServiceBEntry})
	})
	t.Run("reconnect remove", func(t *testing.T) {
		fe := fakeEcsClient{
			// ServiceB missing
			Services: []types.Service{ServiceA},
		}
		e := newECS(fk, fe, NewDynamicPoller(time.Second, time.Second), networkIDCallback)
		go e.Run(test.NewStop(t))
		assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{ServiceAEntry})
	})
	t.Run("reconnect add", func(t *testing.T) {
		fe := fakeEcsClient{
			// ServiceA removed, ServiceB added back
			Services: []types.Service{ServiceB},
		}
		e := newECS(fk, fe, NewDynamicPoller(time.Second, time.Second), networkIDCallback)
		go e.Run(test.NewStop(t))
		assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{ServiceBEntry})
	})
}

func testSetup(t *testing.T, tasks []types.Task, services []types.Service) *ECSDiscovery {
	fk := kube.NewFakeClient()
	fe := fakeEcsClient{
		Tasks:    tasks,
		Services: services,
	}
	e := newECS(fk, fe, NewDynamicPoller(time.Second, time.Second), networkIDCallback)
	stop := test.NewStop(t)
	fk.RunAndWait(stop)
	go e.Run(stop)
	return e
}

func workloadEntryFetcher(e *ECSDiscovery) func() []*clientnetworking.WorkloadEntry {
	return func() []*clientnetworking.WorkloadEntry {
		we := e.workloadEntries.List(metav1.NamespaceAll, klabels.Everything())
		slices.SortBy(we, (*clientnetworking.WorkloadEntry).GetName)
		return we
	}
}

func serviceEntryFetcher(e *ECSDiscovery) func() []*clientnetworking.ServiceEntry {
	return func() []*clientnetworking.ServiceEntry {
		se := e.serviceEntries.List(metav1.NamespaceAll, klabels.Everything())
		slices.SortBy(se, (*clientnetworking.ServiceEntry).GetName)
		return se
	}
}

type fakeEcsClient struct {
	Tasks    []types.Task
	Services []types.Service
}

func (f fakeEcsClient) ListTasks(ctx context.Context, input *ecs.ListTasksInput, f2 ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	return &ecs.ListTasksOutput{
		NextToken: nil,
		TaskArns: slices.Map(f.Tasks, func(e types.Task) string {
			return *e.TaskArn
		}),
	}, nil
}

func (f fakeEcsClient) DescribeTasks(ctx context.Context, input *ecs.DescribeTasksInput, f2 ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	return &ecs.DescribeTasksOutput{
		Tasks: slices.MapFilter(input.Tasks, func(e string) *types.Task {
			return slices.FindFunc(f.Tasks, func(task types.Task) bool {
				return *task.TaskArn == e
			})
		}),
	}, nil
}

func (f fakeEcsClient) ListServices(ctx context.Context, input *ecs.ListServicesInput, f2 ...func(*ecs.Options)) (*ecs.ListServicesOutput, error) {
	return &ecs.ListServicesOutput{
		NextToken: nil,
		ServiceArns: slices.Map(f.Services, func(e types.Service) string {
			return *e.ServiceArn
		}),
	}, nil
}

func (f fakeEcsClient) DescribeServices(ctx context.Context, input *ecs.DescribeServicesInput, f2 ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	return &ecs.DescribeServicesOutput{
		Services: slices.MapFilter(input.Services, func(e string) *types.Service {
			return slices.FindFunc(f.Services, func(task types.Service) bool {
				return *task.ServiceArn == e
			})
		}),
	}, nil
}

func testArn(typ, name string) string {
	return fmt.Sprintf("arn:aws:ecs:us-west-2:111111111111:%v/some-cluster/%v", typ, name)
}
