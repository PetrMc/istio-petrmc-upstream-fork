// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package autowaypoint

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayapi "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
)

// modifyFn is a function that modifies the namespace or gateways in the test case
type modifyFn func(nsName string, client kube.CLIClient) error

// customAssertFn is a function that asserts a condition on the test namespace
type customAssertFn func(t *testing.T, nsName string, client kube.CLIClient)

func TestAutoWaypointController(t *testing.T) {
	testCases := []struct {
		name                 string
		ns                   *v1.Namespace
		gws                  []*gatewayapi.Gateway
		modifyFn             modifyFn
		customAssertFn       customAssertFn
		expectedGatewayCount int
		expectSoloAuto       bool
	}{
		// opt-in is the most basic path which should create a waypoint
		{
			name: "opt-in",
			ns: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "opt-in",
					Labels: map[string]string{
						"istio.io/use-waypoint": "auto",
					},
				},
			},
			expectedGatewayCount: 1,
			expectSoloAuto:       true,
		},
		// opt-out should not create a waypoint
		{
			name: "opt-out",
			ns: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "opt-out"},
			},
			expectedGatewayCount: 0,
			expectSoloAuto:       false,
		},
		// opt-in-with-gw should create a waypoint even though there is another gateway	in the namespace
		{
			name: "opt-in-with-gw",
			ns: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "opt-in-with-gw",
					Labels: map[string]string{
						"istio.io/use-waypoint": "auto",
					},
				},
			},
			gws: []*gatewayapi.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "gw1"},
				},
			},
			expectedGatewayCount: 2,
			expectSoloAuto:       true,
		},
		// opt-in-with-gw-auto should not overwrite the existing gateway, which is named "auto"
		{
			name: "opt-in-with-gw-auto",
			ns: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "opt-in-with-gw-auto",
				},
			},
			gws: []*gatewayapi.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "auto",
						Annotations: map[string]string{
							"some-annotation": "some-value",
						},
					},
					Spec: gatewayapi.GatewaySpec{
						GatewayClassName: "test-gw-class",
					},
				},
			},
			modifyFn: func(nsName string, client kube.CLIClient) error {
				_, err := client.Kube().CoreV1().Namespaces().Patch(context.Background(),
					nsName,
					types.MergePatchType,
					[]byte(`{"metadata":{"labels":{"istio.io/use-waypoint":"auto"}}}`),
					metav1.PatchOptions{})
				return err
			},
			expectedGatewayCount: 1,
			expectSoloAuto:       false,
			customAssertFn: func(t *testing.T, nsName string, client kube.CLIClient) {
				gw, err := client.GatewayAPI().GatewayV1beta1().Gateways(nsName).Get(context.Background(), "auto", metav1.GetOptions{})
				assert.NoError(t, err)
				assert.Equal(t, "some-value", gw.Annotations["some-annotation"])
				spec := gw.Spec
				assert.Equal(t, gatewayapi.GatewaySpec{GatewayClassName: "test-gw-class"}, spec)
			},
		},
		// opt-in-with-gw-auto-removed asserts that we track correctly even when there is a gateway named "auto" that is not a waypoint
		// we should create an auto waypoint when the existing gateway named "auto" is removed
		{
			name: "opt-in-with-gw-auto-removed",
			ns: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "opt-in-with-gw-auto-removed",
				},
			},
			gws: []*gatewayapi.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "auto"},
					Spec: gatewayapi.GatewaySpec{
						GatewayClassName: "test-gw-class",
					},
				},
			},
			expectedGatewayCount: 1,
			expectSoloAuto:       true,
			modifyFn: func(nsName string, client kube.CLIClient) error {
				_, err := client.Kube().CoreV1().Namespaces().Patch(context.Background(),
					nsName,
					types.MergePatchType,
					[]byte(`{"metadata":{"labels":{"istio.io/use-waypoint":"auto"}}}`),
					metav1.PatchOptions{})
				if err != nil {
					return err
				}

				return client.GatewayAPI().GatewayV1beta1().Gateways(nsName).Delete(context.Background(), "auto", metav1.DeleteOptions{})
			},
		},
	}

	client := kube.NewFakeClient()
	dbg := new(krt.DebugHandler)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		if t.Failed() {
			b, err := dbg.MarshalJSON()
			if err != nil {
				t.Logf("failed to marshal krt debug info: %v", err)
			}
			t.Logf("krt debug info: %s", string(b))
		}
		cancel()
	})
	clienttest.MakeCRD(t, client, gvr.KubernetesGateway)

	controller := NewAutoWaypoint(client, dbg)
	if controller == nil {
		t.Fatalf("failed to create controller")
	}
	go controller.Run(ctx.Done())

	// Run the client after creating and running the controller to make sure informers are started
	client.RunAndWait(ctx.Done())

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// create the test case namespace
			_, err := client.Kube().CoreV1().Namespaces().Create(context.Background(), tc.ns, metav1.CreateOptions{})
			assert.NoError(t, err)

			// create any test case gateways
			for _, gw := range tc.gws {
				_, err := client.GatewayAPI().GatewayV1beta1().Gateways(tc.ns.Name).Create(context.Background(), gw, metav1.CreateOptions{})
				assert.NoError(t, err)
			}

			// modify the namespace or gateways as needed
			if tc.modifyFn != nil {
				assert.NoError(t, tc.modifyFn(tc.ns.Name, client))
			}

			// perform eventual assertions
			retry.UntilSuccessOrFail(t, func() error {
				gws, err := client.GatewayAPI().GatewayV1beta1().Gateways(tc.ns.Name).List(context.Background(), metav1.ListOptions{})
				assert.NoError(t, err)
				if len(gws.Items) != tc.expectedGatewayCount {
					return fmt.Errorf("expected %d gateway, got %d", tc.expectedGatewayCount, len(gws.Items))
				}
				if tc.expectSoloAuto {
					assertSoloAuto(t, tc.ns.Name, client)
				}
				if tc.customAssertFn != nil {
					tc.customAssertFn(t, tc.ns.Name, client)
				}
				return nil
			}, retry.Timeout(time.Second*1))
			// clean up
			err = client.Kube().CoreV1().Namespaces().Delete(context.Background(), tc.ns.Name, metav1.DeleteOptions{})
			assert.NoError(t, err)
		})
	}
}

// assertSoloAuto asserts that the waypoint exists with the correct annotations and spec
func assertSoloAuto(t *testing.T, nsName string, client kube.CLIClient) {
	gw, err := client.GatewayAPI().GatewayV1beta1().Gateways(nsName).Get(context.Background(), "auto", metav1.GetOptions{})
	assert.NoError(t, err)
	if gw == nil {
		t.Fatalf("Expected a solo auto waypoint to exist, but none found")
	}
	assert.Equal(t, "true", gw.Annotations["solo.io/auto-waypoint"])
	spec := gw.Spec
	assert.Equal(t, constants.WaypointGatewayClassName, spec.GatewayClassName)
	assert.Equal(t, 1, len(spec.Listeners))
	assert.Equal(t, 15008, spec.Listeners[0].Port)
	assert.Equal(t, "mesh", spec.Listeners[0].Name)
	assert.Equal(t, gatewayapi.ProtocolType(protocol.HBONE), spec.Listeners[0].Protocol)
}
