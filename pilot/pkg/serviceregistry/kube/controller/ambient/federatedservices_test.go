// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ambient

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/api/label"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/krt/krttest"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/workloadapi"
	"istio.io/istio/platform/discovery/peering"
)

func TestBucket(t *testing.T) {
	cases := []struct {
		in  int
		out uint32
	}{
		{0, 0},
		{1, 1},
		{2, 2},
		{3, 2},
		{4, 4},
		{-1, 0},
		{511, 256},
		{999999999999, 2048},
	}
	for _, tt := range cases {
		t.Run(fmt.Sprint(tt.in), func(t *testing.T) {
			got := bucket(tt.in)
			assert.Equal(t, tt.out, got)
		})
	}
}

func TestFederatedServiceDraining(t *testing.T) {
	for drain, drainWeight := range map[string]*wrapperspb.UInt32Value{
		"none": nil,
		"0":    wrapperspb.UInt32(0),
		"10":   wrapperspb.UInt32(10),
		"100":  wrapperspb.UInt32(100),
	} {
		gw := &v1beta1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "istio-eastwest",
				Namespace: "ns-gtw",
				Labels: map[string]string{
					label.TopologyCluster.Name: testC,
					label.TopologyNetwork.Name: testC,
				},
			},
			Spec: v1beta1.GatewaySpec{
				GatewayClassName: "istio-eastwest",
				Listeners: []v1beta1.Listener{
					{
						Name:     "cross-network",
						Port:     15008,
						Protocol: "HBONE",
					},
				},
			},
		}
		if drainWeight != nil {
			gw.Annotations = map[string]string{
				peering.DrainingWeightAnnotation: drain,
			}
		}
		mock := krttest.NewMock(t, []any{gw})
		a := newAmbientUnitTest(t)
		gateways := krttest.GetMockCollection[*v1beta1.Gateway](mock)
		opts := krt.NewOptionsBuilder(test.NewStop(t), "", krt.GlobalDebugHandler)
		a.drainingByClusters = drainingCollection(gateways, opts)
		kube.WaitForCacheSync("draining", a.stop, a.drainingByClusters.HasSynced)

		t.Run(drain, func(t *testing.T) {
			inputs := []any{
				model.ServiceInfo{
					SoloServiceScope: "global",
					Source:           model.TypedObject{Kind: kind.Service},
					Service: &workloadapi.Service{
						Name:      "svc",
						Namespace: "ns",
						Hostname:  "hostname",
						Ports: []*workloadapi.Port{{
							ServicePort: 80,
							TargetPort:  8080,
						}},
					},
					LabelSelector: model.NewSelector(map[string]string{"app": "foo"}),
				},
			}
			mock := krttest.NewMock(t, inputs)
			workloadServices := krttest.GetMockCollection[model.ServiceInfo](mock)
			workloads := krttest.GetMockCollection[model.WorkloadInfo](mock)
			waypoints := krttest.GetMockCollection[Waypoint](mock)
			workloadServiceIndex := krt.NewIndex[string, model.WorkloadInfo](workloads, "service", func(o model.WorkloadInfo) []string {
				return maps.Keys(o.Workload.Services)
			})
			federatedServices, _ := a.FederatedServicesCollection(a.meshConfig, workloadServices, workloads, waypoints, workloadServiceIndex, opts)
			kube.WaitForCacheSync("federatedServices", a.stop, federatedServices.HasSynced)

			if drainWeight.GetValue() == 0 {
				drainWeight = nil
			}
			result := []model.FederatedService{{
				FederatedService: &workloadapi.FederatedService{
					Name:      "svc",
					Namespace: "ns",
					Hostname:  "hostname",
					Ports: []*workloadapi.Port{{
						ServicePort: 80,
						TargetPort:  8080,
					}},
					Capacity:       wrapperspb.UInt32(0),
					DrainingWeight: drainWeight,
				},
			}}
			assert.Equal(t, federatedServices.List(), result)
		})
	}
}
