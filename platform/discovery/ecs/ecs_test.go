// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"

	"istio.io/api/annotation"
	networking "istio.io/api/networking/v1alpha3"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
)

// TODO: test rate limit?

var (
	TestAccountID1   = "111111111111"
	TestAccountID2   = "222222222222"
	TestRoleAccount1 = "arn:aws:iam::111111111111:role/istiod-role"
	TestRoleAccount2 = "arn:aws:iam::222222222222:role/istiod-role"
	TestAccount1     = ecsAccount{
		role:   TestRoleAccount1,
		domain: "one.test",
	}
	TestAccount2 = ecsAccount{
		role:   TestRoleAccount2,
		domain: "two.test",
	}
	TestCluster1 = "cluster1"
	TestCluster2 = "cluster2"
	TestRegion   = "us-west-2"
)

type TestECSInfo struct {
	role        string
	domain      string
	accountID   string
	clusterName string
}

var (
	TestServiceA = func(t TestECSInfo) types.Service {
		return types.Service{
			ClusterArn:  ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:cluster/%s", t.accountID, t.clusterName)),
			ServiceArn:  ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:service/%s/a", t.accountID, t.clusterName)),
			ServiceName: ptr.Of("a"),
		}
	}
	TestServiceAEntry = func(t TestECSInfo) *clientnetworking.ServiceEntry {
		arn := fmt.Sprintf("arn:aws:ecs:us-west-2:%s:service/%s/a", t.accountID, t.clusterName)
		return &clientnetworking.ServiceEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("ecs-service-%s-%s-%s-%s", t.accountID, TestRegion, t.clusterName, "a"),
				Namespace: "istio-system",
				Labels: map[string]string{
					ServiceLabel: "a",
				},
				Annotations: map[string]string{
					ARNAnnotation:          arn,
					DiscoveredByAnnotation: t.role,
					ClusterAnnotation:      t.clusterName,
					ResourceAnnotation:     "service/istio-system/" + arn,
				},
			},
			Spec: networking.ServiceEntry{
				Hosts:      []string{fmt.Sprintf("a.%s.%s", t.clusterName, t.domain)},
				Ports:      defaultPorts,
				Resolution: networking.ServiceEntry_STATIC,
				WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{
					HostnameLabel: fmt.Sprintf("a.%s.%s", t.clusterName, t.domain),
				}},
			},
		}
	}
	TestServiceB = func(t TestECSInfo) types.Service {
		return types.Service{
			ClusterArn:  ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:cluster/%s", t.accountID, t.clusterName)),
			ServiceArn:  ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:service/%s/b", t.accountID, t.clusterName)),
			ServiceName: ptr.Of("b"),
			Tags: []types.Tag{{
				Key:   ptr.Of(ServicePortsTag),
				Value: ptr.Of("http:80:8080"),
			}},
		}
	}
	TestServiceBEntry = func(t TestECSInfo) *clientnetworking.ServiceEntry {
		arn := fmt.Sprintf("arn:aws:ecs:us-west-2:%s:service/%s/b", t.accountID, t.clusterName)
		return &clientnetworking.ServiceEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("ecs-service-%s-%s-%s-%s", t.accountID, TestRegion, t.clusterName, "b"),
				Namespace: "istio-system",
				Labels:    map[string]string{ServiceLabel: "b"},
				Annotations: map[string]string{
					ARNAnnotation:          arn,
					DiscoveredByAnnotation: t.role,
					ClusterAnnotation:      t.clusterName,
					ResourceAnnotation:     "service/istio-system/" + arn,
				},
			},
			Spec: networking.ServiceEntry{
				Hosts: []string{fmt.Sprintf("b.%s.%s", t.clusterName, t.domain)},
				Ports: []*networking.ServicePort{{
					Number:     80,
					Protocol:   "HTTP",
					Name:       "port-0",
					TargetPort: 8080,
				}},
				Resolution: networking.ServiceEntry_STATIC,
				WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{
					HostnameLabel: fmt.Sprintf("b.%s.%s", t.clusterName, t.domain),
				}},
			},
		}
	}
	TestServiceCrossNamespace = func(t TestECSInfo) types.Service {
		return types.Service{
			ClusterArn:  ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:cluster/%s", t.accountID, t.clusterName)),
			ServiceArn:  ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:service/%s/cross-ns", t.accountID, t.clusterName)),
			ServiceName: ptr.Of("cross-ns"),
			Tags: []types.Tag{{
				Key:   ptr.Of(NamespaceTag),
				Value: ptr.Of("other-ns"),
			}},
		}
	}
	TestServiceCrossNamespaceEntry = func(t TestECSInfo) *clientnetworking.ServiceEntry {
		arn := fmt.Sprintf("arn:aws:ecs:us-west-2:%s:service/%s/cross-ns", t.accountID, t.clusterName)
		return &clientnetworking.ServiceEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("ecs-service-%s-%s-%s-%s", t.accountID, TestRegion, t.clusterName, "cross-ns"),
				Namespace: "other-ns",
				Labels: map[string]string{
					ServiceLabel: "cross-ns",
					NamespaceTag: "other-ns",
				},
				Annotations: map[string]string{
					ARNAnnotation:          arn,
					DiscoveredByAnnotation: t.role,
					ClusterAnnotation:      t.clusterName,
					ResourceAnnotation:     "service/other-ns/" + arn,
				},
			},
			Spec: networking.ServiceEntry{
				Hosts:      []string{fmt.Sprintf("cross-ns.%s.%s", t.clusterName, t.domain)},
				Ports:      defaultPorts,
				Resolution: networking.ServiceEntry_STATIC,
				WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{
					HostnameLabel: fmt.Sprintf("cross-ns.%s.%s", t.clusterName, t.domain),
				}},
			},
		}
	}
	TestTaskA1 = func(t TestECSInfo) types.Task {
		return types.Task{
			ClusterArn: ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:cluster/%s", t.accountID, t.clusterName)),
			TaskArn:    ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:task/%s/a1", t.accountID, t.clusterName)),
			Group:      ptr.Of("service:a"),
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
	}
	TestTaskA1WorkloadEntry = func(t TestECSInfo) *clientnetworking.WorkloadEntry {
		arn := fmt.Sprintf("arn:aws:ecs:us-west-2:%s:task/%s/a1", t.accountID, t.clusterName)
		return &clientnetworking.WorkloadEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("ecs-task-%s-%s-%s-%s", t.accountID, TestRegion, t.clusterName, "a1"),
				Namespace: "istio-system",
				Labels: map[string]string{
					HostnameLabel:     fmt.Sprintf("a.%s.%s", t.clusterName, t.domain),
					ServiceLabel:      "a",
					WorkloadNameLabel: "a",
				},
				Annotations: map[string]string{
					ARNAnnotation:                      arn,
					annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
					DiscoveredByAnnotation:             t.role,
					ClusterAnnotation:                  t.clusterName,
					ResourceAnnotation:                 "task/istio-system/" + arn,
				},
			},
			Spec: networking.WorkloadEntry{
				Address:        "240.240.240.1",
				ServiceAccount: "default",
				Locality:       "us-west-2/us-west-2a",
			},
		}
	}
	TestTaskCross1 = func(t TestECSInfo) types.Task {
		return types.Task{
			ClusterArn: ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:cluster/%s", t.accountID, t.clusterName)),
			TaskArn:    ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:task/%s/cross1", t.accountID, t.clusterName)),
			Group:      ptr.Of("service:cross-ns"),
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
				Key:   ptr.Of(NamespaceTag),
				Value: ptr.Of("other-ns"),
			}},
		}
	}
	TestTaskCross1WorkloadEntry = func(t TestECSInfo) *clientnetworking.WorkloadEntry {
		arn := fmt.Sprintf("arn:aws:ecs:us-west-2:%s:task/%s/cross1", t.accountID, t.clusterName)
		return &clientnetworking.WorkloadEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("ecs-task-%s-%s-%s-%s", t.accountID, TestRegion, t.clusterName, "cross1"),
				Namespace: "other-ns",
				Labels: map[string]string{
					HostnameLabel:     fmt.Sprintf("cross-ns.%s.%s", t.clusterName, t.domain),
					ServiceLabel:      "cross-ns",
					NamespaceTag:      "other-ns",
					WorkloadNameLabel: "cross-ns",
				},
				Annotations: map[string]string{
					ARNAnnotation:                      arn,
					annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
					DiscoveredByAnnotation:             t.role,
					ClusterAnnotation:                  t.clusterName,
					ResourceAnnotation:                 "task/other-ns/" + arn,
				},
			},
			Spec: networking.WorkloadEntry{
				Address:        "240.240.240.2",
				ServiceAccount: "default",
				Locality:       "us-west-2/us-west-2b",
			},
		}
	}
	TestTaskNonService = func(t TestECSInfo) types.Task {
		return types.Task{
			ClusterArn: ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:cluster/%s", t.accountID, t.clusterName)),
			TaskArn:    ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:task/%s/nonservice", t.accountID, t.clusterName)),
			Group:      ptr.Of("family:taskgroup"),
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
	}
	TestTaskNonServiceWorkloadEntry = func(t TestECSInfo) *clientnetworking.WorkloadEntry {
		arn := fmt.Sprintf("arn:aws:ecs:us-west-2:%s:task/%s/nonservice", t.accountID, t.clusterName)
		return &clientnetworking.WorkloadEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("ecs-task-%s-%s-%s-%s", t.accountID, TestRegion, t.clusterName, "nonservice"),
				Namespace: "istio-system",
				Labels: map[string]string{
					HostnameLabel:     fmt.Sprintf("taskgroup.%s.%s", t.clusterName, t.domain),
					ServiceLabel:      "taskgroup",
					WorkloadNameLabel: "taskgroup",
				},
				Annotations: map[string]string{
					ARNAnnotation:                      arn,
					annotation.AmbientRedirection.Name: constants.AmbientRedirectionEnabled,
					DiscoveredByAnnotation:             t.role,
					ClusterAnnotation:                  t.clusterName,
					ResourceAnnotation:                 "task/istio-system/" + arn,
				},
			},
			Spec: networking.WorkloadEntry{
				Address:        "240.240.240.1",
				ServiceAccount: "default",
				Locality:       "us-west-2/us-west-2a",
			},
		}
	}
	TestCluster = func(t TestECSInfo, discoveryEnabled bool) types.Cluster {
		return types.Cluster{
			ClusterName: ptr.Of(t.clusterName),
			ClusterArn:  ptr.Of(fmt.Sprintf("arn:aws:ecs:us-west-2:%s:cluster/%s", t.accountID, t.clusterName)),
			Tags: []types.Tag{
				{
					Key:   ptr.Of(DiscoveryEnabledTag),
					Value: ptr.Of(strconv.FormatBool(discoveryEnabled)),
				},
			},
		}
	}
)

func TestECS(t *testing.T) {
	i := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}
	roles := []*ecsAccountDiscovery{
		{
			account: TestAccount1,
			client: NewFakeClient(
				[]types.Cluster{
					TestCluster(i, true),
				},
				[]types.Task{
					TestTaskA1(i),
				},
				[]types.Service{
					TestServiceA(i),
					TestServiceB(i),
				},
			),
		},
	}
	e := testSetup(t, roles)

	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{
		TestTaskA1WorkloadEntry(i),
	})
	assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
		TestServiceAEntry(i),
		TestServiceBEntry(i),
	})
}

func TestTags(t *testing.T) {
	i := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}
	service := TestServiceA(i)
	service.Tags = []types.Tag{
		{
			Key:   ptr.Of(HostnameTag),
			Value: ptr.Of("example.domain"),
		},
		{
			Key:   ptr.Of(ServicePortsTag),
			Value: ptr.Of("http:81:8081/tcp:9090"),
		},
		{
			Key:   ptr.Of(NamespaceTag),
			Value: ptr.Of("custom-namespace"),
		},
	}
	task := TestTaskA1(i)
	task.Tags = []types.Tag{
		{
			Key:   ptr.Of(AWSManagedServiceNameTag),
			Value: ptr.Of("example"),
		},
		{
			Key:   ptr.Of(HostnameTag),
			Value: ptr.Of("example.domain"),
		},
		{
			Key:   ptr.Of(ServiceAccountTag),
			Value: ptr.Of("custom-service-account"),
		},
	}
	roles := []*ecsAccountDiscovery{
		{
			account: TestAccount1,
			client: NewFakeClient(
				[]types.Cluster{
					TestCluster(i, true),
				},
				[]types.Task{
					task,
				},
				[]types.Service{
					service,
				},
			),
		},
	}
	workloadEntry := TestTaskA1WorkloadEntry(i)
	workloadEntry.Labels[HostnameLabel] = "example.domain"
	workloadEntry.Labels[ServiceAccountTag] = "custom-service-account"
	workloadEntry.Labels[ServiceLabel] = "example"
	workloadEntry.Labels[WorkloadNameLabel] = "example"
	workloadEntry.Spec.ServiceAccount = "custom-service-account"
	e := testSetup(t, roles)
	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{
		workloadEntry,
	})

	serviceEntry := TestServiceAEntry(i)
	serviceEntry.Spec.Hosts = []string{
		"example.domain",
	}
	serviceEntry.Labels[HostnameLabel] = "example.domain"
	serviceEntry.Labels[NamespaceTag] = "custom-namespace"
	serviceEntry.Annotations[ResourceAnnotation] = "service/custom-namespace/arn:aws:ecs:us-west-2:111111111111:service/cluster1/a"
	serviceEntry.Spec.WorkloadSelector = &networking.WorkloadSelector{
		Labels: map[string]string{
			HostnameLabel: "example.domain",
		},
	}
	serviceEntry.Spec.Ports = []*networking.ServicePort{
		{
			Number:     81,
			TargetPort: 8081,
			Protocol:   "HTTP",
			Name:       "port-0",
		},
		{
			Number:   9090,
			Protocol: "TCP",
			Name:     "port-1",
		},
	}
	serviceEntry.Namespace = "custom-namespace"
	assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
		serviceEntry,
	})
}

func TestCrossNamespace(t *testing.T) {
	i := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}
	roles := []*ecsAccountDiscovery{
		{
			account: TestAccount1,
			client: NewFakeClient(
				[]types.Cluster{
					TestCluster(i, true),
				},
				[]types.Task{
					TestTaskCross1(i),
				},
				[]types.Service{
					TestServiceCrossNamespace(i),
				},
			),
		},
	}
	e := testSetup(t, roles)

	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{
		TestTaskCross1WorkloadEntry(i),
	})
	assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
		TestServiceCrossNamespaceEntry(i),
	})
}

func TestNonService(t *testing.T) {
	i := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}
	// Task that is not a service
	roles := []*ecsAccountDiscovery{
		{
			account: TestAccount1,
			client: NewFakeClient(
				[]types.Cluster{
					TestCluster(i, true),
				},
				[]types.Task{
					TestTaskNonService(i),
				},
				[]types.Service{},
			),
		},
	}
	e := testSetup(t, roles)

	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{
		TestTaskNonServiceWorkloadEntry(i),
	})
	assert.EventuallyEqual(t, serviceEntryFetcher(e), nil)
}

func TestMulticluster(t *testing.T) {
	i1 := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}
	i2 := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster2,
	}
	roles := []*ecsAccountDiscovery{
		{
			account: TestAccount1,
			client: NewFakeClient(
				[]types.Cluster{
					TestCluster(i1, true),
					TestCluster(i2, true),
				},
				[]types.Task{
					TestTaskA1(i1),
					TestTaskA1(i2),
				},
				[]types.Service{
					TestServiceA(i1),
					TestServiceB(i1),
					TestServiceA(i2),
					TestServiceB(i2),
				},
			),
		},
	}
	e := testSetup(t, roles)

	assert.EventuallyEqual(t, workloadEntryFetcher(e),
		[]*clientnetworking.WorkloadEntry{
			TestTaskA1WorkloadEntry(i1),
			TestTaskA1WorkloadEntry(i2),
		})
	assert.EventuallyEqual(t, serviceEntryFetcher(e),
		[]*clientnetworking.ServiceEntry{
			TestServiceAEntry(i1),
			TestServiceBEntry(i1),
			TestServiceAEntry(i2),
			TestServiceBEntry(i2),
		})
}

func TestCrossAccount(t *testing.T) {
	i1 := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}
	i2 := TestECSInfo{
		role:        TestRoleAccount2,
		domain:      TestAccount2.domain,
		accountID:   TestAccountID2,
		clusterName: TestCluster1,
	}
	roles := []*ecsAccountDiscovery{
		{
			account: TestAccount1,
			client: NewFakeClient(
				[]types.Cluster{
					TestCluster(i1, true),
				},
				[]types.Task{
					TestTaskA1(i1),
				},
				[]types.Service{
					TestServiceA(i1),
					TestServiceB(i1),
				},
			),
		},
		{
			account: TestAccount2,
			client: NewFakeClient(
				[]types.Cluster{
					TestCluster(i2, true),
				},
				[]types.Task{
					TestTaskA1(i2),
				},
				[]types.Service{
					TestServiceA(i2),
					TestServiceB(i2),
				},
			),
		},
	}
	e := testSetup(t, roles)

	assert.EventuallyEqual(t, workloadEntryFetcher(e),
		[]*clientnetworking.WorkloadEntry{
			TestTaskA1WorkloadEntry(i1),
			TestTaskA1WorkloadEntry(i2),
		})
	assert.EventuallyEqual(t, serviceEntryFetcher(e),
		[]*clientnetworking.ServiceEntry{
			TestServiceAEntry(i1),
			TestServiceBEntry(i1),
			TestServiceAEntry(i2),
			TestServiceBEntry(i2),
		})
}

func TestECSDowntime(t *testing.T) {
	i := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}

	// Use a persistent kube client, but create new ECS clients various times. This simulates Istiod restarting, etc.
	fk := kube.NewFakeClient()
	overallStop := test.NewStop(t)

	t.Run("initial state", func(t *testing.T) {
		// TaskA, ServiceA and ServiceB
		roles := []*ecsAccountDiscovery{
			{
				account: TestAccount1,
				client: NewFakeClient(
					[]types.Cluster{
						TestCluster(i, true),
					},
					[]types.Task{
						TestTaskA1(i),
					},
					[]types.Service{
						TestServiceA(i),
						TestServiceB(i),
					},
				),
			},
		}
		e := newTestECS(fk, roles)
		fk.RunAndWait(overallStop)
		go e.Run(test.NewStop(t))
		assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{TestTaskA1WorkloadEntry(i)})
		assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
			TestServiceAEntry(i),
			TestServiceBEntry(i),
		})
	})
	t.Run("reconnect remove task a and service b", func(t *testing.T) {
		// TaskA, ServiceB missing
		roles := []*ecsAccountDiscovery{
			{
				account: TestAccount1,
				client: NewFakeClient(
					[]types.Cluster{
						TestCluster(i, true),
					},
					[]types.Task{},
					[]types.Service{
						TestServiceA(i),
					},
				),
			},
		}
		e := newTestECS(fk, roles)
		go e.Run(test.NewStop(t))
		assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{})
		assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{TestServiceAEntry(i)})
	})
	t.Run("reconnect add service b, remove service a", func(t *testing.T) {
		// ServiceA removed, ServiceB added back
		roles := []*ecsAccountDiscovery{
			{
				account: TestAccount1,
				client: NewFakeClient(
					[]types.Cluster{
						TestCluster(i, true),
					},
					[]types.Task{},
					[]types.Service{
						TestServiceB(i),
					},
				),
			},
		}
		e := newTestECS(fk, roles)
		go e.Run(test.NewStop(t))
		assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
			TestServiceBEntry(i),
		})
	})
}

func TestECSDisabledCluster(t *testing.T) {
	i := TestECSInfo{
		role:        TestRoleAccount1,
		domain:      TestAccount1.domain,
		accountID:   TestAccountID1,
		clusterName: TestCluster1,
	}
	client := &fakeEcsClient{
		mu:      sync.RWMutex{},
		limiter: rate.NewLimiter(ECSRateLimit, ECSBurstLimit),
		clusters: []types.Cluster{
			TestCluster(i, true),
		},
		tasks: []types.Task{
			TestTaskA1(i),
		},
		services: []types.Service{
			TestServiceA(i),
			TestServiceB(i),
		},
	}

	// discovery for cluster is enabled at first
	roles := []*ecsAccountDiscovery{
		{
			account: TestAccount1,
			client:  client,
		},
	}
	e := testSetup(t, roles)
	assert.EventuallyEqual(t, workloadEntryFetcher(e), []*clientnetworking.WorkloadEntry{
		TestTaskA1WorkloadEntry(i),
	})
	assert.EventuallyEqual(t, serviceEntryFetcher(e), []*clientnetworking.ServiceEntry{
		TestServiceAEntry(i),
		TestServiceBEntry(i),
	})

	// disable discovery for cluster and check that all resources are removed
	client.SetClusters([]types.Cluster{TestCluster(i, false)})
	assert.EventuallyEqual(t, workloadEntryFetcher(e), nil)
	assert.EventuallyEqual(t, serviceEntryFetcher(e), nil)
}

func networkIDCallback(endpointIP string, labels labels.Instance) network.ID {
	return ""
}

func testSetup(t *testing.T, roles []*ecsAccountDiscovery) *ECSDiscovery {
	fk := kube.NewFakeClient()
	e := newTestECS(fk, roles)
	stop := test.NewStop(t)
	fk.RunAndWait(stop)
	go e.Run(stop)
	return e
}

func newTestECS(fk kube.Client, roles []*ecsAccountDiscovery) *ECSDiscovery {
	ads := make(map[string]*ecsAccountDiscovery)
	for _, ad := range roles {
		ad.lookupNetwork = networkIDCallback
		ad.clusters = map[arn.ARN]*ecsClusterDiscovery{}
		ad.serviceEntries = kclient.New[*clientnetworking.ServiceEntry](fk)
		ad.workloadEntries = kclient.New[*clientnetworking.WorkloadEntry](fk)
		ad.queue = controllers.NewQueue(fmt.Sprintf("ecs-%s", ad.account.role),
			controllers.WithReconciler(ad.Reconcile),
			controllers.WithMaxAttempts(25))
		ads[ad.account.role] = ad
	}

	return &ECSDiscovery{
		accounts:   ads,
		kubeClient: fk,
	}
}

func workloadEntryFetcher(e *ECSDiscovery) func() []*clientnetworking.WorkloadEntry {
	return func() []*clientnetworking.WorkloadEntry {
		we := []*clientnetworking.WorkloadEntry{}
		for _, r := range e.accounts {
			we = append(we, r.workloadEntries.List(metav1.NamespaceAll, klabels.Everything())...)
			// since all roles share the same kube client they will all have the same entries
			break
		}
		slices.SortBy(we, (*clientnetworking.WorkloadEntry).GetName)
		return we
	}
}

func serviceEntryFetcher(e *ECSDiscovery) func() []*clientnetworking.ServiceEntry {
	return func() []*clientnetworking.ServiceEntry {
		se := []*clientnetworking.ServiceEntry{}
		for _, r := range e.accounts {
			se = append(se, r.serviceEntries.List(metav1.NamespaceAll, klabels.Everything())...)
			// since all roles share the same kube client they will all have the same entries
			break
		}
		slices.SortBy(se, (*clientnetworking.ServiceEntry).GetName)
		return se
	}
}
