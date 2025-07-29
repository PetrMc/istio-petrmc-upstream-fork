// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peering_test

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "sigs.k8s.io/gateway-api/apis/v1"
	k8sbeta "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/api/label"
	networking "istio.io/api/networking/v1alpha3"
	networkingclient "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	pilotstatus "istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pilot/test/xds"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/backoff"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/platform/discovery/peering"
)

func TestPeering(t *testing.T) {
	setup := func(t test.Failer) (*Cluster, *Cluster) {
		c1 := NewCluster(t, "c1", "c1")
		c2 := NewCluster(t, "c2", "c2")
		c1.ConnectTo(c2)
		return c1, c2
	}

	ports1 := []corev1.ServicePort{
		{
			Port:     80,
			Protocol: "TCP",
			Name:     "http",
		},
		{
			Port:       81,
			Name:       "81",
			Protocol:   "TCP",
			TargetPort: intstr.FromInt(8081),
		},
		{
			Port:       82,
			Name:       "82",
			Protocol:   "TCP",
			TargetPort: intstr.FromString("target-821"),
		},
		{
			Port:     91,
			Protocol: "TCP",
			Name:     "extra1",
		},
	}
	ports2 := []corev1.ServicePort{
		{
			Port:     80,
			Protocol: "TCP",
			Name:     "http",
		},
		{
			Port:       81,
			Name:       "81",
			Protocol:   "TCP",
			TargetPort: intstr.FromInt(8082),
		},
		{
			Port:       82,
			Name:       "82",
			Protocol:   "TCP",
			TargetPort: intstr.FromString("target-822"),
		},
		{
			Port:     92,
			Protocol: "TCP",
			Name:     "extra2",
		},
	}

	defaultSvc1Name := "autogen.default.svc1"
	defaultSvc2Name := "autogen.default.svc2"
	defaultSvc3Name := "autogen.default.svc3"
	c1Svc1Name := "autogen.c1.default.svc1"
	c2Svc1Name := "autogen.c2.default.svc1"
	c2Svc2Name := "autogen.c2.default.svc2"
	c2Svc3Name := "autogen.c2.default.svc3"

	t.Run("both exported", func(t *testing.T) {
		c1, c2 := setup(t)
		c1.CreateService("svc1", true, ports1)
		c2.CreateService("svc1", true, ports2)

		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})

		AssertWE(c2)
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c2, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8082},
			{Name: "target-822", Number: 82},
			{Name: "extra2", Number: 92},
		})
	})
	t.Run("local exported", func(t *testing.T) {
		c1, c2 := setup(t)
		c1.CreateService("svc1", true, ports1)
		c2.CreateService("svc1", false, ports2)

		AssertWE(c1)
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})

		AssertWE(c2)
		AssertSE(c2)
	})
	t.Run("local only", func(t *testing.T) {
		c1, c2 := setup(t)
		c1.CreateService("svc1", true, ports1)
		// No service in c2 at all

		AssertWE(c1)
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})

		AssertWE(c2)
		AssertSE(c2)
	})
	t.Run("remote only", func(t *testing.T) {
		c1, c2 := setup(t)
		// No service in c1 at all
		c2.CreateService("svc1", true, ports2)

		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "port-82": 82})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "port-80", Number: 80, Protocol: "TCP", TargetPort: 80},
			{Name: "port-81", Number: 81, Protocol: "TCP", TargetPort: 81},
			{Name: "port-82", Number: 82, Protocol: "TCP", TargetPort: 82},
			{Name: "port-92", Number: 92, Protocol: "TCP", TargetPort: 92},
		})

		AssertWE(c2)
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c2, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8082},
			{Name: "target-822", Number: 82},
			{Name: "extra2", Number: 92},
		})
	})
	t.Run("fully bidirection", func(t *testing.T) {
		c1, c2 := setup(t)
		c2.ConnectTo(c1)
		c1.CreateService("svc1", true, ports1)
		c2.CreateService("svc1", true, ports2)

		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})

		AssertWE(c2, DesiredWE{Name: c1Svc1Name, Locality: c1.Locality()})
		AssertWEPorts(c2, c1Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-91": 91, "target-822": 82})
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c2, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8082},
			{Name: "target-822", Number: 82},
			{Name: "extra2", Number: 92},
		})
	})
	t.Run("service accounts", func(t *testing.T) {
		c1, c2 := setup(t)
		c2.ConnectTo(c1)
		c1.CreateService("svc1", true, ports1)
		c1.CreateWorkload("svc1", "we1", "sa-1", nil)
		c1.CreateWorkload("svc1", "we2", "sa-1", nil)
		c1.CreateWorkload("svc1", "we3", "sa-2", nil)
		c2.CreateService("svc1", true, ports2)

		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c2, c1Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-91": 91, "target-822": 82})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})

		AssertWE(c2, DesiredWE{Name: c1Svc1Name, Locality: c1.Locality()})
		AssertWEPorts(c2, c1Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-91": 91, "target-822": 82})
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name, ServiceAccounts: []string{
			"spiffe://cluster.local/ns/default/sa/sa-1",
			"spiffe://cluster.local/ns/default/sa/sa-2",
		}})
		AssertSEPorts(c2, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8082},
			{Name: "target-822", Number: 82},
			{Name: "extra2", Number: 92},
		})
	})
	t.Run("add and remove gateway", func(t *testing.T) {
		c1, c2 := setup(t)
		c2.ConnectTo(c1)
		c1.CreateService("svc1", true, ports1)
		c2.CreateService("svc1", true, ports2)

		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})

		AssertWE(c2, DesiredWE{Name: c1Svc1Name, Locality: c1.Locality()})
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name})

		// Removal of gateway is a permanent drop, not an ephemeral one -- should cleanup resources.
		c2.DisconnectFrom(c1)
		AssertWE(c2)
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c2, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8082},
			{Name: "target-822", Number: 82},
			{Name: "extra2", Number: 92},
		})
	})
	t.Run("add and remove service", func(t *testing.T) {
		c1, c2 := setup(t)
		c2.ConnectTo(c1)
		c1.CreateService("svc1", true, ports1)
		c2.CreateService("svc1", true, ports2)

		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})

		AssertWE(c2, DesiredWE{Name: c1Svc1Name, Locality: c1.Locality()})
		AssertWEPorts(c2, c1Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-91": 91, "target-822": 82})
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c2, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8082},
			{Name: "target-822", Number: 82},
			{Name: "extra2", Number: 92},
		})

		// Delete from one cluster...
		c1.DeleteService("svc1")
		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-82": 82, "port-92": 92})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "port-80", Number: 80, Protocol: "TCP", TargetPort: 80},
			{Name: "port-81", Number: 81, Protocol: "TCP", TargetPort: 81},
			{Name: "port-82", Number: 82, Protocol: "TCP", TargetPort: 82},
			{Name: "port-92", Number: 92, Protocol: "TCP", TargetPort: 92},
		})

		AssertWE(c2)
		AssertSE(c2, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c2, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8082},
			{Name: "target-822", Number: 82},
			{Name: "extra2", Number: 92},
		})

		// Delete from other cluster...
		c2.DeleteService("svc1")
		AssertWE(c1)
		AssertSE(c1)

		AssertWE(c2)
		AssertSE(c2)
	})
	t.Run("disconnects", func(t *testing.T) {
		c1 := NewCluster(t, "c1", "c1")
		c2 := NewCluster(t, "c2", "c2")

		c1.ConnectTo(c2)
		c1.CreateService("svc1", true, ports1)

		AssertWE(c1)
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})

		// Add a service during disconnection
		c1.Outage.setOutage(true)
		c2.CreateService("svc1", true, ports2)
		c1.Outage.setOutage(false)
		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
	})
	t.Run("restarts", func(t *testing.T) {
		features.RemoteClusterTimeout = time.Millisecond * 100
		c1k := kube.NewFakeClient()
		initialStop := make(chan struct{})
		// Peering will stop when we trigger it, kube client will run for as long as the test
		c1 := newCluster(t, c1k, initialStop, false, "c1", "c1")
		fullStop := test.NewStop(t)
		c1k.RunAndWait(fullStop)

		// Initial run as normal
		c2 := NewCluster(t, "c2", "c2")
		c1.ConnectTo(c2)

		c1ports2 := []corev1.ServicePort{
			{
				Port:       2001,
				TargetPort: intstr.FromInt(2001),
				Protocol:   "TCP",
				Name:       "2001",
			},
		}
		c1ports3 := []corev1.ServicePort{
			{
				Port:       3001,
				TargetPort: intstr.FromInt(3001),
				Protocol:   "TCP",
				Name:       "3001",
			},
		}
		c2ports2 := []corev1.ServicePort{
			{
				Port:       2002,
				TargetPort: intstr.FromInt(2002),
				Protocol:   "TCP",
				Name:       "2002",
			},
		}
		c2ports3 := []corev1.ServicePort{
			{
				Port:       2003,
				TargetPort: intstr.FromInt(2003),
				Protocol:   "TCP",
				Name:       "2003",
			},
		}
		// 1-3 exist in c1
		// 1 and 2 exist in c2
		c1.CreateService("svc1", true, ports1)
		c1.CreateService("svc2", true, c1ports2)
		c1.CreateService("svc3", true, c1ports3)
		c2.CreateService("svc1", true, ports2)
		c2.CreateService("svc2", true, c2ports2)
		c2.CreateWorkload("svc1", "we1", "sa-1", nil)
		c2.CreateWorkload("svc1", "we2", "sa-1", nil)
		c2.CreateWorkload("svc1", "we3", "sa-2", nil)

		// we have pointers for the 2 services that exist remotely
		AssertWE(
			c1,
			DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()},
			DesiredWE{Name: c2Svc2Name, Locality: c2.Locality()},
		)
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertWEPorts(c1, c2Svc2Name, map[string]uint32{"port-2002": 2002})
		// svc1 has some remote workloads so should have those SAs included
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name, ServiceAccounts: []string{
			"spiffe://cluster.local/ns/default/sa/sa-1",
			"spiffe://cluster.local/ns/default/sa/sa-2",
		}}, DesiredSE{Name: defaultSvc2Name}, DesiredSE{Name: defaultSvc3Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})
		AssertSEPorts(c1, defaultSvc2Name, []*networking.ServicePort{
			{Name: "2001", Number: 2001, TargetPort: 2001},
		})
		AssertSEPorts(c1, defaultSvc3Name, []*networking.ServicePort{
			{Name: "3001", Number: 3001, TargetPort: 3001},
		})

		// c1 "restarts"
		close(initialStop)
		kube.ResetInformerTrick(c1k)

		secondStop := make(chan struct{})
		// c1 reconnects, but with a simulated an outage against c2
		c1 = newCluster(t, c1k, secondStop, true, "c1", "c1")
		c1k.RunAndWait(fullStop)
		// We should NOT clean things up
		AssertWE(
			c1,
			DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()},
			DesiredWE{Name: "autogen.c2.default.svc2", Locality: c2.Locality()},
		)
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertWEPorts(c1, c2Svc2Name, map[string]uint32{"port-2002": 2002})
		AssertSE(c1, DesiredSE{Name: "autogen.default.svc1", ServiceAccounts: []string{
			"spiffe://cluster.local/ns/default/sa/sa-1",
			"spiffe://cluster.local/ns/default/sa/sa-2",
		}}, DesiredSE{Name: "autogen.default.svc2"}, DesiredSE{Name: "autogen.default.svc3"})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})
		AssertSEPorts(c1, defaultSvc2Name, []*networking.ServicePort{
			{Name: "2001", Number: 2001, TargetPort: 2001},
		})
		AssertSEPorts(c1, defaultSvc3Name, []*networking.ServicePort{
			{Name: "3001", Number: 3001, TargetPort: 3001},
		})

		// While disconnected, make some changes
		c2.DeleteService("svc2")
		c2.CreateService("svc3", true, c2ports3)
		c2.DeleteWorkload("we3")

		// Reconnect... we should see the changes
		c1.Outage.setOutage(false)
		// svc2 removed, svc3 added
		AssertWE(
			c1,
			DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()},
			DesiredWE{Name: c2Svc3Name, Locality: c2.Locality()},
		)
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertWEPorts(c1, c2Svc3Name, map[string]uint32{"port-2003": 2003})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name, ServiceAccounts: []string{
			"spiffe://cluster.local/ns/default/sa/sa-1",
		}}, DesiredSE{Name: "autogen.default.svc2"}, DesiredSE{Name: "autogen.default.svc3"})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})
		AssertSEPorts(c1, defaultSvc2Name, []*networking.ServicePort{
			{Name: "2001", Number: 2001, TargetPort: 2001},
		})
		AssertSEPorts(c1, defaultSvc3Name, []*networking.ServicePort{
			{Name: "3001", Number: 3001, TargetPort: 3001},
		})

		// c1 "restarts" again
		close(secondStop)
		kube.ResetInformerTrick(c1k)
		// c1 reconnects, but no longer connected
		c1.DisconnectFrom(c2)
		thirdStop := make(chan struct{})
		c1 = newCluster(t, c1k, thirdStop, false, "c1", "c1")
		c1k.RunAndWait(fullStop)
		// We should cleanup everything
		AssertWE(c1)
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name}, DesiredSE{Name: defaultSvc2Name}, DesiredSE{Name: defaultSvc3Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})
		AssertSEPorts(c1, defaultSvc2Name, []*networking.ServicePort{
			{Name: "2001", Number: 2001, TargetPort: 2001},
		})
		AssertSEPorts(c1, defaultSvc3Name, []*networking.ServicePort{
			{Name: "3001", Number: 3001, TargetPort: 3001},
		})
	})
	t.Run("local service changes", func(t *testing.T) {
		c1, c2 := setup(t)
		c1.CreateServiceLabel("svc1", peering.ServiceScopeGlobal, ports1)
		c2.CreateServiceLabel("svc1", peering.ServiceScopeGlobal, ports2)

		AssertWE(c1, DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()})
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		lbls := map[string]string{
			"app":                               "svc1",
			peering.ParentServiceLabel:          "svc1",
			peering.ServiceScopeLabel:           peering.ServiceScopeGlobal,
			peering.ParentServiceNamespaceLabel: "default",
			peering.SourceClusterLabel:          "c2",
			model.TunnelLabel:                   model.TunnelHTTP,
		}
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})
		AssertWELabels(c1, c2Svc1Name, lbls)
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})

		// Switch the label
		c1.CreateServiceLabel("svc1", peering.ServiceScopeGlobalOnly, ports1)
		lbls[peering.ServiceScopeLabel] = peering.ServiceScopeGlobalOnly
		AssertWELabels(c1, c2Svc1Name, lbls)
	})
	t.Run("merge service ports", func(t *testing.T) {
		ports3 := []corev1.ServicePort{
			{
				Port:     80,
				Protocol: "TCP",
				Name:     "http",
			},
			{
				Port:       81,
				Name:       "81",
				Protocol:   "TCP",
				TargetPort: intstr.FromInt(8083),
			},
			{
				Port:       82,
				Name:       "82",
				Protocol:   "TCP",
				TargetPort: intstr.FromString("target-823"),
			},
			{
				Port:     93,
				Protocol: "TCP",
				Name:     "extra3",
			},
		}
		c1, c2 := setup(t)
		c3 := NewCluster(t, "c3", "c3")
		c1.ConnectTo(c3)
		c2.CreateService("svc1", true, ports2)
		c3.CreateService("svc1", true, ports3)

		c3Svc1Name := "autogen.c3.default.svc1"

		AssertWE(
			c1,
			DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()},
			DesiredWE{Name: c3Svc1Name, Locality: c3.Locality()},
		)
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-82": 82, "port-92": 92})
		AssertWEPorts(c1, c3Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-82": 82, "port-93": 93})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "port-80", Number: 80, Protocol: "TCP", TargetPort: 80},
			{Name: "port-81", Number: 81, Protocol: "TCP", TargetPort: 81},
			{Name: "port-82", Number: 82, Protocol: "TCP", TargetPort: 82},
			{Name: "port-92", Number: 92, Protocol: "TCP", TargetPort: 92},
			{Name: "port-93", Number: 93, Protocol: "TCP", TargetPort: 93},
		})
		AssertWE(c2)
		AssertWE(c3)

		// c1 local
		c1.CreateService("svc1", false, ports1)
		AssertWE(
			c1,
			DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()},
			DesiredWE{Name: c3Svc1Name, Locality: c3.Locality()},
		)
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-82": 82, "port-92": 92})
		AssertWEPorts(c1, c3Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-82": 82, "port-93": 93})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "port-80", Number: 80, Protocol: "TCP", TargetPort: 80},
			{Name: "port-81", Number: 81, Protocol: "TCP", TargetPort: 81},
			{Name: "port-82", Number: 82, Protocol: "TCP", TargetPort: 82},
			{Name: "port-92", Number: 92, Protocol: "TCP", TargetPort: 92},
			{Name: "port-93", Number: 93, Protocol: "TCP", TargetPort: 93},
		})
		AssertWE(c2)
		AssertWE(c3)

		// c1 global
		c1.CreateService("svc1", true, ports1)
		AssertWE(
			c1,
			DesiredWE{Name: c2Svc1Name, Locality: c2.Locality()},
			DesiredWE{Name: c3Svc1Name, Locality: c3.Locality()},
		)
		AssertWEPorts(c1, c2Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-92": 92, "target-821": 82})
		AssertWEPorts(c1, c3Svc1Name, map[string]uint32{"port-80": 80, "port-81": 81, "port-93": 93, "target-821": 82})
		AssertSE(c1, DesiredSE{Name: defaultSvc1Name})
		AssertSEPorts(c1, defaultSvc1Name, []*networking.ServicePort{
			{Name: "http", Protocol: "HTTP", Number: 80},
			{Name: "81", Number: 81, TargetPort: 8081},
			{Name: "target-821", Number: 82},
			{Name: "extra1", Number: 91},
		})
		AssertWE(c2)
		AssertWE(c3)
	})
	t.Run("flat network", func(t *testing.T) {
		c1 := NewCluster(t, "c1", "net1")
		c2 := NewCluster(t, "c2", "net1")
		c3 := NewCluster(t, "c3", "net2")
		c1.ConnectTo(c2)
		c1.ConnectTo(c3)
		// No service in c1 at all
		// c2 is flat
		// c3 is cross-network
		c2.CreateService("svc1", true, nil)
		c2.CreatePod("svc1", "pod1", "1.2.3.4")
		c2.CreatePodWithLocality("svc1", "pod-locality", "1.2.3.5", "custom-region", "custom-zone")
		c3.CreateService("svc1", true, nil)
		c3.CreatePod("svc1", "pod2", "2.3.4.5")

		AssertWE(c1,
			// aggregated ew gw with gateway locality
			DesiredWE{Name: "autogen.c2.default.svc1", Locality: "region-c2/zone-c2"},
			// aggregated ew gw with gateway locality
			DesiredWE{Name: "autogen.c3.default.svc1", Locality: "region-c3/zone-c3"},
			// direct to pod with gateway locality
			DesiredWE{Name: "autogenflat.c2.default.pod1.f2396f15c5c2", Address: "1.2.3.4", Locality: "region-c2/zone-c2"},
			// direct to pod with custom locality
			DesiredWE{
				Name: "autogenflat.c2.default.pod-locality.f2396f15c5c2",
				Address: "1.2.3.5",
				Locality: "custom-region/custom-zone"
			},
		)
		AssertSE(c1, DesiredSE{Name: "autogen.default.svc1"})

		AssertWE(c2)
		AssertSE(c2, DesiredSE{Name: "autogen.default.svc1"})

		c1.Outage.setOutage(true)
		c2.DeletePod("pod1")
		c1.Outage.setOutage(false)
		AssertWE(c1,
			DesiredWE{Name: "autogen.c2.default.svc1", Locality: "region-c2/zone-c2"},
			DesiredWE{Name: "autogen.c3.default.svc1", Locality: "region-c3/zone-c3"},
			// pod remains with gateway locality
			DesiredWE{
				Name: "autogenflat.c2.default.pod-locality.f2396f15c5c2",
				Address: "1.2.3.5",
				Locality: "custom-region/custom-zone"
			},
		)
	})
}

func init() {
	// For tests
	peering.EnableAutomaticGatewayCreation = true
}

func TestAutomatedPeering(t *testing.T) {
	t.Run("creates and removes gme peer gateway", func(t *testing.T) {
		c1 := NewCluster(t, "c1", "c1")
		c2 := NewCluster(t, "c2", "c2")
		c1.CreateService("svc1", true, nil)

		ewGw := c1.CreateOrUpdateEastWestGateway(
			"istio-eastwest",
			constants.IstioSystemNamespace,
			nil,
		)

		generatedPeer := c1.GeneratedPeerGateway(ewGw)
		AssertPeerGateway(c1, generatedPeer)

		// simulate distribution of the generated peer gateway to c2 by GME which should start peering to c1
		// and create a WE and SE for c1's exported service
		c2.CreateGateway(generatedPeer)
		expectedGatewayStatus := k8s.GatewayStatus{
			Conditions: []metav1.Condition{
				{
					Type:               constants.SoloConditionPeeringSucceeded,
					Status:             metav1.ConditionTrue,
					Reason:             string(k8s.GatewayReasonProgrammed),
					LastTransitionTime: metav1.Now(),
				},
			},
		}
		AssertPeerGatewayStatus(c2, generatedPeer.GetName(), generatedPeer.GetNamespace(), expectedGatewayStatus)

		AssertWE(c1)
		AssertSE(c1, DesiredSE{Name: "autogen.default.svc1"})

		AssertWE(c2, DesiredWE{Name: "autogen.c1.default.svc1"})
		AssertSE(c2, DesiredSE{Name: "autogen.default.svc1"})

		// remove the east-west gateway from c1 which should remove the generated peer gateway from c1
		c1.DeleteGateway("istio-eastwest", constants.IstioSystemNamespace)
		AssertPeerGateway(c1)
	})
	t.Run("writes Programmed: Unknown status when network is not synced", func(t *testing.T) {
		c1 := NewCluster(t, "c1", "n1")
		c2 := NewCluster(t, "c2", "n2")
		ewGw := c1.CreateOrUpdateEastWestGateway(
			"istio-eastwest",
			constants.IstioSystemNamespace,
			nil,
		)

		generatedPeer := c1.GeneratedPeerGateway(ewGw)
		AssertPeerGateway(c1, generatedPeer)

		generatedPeer.Spec.Addresses[0].Value = "1.1.1.1"
		c2.CreateGateway(generatedPeer)
		expectedGatewayStatus := k8s.GatewayStatus{
			Conditions: []metav1.Condition{
				{
					Type:               constants.SoloConditionPeeringSucceeded,
					Status:             metav1.ConditionUnknown,
					Reason:             string(k8s.GatewayReasonPending),
					Message:            "network not yet synced check istiod log for more details",
					LastTransitionTime: metav1.Now(),
				},
			},
		}
		AssertPeerGatewayStatus(c2, generatedPeer.GetName(), generatedPeer.GetNamespace(), expectedGatewayStatus)
	})
	t.Run("writes Programmed: False status when istio-remote is invalid", func(t *testing.T) {
		c1 := NewCluster(t, "c1", "n1")
		c2 := NewCluster(t, "c2", "n2")
		ewGw := c1.CreateOrUpdateEastWestGateway(
			"istio-eastwest",
			constants.IstioSystemNamespace,
			nil,
		)

		generatedPeer := c1.GeneratedPeerGateway(ewGw)
		AssertPeerGateway(c1, generatedPeer)

		// zero out labels removing network topology label and rendering the istio-remote gateway invalid
		generatedPeer.Labels = map[string]string{}
		c2.CreateGateway(generatedPeer)
		expectedGatewayStatus := k8s.GatewayStatus{
			Conditions: []metav1.Condition{
				{
					Type:               constants.SoloConditionPeeringSucceeded,
					Status:             metav1.ConditionFalse,
					Reason:             string(k8s.GatewayReasonInvalid),
					Message:            "no network started encountered error validating gateway: no network label found in gateway",
					LastTransitionTime: metav1.Now(),
				},
			},
		}
		AssertPeerGatewayStatus(c2, generatedPeer.GetName(), generatedPeer.GetNamespace(), expectedGatewayStatus)
	})
	t.Run("istioctl link gw equals generated peer gateway", func(t *testing.T) {
		c1 := NewCluster(t, "c1", "c1")
		c2 := NewCluster(t, "c2", "c2")
		c1.CreateService("svc1", true, nil)

		ewGw := c1.CreateOrUpdateEastWestGateway(
			"istio-eastwest",
			constants.IstioSystemNamespace,
			nil,
		)

		ip, ports := c1.XdsHostPort()
		port, _ := strconv.Atoi(ports)
		gwAddress := k8sbeta.GatewaySpecAddress{
			Value: ip,
			Type:  ptr.Of(k8sbeta.IPAddressType),
		}
		istioctlLinkGw := makeLinkGateway(ewGw.GetName(), ewGw.GetNamespace(), gwAddress, c1.ClusterName, c1.NetworkName, c1.ClusterName, port)
		generatedPeer := c1.GeneratedPeerGateway(ewGw)
		AssertPeerGatewayEqual(istioctlLinkGw, generatedPeer)

		// apply link gateway to c2
		c2.CreateGateway(istioctlLinkGw)

		AssertWE(c1)
		AssertSE(c1, DesiredSE{Name: "autogen.default.svc1"})

		AssertWE(c2, DesiredWE{Name: "autogen.c1.default.svc1"})
		AssertSE(c2, DesiredSE{Name: "autogen.default.svc1"})
	})
	t.Run("creates gme peer gateway with address override", func(t *testing.T) {
		c1 := NewCluster(t, "c1", "c1")
		ewGwAddressOverride := k8sbeta.GatewaySpecAddress{
			Value: "example.host",
			Type:  ptr.Of(k8sbeta.HostnameAddressType),
		}
		ewGw := c1.CreateOrUpdateEastWestGateway(
			"istio-eastwest",
			constants.IstioSystemNamespace,
			[]k8sbeta.GatewaySpecAddress{
				ewGwAddressOverride,
			})
		ewGw.Spec.Addresses = []k8sbeta.GatewaySpecAddress{ewGwAddressOverride}

		generatedPeer := c1.GeneratedPeerGateway(ewGw)
		AssertPeerGateway(c1, generatedPeer)
	})
	negativeTests := []struct {
		name string
		ewGw *k8sbeta.Gateway
	}{
		{
			"no expose-istiod label",
			&k8sbeta.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "istio-eastwest",
					Namespace: constants.IstioSystemNamespace,
					Labels: map[string]string{
						label.TopologyNetwork.Name: "c1",
					},
				},
				Spec: k8sbeta.GatewaySpec{
					GatewayClassName: constants.EastWestGatewayClassName,
				},
			},
		},
		{
			"no network topology label",
			&k8sbeta.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "istio-eastwest",
					Namespace: constants.IstioSystemNamespace,
					Labels: map[string]string{
						constants.ExposeIstiodLabel: "15012",
					},
				},
				Spec: k8sbeta.GatewaySpec{
					GatewayClassName: constants.EastWestGatewayClassName,
				},
			},
		},
		{
			"no cross-network listener",
			&k8sbeta.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "istio-eastwest",
					Namespace: constants.IstioSystemNamespace,
					Labels: map[string]string{
						constants.ExposeIstiodLabel: "15012",
						label.TopologyNetwork.Name:  "c1",
					},
				},
				Spec: k8sbeta.GatewaySpec{
					GatewayClassName: constants.EastWestGatewayClassName,
					Listeners: []k8sbeta.Listener{
						{
							Name:     "xds-tls",
							Port:     k8s.PortNumber(15012),
							Protocol: k8s.TLSProtocolType,
							TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
						},
					},
				},
			},
		},
		{
			"no xds-tls listener",
			&k8sbeta.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "istio-eastwest",
					Namespace: constants.IstioSystemNamespace,
					Labels: map[string]string{
						constants.ExposeIstiodLabel: "15012",
						label.TopologyNetwork.Name:  "c1",
					},
				},
				Spec: k8sbeta.GatewaySpec{
					GatewayClassName: constants.EastWestGatewayClassName,
					Listeners: []k8sbeta.Listener{
						{
							Name:     "cross-network",
							Port:     k8sbeta.PortNumber(15008),
							Protocol: "HBONE",
							TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
						},
					},
				},
			},
		},
		{
			"no status address",
			&k8sbeta.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "istio-eastwest",
					Namespace: constants.IstioSystemNamespace,
					Labels: map[string]string{
						constants.ExposeIstiodLabel: "15012",
						label.TopologyNetwork.Name:  "c1",
					},
				},
				Spec: k8sbeta.GatewaySpec{
					GatewayClassName: constants.EastWestGatewayClassName,
					Listeners: []k8sbeta.Listener{
						{
							Name:     "cross-network",
							Port:     k8sbeta.PortNumber(15008),
							Protocol: "HBONE",
							TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
						},
						{
							Name:     "xds-tls",
							Port:     k8s.PortNumber(15012),
							Protocol: k8s.TLSProtocolType,
							TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
						},
					},
				},
				Status: k8sbeta.GatewayStatus{},
			},
		},
	}
	// Test negative cases where the peer gateway is not created due to an invalid east-west gateway
	for _, tc := range negativeTests {
		t.Run(tc.name, func(t *testing.T) {
			c1 := NewCluster(t, "c1", "c1")

			clienttest.NewWriter[*k8sbeta.Gateway](c1.t, c1.Kube).CreateOrUpdate(tc.ewGw)
			// this is a bit awkward, but we need to wait for reconciliation to happen
			// as the assertion would succeed before reconciliation is completed since there are no remote gateways by default.
			_ = retry.Until(func() bool {
				AssertPeerGateway(c1)
				return false
			}, retry.Timeout(10*time.Millisecond))
		})
	}
}

func init() {
	// With so many controller copies this is super noisy
	log.FindScope("krt").SetOutputLevel(log.WarnLevel)
	features.EnableAmbient = true
	features.EnablePeering = true
	controller.DisableTestClientCleanup = true
}

type Cluster struct {
	Peering         *peering.NetworkWatcher
	Kube            kube.Client
	Discovery       *xds.FakeDiscoveryServer
	ClusterName     string
	NetworkName     string
	t               test.Failer
	ServiceEntries  clienttest.TestClient[*networkingclient.ServiceEntry]
	WorkloadEntries clienttest.TestClient[*networkingclient.WorkloadEntry]
	Gateways        clienttest.TestClient[*k8sbeta.Gateway]
	Outage          *OutageInjector
}

func (c *Cluster) Locality() string {
	return fmt.Sprintf("region-%s/zone-%s", c.ClusterName, c.ClusterName)
}

func (c *Cluster) CreateService(name string, global bool, ports []corev1.ServicePort) {
	if global {
		c.CreateServiceLabel(name, peering.ServiceScopeGlobal, ports)
	} else {
		c.CreateServiceLabel(name, "", ports)
	}
}

func (c *Cluster) CreateServiceLabel(name string, lbl string, ports []corev1.ServicePort) {
	labels := map[string]string{}
	if lbl != "" {
		labels[peering.ServiceScopeLabel] = lbl
	}
	clienttest.NewWriter[*corev1.Service](c.t, c.Kube).CreateOrUpdate(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": name,
			},
			Ports: ports,
		},
	})
}

func (c *Cluster) DeleteService(name string) {
	clienttest.NewWriter[*corev1.Service](c.t, c.Kube).Delete(name, "default")
}

func (c *Cluster) CreateWorkload(serviceName string, workloadName string, serviceAccount string, ports map[string]uint32) {
	clienttest.NewWriter[*networkingclient.WorkloadEntry](c.t, c.Kube).CreateOrUpdate(&networkingclient.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName,
			Namespace: "default",
			Labels: map[string]string{
				"app": serviceName,
			},
		},
		Spec: networking.WorkloadEntry{
			ServiceAccount: serviceAccount,
			Ports:          ports,
		},
	})
}

func (c *Cluster) DeletePod(podName string) {
	clienttest.NewWriter[*corev1.Pod](c.t, c.Kube).Delete(podName, "default")
}

func (c *Cluster) CreatePod(serviceName string, podName string, podIP string) {
	c.CreatePodWithLocality(serviceName, podName, podIP, "", "")
}

func (c *Cluster) CreatePodWithLocality(serviceName, podName, podIP, region, zone string) {

	var nodeName string
	if region != "" || zone == "" {
		nodeName = podName + "-node"
		clienttest.NewWriter[*corev1.Node](c.t, c.Kube).CreateOrUpdate(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Labels: map[string]string{
					"topology.kubernetes.io/zone":   zone,
					"topology.kubernetes.io/region": region,
				},
			},
			Spec: corev1.NodeSpec{},
		})
	}

	clienttest.NewWriter[*corev1.Pod](c.t, c.Kube).CreateOrUpdateStatus(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "default",
			Labels: map[string]string{
				"app": serviceName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			},
			PodIP: podIP,
			PodIPs: []corev1.PodIP{
				{
					IP: podIP,
				},
			},
			Phase: corev1.PodRunning,
		},
	})
}

func (c *Cluster) DeleteWorkload(workloadName string) {
	clienttest.NewWriter[*networkingclient.WorkloadEntry](c.t, c.Kube).Delete(workloadName, "default")
}

func (c *Cluster) CreateOrUpdateEastWestGateway(gwName, gwNs string, gwAddresses []k8sbeta.GatewaySpecAddress) *k8sbeta.Gateway {
	ip, ports := c.XdsHostPort()
	port, _ := strconv.Atoi(ports)
	gwStatusAddr := k8s.GatewayStatusAddress{
		Value: ip,
		Type:  ptr.Of(k8sbeta.IPAddressType),
	}
	if len(gwAddresses) > 0 {
		gwStatusAddr = k8s.GatewayStatusAddress{
			Value: gwAddresses[0].Value,
			Type:  gwAddresses[0].Type,
		}
	}
	gw := &k8sbeta.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gwName,
			Namespace: gwNs,
			Labels: map[string]string{
				label.TopologyNetwork.Name:  c.NetworkName,
				label.TopologyCluster.Name:  c.ClusterName,
				constants.ExposeIstiodLabel: ports,
			},
		},
		Spec: k8sbeta.GatewaySpec{
			GatewayClassName: constants.EastWestGatewayClassName,
			Listeners: []k8sbeta.Listener{
				{
					Name:     "cross-network",
					Port:     k8sbeta.PortNumber(15008),
					Protocol: "HBONE",
					TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
				{
					Name:     "xds-tls",
					Port:     k8s.PortNumber(port),
					Protocol: k8s.TLSProtocolType,
					TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
			},
			Addresses: gwAddresses,
		},
		Status: k8sbeta.GatewayStatus{
			Addresses: []k8s.GatewayStatusAddress{
				gwStatusAddr,
			},
		},
	}
	clienttest.NewWriter[*k8sbeta.Gateway](c.t, c.Kube).CreateOrUpdate(gw)
	return gw
}

func (c *Cluster) XdsHostPort() (string, string) {
	addr := c.Discovery.Listener.Addr().String()
	ip, port, _ := net.SplitHostPort(addr)
	return ip, port
}

func (c *Cluster) GeneratedPeerGateway(ewGateway *k8sbeta.Gateway) *k8sbeta.Gateway {
	ip, ports := c.XdsHostPort()
	port, _ := strconv.Atoi(ports)
	gwAddresses := []k8sbeta.GatewaySpecAddress{
		{
			Value: ip,
			Type:  ptr.Of(k8sbeta.IPAddressType),
		},
	}
	if len(ewGateway.Spec.Addresses) > 0 {
		gwAddresses = ewGateway.Spec.Addresses
	}
	return &k8sbeta.Gateway{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.KubernetesGateway.Kind,
			APIVersion: gvk.KubernetesGateway.GroupVersion(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("istio-remote-peer-%s", c.ClusterName),
			Namespace: constants.IstioSystemNamespace,
			Annotations: map[string]string{
				constants.GatewayServiceAccountAnnotation: constants.EastWestGatewayClassName,
				constants.TrustDomainAnnotation:           c.ClusterName,
			},
			Labels: map[string]string{
				constants.ExposeIstiodLabel: ports,
				label.TopologyNetwork.Name:  c.NetworkName,
				label.TopologyCluster.Name:  c.ClusterName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       gvk.KubernetesGateway_v1.Kind,
					APIVersion: gvk.KubernetesGateway_v1.GroupVersion(),
					Name:       ewGateway.Name,
					UID:        ewGateway.UID,
				},
			},
		},
		Spec: k8sbeta.GatewaySpec{
			GatewayClassName: constants.RemoteGatewayClassName,
			Addresses:        gwAddresses,
			Listeners: []k8sbeta.Listener{
				{
					Name:     "cross-network",
					Port:     k8sbeta.PortNumber(15008),
					Protocol: "HBONE",
					TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
				{
					Name:     "xds-tls",
					Port:     k8sbeta.PortNumber(port),
					Protocol: k8s.TLSProtocolType,
					TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
			},
		},
	}
}

// makeLinkGateway simulates a creation of the istio-remote gateway by the `istioctl multicluster link` command.
// code is copied from the istioctl/pkg/peer/link.go file with slight modifications to use the istiod xds port instead
// of the hardcoded 15012 port to work with the fake discovery server.
func makeLinkGateway(
	ewGwName string,
	gwNs string,
	addr k8sbeta.GatewaySpecAddress,
	cluster string,
	network string,
	trustDomain string,
	xdsPort int,
) *k8sbeta.Gateway {
	gw := k8sbeta.Gateway{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.KubernetesGateway_v1.Kind,
			APIVersion: gvk.KubernetesGateway_v1.GroupVersion(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-remote-peer-" + network,
			Namespace: gwNs,
			Annotations: map[string]string{
				constants.GatewayServiceAccountAnnotation: ewGwName,
				constants.TrustDomainAnnotation:           trustDomain,
			},
			Labels: map[string]string{
				label.TopologyNetwork.Name: network,
				label.TopologyCluster.Name: cluster,
			},
		},
		Spec: k8sbeta.GatewaySpec{
			GatewayClassName: "istio-remote",
			Addresses:        []k8sbeta.GatewaySpecAddress{addr},
			Listeners: []k8sbeta.Listener{
				{
					Name:     "cross-network",
					Port:     k8sbeta.PortNumber(15008),
					Protocol: "HBONE",
					TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
				{
					Name:     "xds-tls",
					Port:     k8sbeta.PortNumber(xdsPort),
					Protocol: k8s.TLSProtocolType,
					TLS:      &k8sbeta.GatewayTLSConfig{Mode: ptr.Of(k8s.TLSModePassthrough)},
				},
			},
		},
	}

	return &gw
}

func (c *Cluster) CreateGateway(gw *k8sbeta.Gateway) {
	clienttest.NewWriter[*k8sbeta.Gateway](c.t, c.Kube).CreateOrUpdate(gw)
}

func (c *Cluster) DeleteGateway(name, namespace string) {
	clienttest.NewWriter[*k8sbeta.Gateway](c.t, c.Kube).Delete(name, namespace)
}

func (c *Cluster) DisconnectFrom(other *Cluster) {
	clienttest.NewWriter[*k8sbeta.Gateway](c.t, c.Kube).Delete("peer-to-"+other.ClusterName, "istio-system")
}

func (c *Cluster) ConnectTo(other *Cluster) {
	addr := other.Discovery.Listener.Addr().String()
	ip, ports, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(ports)
	gw := &k8sbeta.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "peer-to-" + other.ClusterName,
			Namespace: "istio-system",
			Labels: map[string]string{
				label.TopologyNetwork.Name:      other.NetworkName,
				label.TopologyCluster.Name:      other.ClusterName,
				"topology.kubernetes.io/region": "region-" + other.ClusterName,
				"topology.kubernetes.io/zone":   "zone-" + other.ClusterName,
			},
		},
		Spec: k8s.GatewaySpec{
			GatewayClassName: "istio-remote",
			Listeners: []k8s.Listener{{
				Name:     "cross-network",
				Port:     k8s.PortNumber(15008),
				Protocol: "HBONE",
			}, {
				Name:     "xds-tls",
				Port:     k8s.PortNumber(port),
				Protocol: k8s.TLSProtocolType,
			}},
			Addresses: []k8s.GatewaySpecAddress{{
				Type:  ptr.Of(k8s.IPAddressType),
				Value: ip,
			}},
		},
	}
	clienttest.NewWriter[*k8sbeta.Gateway](c.t, c.Kube).CreateOrUpdate(gw)
}

func NewCluster(t test.Failer, clusterName, networkName string) *Cluster {
	return newCluster(t, nil, test.NewStop(t), false, clusterName, networkName)
}

func newCluster(t test.Failer, premadeKubeClient kube.Client, stop chan struct{}, startWithOutage bool, clusterName, networkName string) *Cluster {
	outage := NewOutageInjector()
	outage.setOutage(startWithOutage)
	buildConfig := func(clientName string) *adsc.DeltaADSConfig {
		return &adsc.DeltaADSConfig{
			Config: adsc.Config{
				ClientName: clientName,
				Namespace:  "istio-system",
				Workload:   clusterName,
				GrpcOpts: []grpc.DialOption{
					grpc.WithStreamInterceptor(outage.Interceptor),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				},
				BackoffPolicy: backoff.NewExponentialBackOff(backoff.Option{InitialInterval: time.Millisecond, MaxInterval: time.Second}),
			},
		}
	}
	fo := xds.FakeOptions{
		ListenerBuilder: func() (net.Listener, error) {
			return net.Listen("tcp", "127.0.0.1:0")
		},
	}
	if premadeKubeClient != nil {
		fo.KubeClientBuilder = func(objects ...runtime.Object) kube.Client {
			return premadeKubeClient
		}
	}
	ds := xds.NewFakeDiscoveryServer(t, fo)

	kc := ds.KubeClient()
	// create istio-system namespace with network topology label to simulate network configuration that is set during istio installation
	if _, err := kc.Kube().CoreV1().Namespaces().Get(context.Background(), constants.IstioSystemNamespace, metav1.GetOptions{}); err != nil {
		_, _ = kc.Kube().CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   constants.IstioSystemNamespace,
				Labels: map[string]string{label.TopologyNetwork.Name: networkName},
			},
		}, metav1.CreateOptions{})
	}
	store := &testStore{
		client: kc,
	}
	statusManager := pilotstatus.NewManager(store)
	peer := peering.New(
		kc,
		constants.IstioSystemNamespace,
		cluster.ID(clusterName),
		"cluster.local",
		buildConfig,
		nil,
		fakeMeshHolder(clusterName),
		ds.KubeRegistry,
		statusManager,
	)
	if premadeKubeClient == nil {
		kc.RunAndWait(stop)
	}
	go peer.Run(stop)
	return &Cluster{
		Discovery:   ds,
		Kube:        kc,
		Peering:     peer,
		ClusterName: clusterName,
		NetworkName: networkName,
		Outage:      outage,

		ServiceEntries:  clienttest.NewDirectClient[*networkingclient.ServiceEntry, *networkingclient.ServiceEntry, *networkingclient.ServiceEntryList](t, kc),
		WorkloadEntries: clienttest.NewDirectClient[*networkingclient.WorkloadEntry, *networkingclient.WorkloadEntry, *networkingclient.WorkloadEntryList](t, kc),
		Gateways:        clienttest.NewDirectClient[*k8sbeta.Gateway, k8sbeta.Gateway, *k8sbeta.GatewayList](t, kc),
		t:               t,
	}
}

func fakeMeshHolder(clusterName string) mesh.Watcher {
	config := mesh.DefaultMeshConfig()
	config.TrustDomain = clusterName
	return meshwatcher.NewTestWatcher(config)
}

type DesiredWE struct {
	Name     string
	Address  string
	Locality string
}

func AssertWE(c *Cluster, we ...DesiredWE) {
	c.t.Helper()
	have := slices.SortBy(we, func(a DesiredWE) string {
		return a.Name
	})
	fetch := func() []DesiredWE {
		return slices.SortBy(
			slices.MapFilter(c.WorkloadEntries.List(peering.PeeringNamespace, klabels.Everything()), func(a *networkingclient.WorkloadEntry) *DesiredWE {
				return &DesiredWE{a.Name, a.Spec.Address, a.Spec.Locality}
			}),
			func(a DesiredWE) string {
				return a.Name
			},
		)
	}
	assert.EventuallyEqual(c.t, fetch, have)
}

func AssertWELabels(c *Cluster, name string, labels map[string]string) {
	c.t.Helper()
	fetch := func() map[string]string {
		we := c.WorkloadEntries.Get(name, peering.PeeringNamespace)
		return we.GetLabels()
	}
	assert.EventuallyEqual(c.t, fetch, labels)
}

func AssertWEPorts(c *Cluster, name string, ports map[string]uint32) {
	c.t.Helper()
	fetch := func() map[string]uint32 {
		we := c.WorkloadEntries.Get(name, peering.PeeringNamespace)
		if we == nil {
			return nil
		}
		return we.Spec.GetPorts()
	}
	assert.EventuallyEqual(c.t, fetch, ports)
}

type DesiredSE struct {
	Name            string
	ServiceAccounts []string
}

func AssertSE(c *Cluster, we ...DesiredSE) {
	c.t.Helper()
	have := slices.SortBy(we, func(a DesiredSE) string {
		return a.Name
	})
	fetch := func() []DesiredSE {
		return slices.SortBy(
			slices.Map(c.ServiceEntries.List(metav1.NamespaceAll, klabels.Everything()), func(a *networkingclient.ServiceEntry) DesiredSE {
				return DesiredSE{Name: a.Name, ServiceAccounts: a.Spec.SubjectAltNames}
			}),
			func(a DesiredSE) string {
				return a.Name
			},
		)
	}
	assert.EventuallyEqual(c.t, fetch, have, retry.Timeout(time.Second*5))
}

func AssertSEPorts(c *Cluster, name string, ports []*networking.ServicePort) {
	c.t.Helper()
	fetch := func() []*networking.ServicePort {
		we := c.ServiceEntries.Get(name, peering.PeeringNamespace)
		return we.Spec.GetPorts()
	}
	assert.EventuallyEqual(c.t, fetch, ports)
}

type OutageInjector struct {
	outage   bool
	mu       sync.RWMutex
	toCancel []context.CancelCauseFunc
}

func NewOutageInjector() *OutageInjector {
	return &OutageInjector{}
}

func (o *OutageInjector) Interceptor(
	ctx context.Context,
	desc *grpc.StreamDesc,
	cc *grpc.ClientConn,
	method string,
	streamer grpc.Streamer,
	opts ...grpc.CallOption,
) (grpc.ClientStream, error) {
	if o.isOutage() {
		return nil, status.Error(codes.Unavailable, "network outage: connection dropped")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	s, err := streamer(ctx, desc, cc, method, opts...)
	o.add(cancel)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (o *OutageInjector) isOutage() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.outage
}

func (o *OutageInjector) setOutage(outage bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outage = outage
	log.Infof("setting outage mode to %v", outage)
	if outage {
		// Close all existing streams
		for _, c := range o.toCancel {
			c(fmt.Errorf("disconnecting due to injected outage"))
		}
		o.toCancel = nil
	}
}

func (o *OutageInjector) add(cancel context.CancelCauseFunc) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toCancel = append(o.toCancel, cancel)
}

func AssertPeerGateway(c *Cluster, desiredGws ...*k8sbeta.Gateway) {
	c.t.Helper()
	have := slices.Map(desiredGws, func(gw *k8sbeta.Gateway) types.NamespacedName {
		return types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
	})
	fetch := func() []*k8sbeta.Gateway {
		return slices.SortBy(
			slices.Filter(c.Gateways.List(metav1.NamespaceAll, klabels.Everything()), func(gw *k8sbeta.Gateway) bool {
				return slices.Contains(have, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace})
			}),
			func(gw *k8sbeta.Gateway) string {
				return gw.Name
			},
		)
	}
	assert.EventuallyEqual(c.t, fetch, desiredGws, retry.Timeout(time.Second*5))
}

func AssertPeerGatewayStatus(c *Cluster, name, namespace string, status k8s.GatewayStatus) {
	c.t.Helper()
	fetch := func() k8sbeta.GatewayStatus {
		return c.Gateways.Get(name, namespace).Status
	}
	assert.EventuallyEqual(c.t, fetch, status, retry.Timeout(time.Second*5))
}

func AssertPeerGatewayEqual(gw1, gw2 *k8sbeta.Gateway) bool {
	if gw1.GetName() != gw2.GetName() {
		return false
	}
	if gw1.GetNamespace() != gw2.GetNamespace() {
		return false
	}
	if gw1.GetLabels()[label.TopologyNetwork.Name] != gw2.GetLabels()[label.TopologyNetwork.Name] {
		return false
	}
	if gw1.GetAnnotations()[constants.GatewayServiceAccountAnnotation] != gw2.GetAnnotations()[constants.GatewayServiceAccountAnnotation] {
		return false
	}
	if gw1.GetAnnotations()[constants.TrustDomainAnnotation] != gw2.GetAnnotations()[constants.TrustDomainAnnotation] {
		return false
	}
	return reflect.DeepEqual(gw1.Spec, gw2.Spec)
}

type testStore struct {
	model.ConfigStore
	client kube.Client
}

func (t *testStore) UpdateStatus(config config.Config) (string, error) {
	gw, err := t.client.GatewayAPI().GatewayV1beta1().Gateways(config.Namespace).Get(context.TODO(), config.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	gw.Status = *(config.Status.(*k8sbeta.GatewayStatus))
	_, err = t.client.GatewayAPI().GatewayV1beta1().Gateways(config.Namespace).UpdateStatus(context.TODO(), gw, metav1.UpdateOptions{})
	return "", err
}

func (t *testStore) Get(typ config.GroupVersionKind, name, namespace string) *config.Config {
	if typ != gvk.KubernetesGateway {
		return nil
	}
	gw, err := t.client.GatewayAPI().GatewayV1beta1().Gateways(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return &config.Config{
		Meta: config.Meta{
			GroupVersionKind: gvk.KubernetesGateway,
			Name:             gw.Name,
			Namespace:        gw.Namespace,
			ResourceVersion:  gw.ResourceVersion,
		},
		Spec:   &gw.Spec,
		Status: &gw.Status,
	}
}
