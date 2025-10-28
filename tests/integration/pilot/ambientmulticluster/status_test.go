//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambientmulticluster

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "sigs.k8s.io/gateway-api/apis/v1"
	k8sbeta "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/cluster"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/tests/integration/pilot/ambientmulticluster/shared"
)

const (
	peeringNamespace = "istio-gateway"
)

func TestStatus(t *testing.T) {
	framework.
		NewTest(t).
		Run(func(t framework.TestContext) {
			ctx := t.Context()
			// TODO expand this test to handle the RemoteFlatCluster
			localCluster := t.Clusters().GetByName(shared.LocalCluster)
			remoteCluster := t.Clusters().GetByName(shared.RemoteNetworkCluster)

			// check istio-remote for local -> remote in local cluster
			localToRemoteGwName := "istio-remote-peer-" + shared.RemoteNetworkCluster
			localToRemoteGw, err := localCluster.GatewayAPI().GatewayV1beta1().Gateways(peeringNamespace).Get(ctx, localToRemoteGwName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to get local gateway: %v", err)
			}

			if len(localToRemoteGw.Spec.Addresses) == 0 {
				t.Fatal("Gateway spec has no addresses")
			}
			addressValue := localToRemoteGw.Spec.Addresses[0].Value
			expectedStatus := k8s.GatewayStatus{
				Addresses: []k8s.GatewayStatusAddress{
					{
						Value: addressValue,
						Type:  ptr.Of(k8sbeta.IPAddressType),
					},
				},
				Conditions: []metav1.Condition{
					{
						ObservedGeneration: 1,
						Type:               string(k8s.GatewayConditionAccepted),
						Status:             metav1.ConditionTrue,
						Reason:             string(k8s.GatewayReasonAccepted),
						Message:            "Resource accepted",
					},
					{
						ObservedGeneration: 1,
						Type:               string(k8s.GatewayConditionProgrammed),
						Status:             metav1.ConditionTrue,
						Reason:             string(k8s.GatewayReasonProgrammed),
						Message:            "This Gateway is remote; Istio will not program it",
					},
					{
						ObservedGeneration: 1,
						Type:               constants.SoloConditionPeerConnected,
						Status:             metav1.ConditionTrue,
						Reason:             string(k8s.GatewayReasonProgrammed),
					},
					{
						ObservedGeneration: 1,
						Type:               constants.SoloConditionPeeringSucceeded,
						Status:             metav1.ConditionTrue,
						Reason:             string(k8s.GatewayReasonProgrammed),
					},
					{
						ObservedGeneration: 1,
						Type:               constants.SoloConditionPeerDataPlaneProgrammed,
						Status:             metav1.ConditionTrue,
						Reason:             string(k8s.GatewayReasonProgrammed),
						Message:            "peering data plane programmed for cross-network traffic with LoadBalancer service type",
					},
				},
			}
			AssertPeerGatewayStatus(t, localCluster, localToRemoteGwName, peeringNamespace, expectedStatus)

			// check istio-remote for remote -> local in remote cluster
			remoteToLocalGwName := "istio-remote-peer-" + shared.LocalCluster
			remoteToLocalGw, err := remoteCluster.GatewayAPI().GatewayV1beta1().Gateways(peeringNamespace).Get(t.Context(), remoteToLocalGwName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to get remote gateway: %v", err)
			}

			// update expected status with remote address
			if len(remoteToLocalGw.Spec.Addresses) == 0 {
				t.Fatal("Remote gateway spec has no addresses")
			}
			remoteAddressValue := remoteToLocalGw.Spec.Addresses[0].Value
			expectedStatus.Addresses[0].Value = remoteAddressValue

			AssertPeerGatewayStatus(t, remoteCluster, remoteToLocalGwName, peeringNamespace, expectedStatus)
		})
}

func AssertPeerGatewayStatus(t framework.TestContext, c cluster.Cluster, name, namespace string, status k8s.GatewayStatus) {
	fetch := func() k8sbeta.GatewayStatus {
		gw, err := c.GatewayAPI().GatewayV1beta1().Gateways(namespace).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get gateway: %v", err)
		}
		return gw.Status
	}
	_ = retry.UntilSuccess(func() error {
		gwStatus := fetch()
		if len(gwStatus.Conditions) > 0 {
			// add transition time stamp to the expected status
			for i := range status.Conditions {
				status.Conditions[i].LastTransitionTime = gwStatus.Conditions[i].LastTransitionTime
			}
		}
		return nil
	}, retry.Timeout(time.Second*5))
	assert.EventuallyEqual(t, fetch, status, retry.Timeout(time.Second*30))
}
