//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambient

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/label"
	v1alpha3 "istio.io/api/networking/v1alpha3"
	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
)

func testNamespacedAutoWaypoint(t framework.TestContext) {
	_, err := t.Clusters().
		Default().
		Kube().
		CoreV1().
		Namespaces().
		Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "solo-auto-waypoint-namespace",
				Labels: map[string]string{
					label.IoIstioUseWaypoint.Name: "auto",
				},
			},
		}, metav1.CreateOptions{})
	assert.NoError(t, err)
	retry.UntilSuccessOrFail(t, func() error {
		waypoint, err := t.Clusters().
			Default().
			GatewayAPI().
			GatewayV1().
			Gateways("solo-auto-waypoint-namespace").
			Get(context.Background(), "auto", metav1.GetOptions{})
		if err != nil {
			return err
		}
		assert.Equal(t, waypoint.Spec.GatewayClassName, "istio-waypoint")
		assert.Equal(t, waypoint.Annotations["solo.io/auto-waypoint"], "true")
		addresses := waypoint.Status.Addresses
		if len(addresses) == 0 {
			return fmt.Errorf("no addresses found for waypoint %s/%s", waypoint.Namespace, waypoint.Name)
		}
		return nil
	})
}

// This could very plausibly be moved into the normal test setup since it does not require it's own namespace to run per se
func testServiceAutoWaypoint(t framework.TestContext) {
	_, err := t.Clusters().
		Default().
		Kube().
		CoreV1().
		Namespaces().
		Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "solo-service-auto-waypoint-namespace",
			},
		}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = t.Clusters().
		Default().
		Kube().
		CoreV1().
		Services("solo-service-auto-waypoint-namespace").
		Create(context.Background(), &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "solo-service-auto-waypoint",
				Labels: map[string]string{
					label.IoIstioUseWaypoint.Name: "auto",
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"app": "solo-service-auto-waypoint",
				},
				Ports: []corev1.ServicePort{
					{
						Port: 80,
					},
				},
			},
		}, metav1.CreateOptions{})
	assert.NoError(t, err)
	retry.UntilSuccessOrFail(t, func() error {
		waypoint, err := t.Clusters().
			Default().
			GatewayAPI().
			GatewayV1().
			Gateways("solo-service-auto-waypoint-namespace").
			Get(context.Background(), "auto", metav1.GetOptions{})
		if err != nil {
			return err
		}
		assert.Equal(t, waypoint.Spec.GatewayClassName, "istio-waypoint")
		assert.Equal(t, waypoint.Annotations["solo.io/auto-waypoint"], "true")
		addresses := waypoint.Status.Addresses
		if len(addresses) == 0 {
			return fmt.Errorf("no addresses found for waypoint %s/%s", waypoint.Namespace, waypoint.Name)
		}
		return nil
	})
}

func testServiceEntryAutoWaypoint(t framework.TestContext) {
	_, err := t.Clusters().
		Default().
		Kube().
		CoreV1().
		Namespaces().
		Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "solo-service-entry-auto-waypoint-namespace",
			},
		}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = t.Clusters().
		Default().
		Istio().
		NetworkingV1().
		ServiceEntries("solo-service-entry-auto-waypoint-namespace").
		Create(context.Background(), &networkingv1.ServiceEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name: "solo-service-entry-auto-waypoint",
				Labels: map[string]string{
					label.IoIstioUseWaypoint.Name: "auto",
				},
			},
			Spec: v1alpha3.ServiceEntry{
				Hosts:      []string{"test.testing.io"},
				Location:   v1alpha3.ServiceEntry_MESH_EXTERNAL,
				Resolution: v1alpha3.ServiceEntry_STATIC,
			},
		}, metav1.CreateOptions{})
	assert.NoError(t, err)
	retry.UntilSuccessOrFail(t, func() error {
		waypoint, err := t.Clusters().
			Default().
			GatewayAPI().
			GatewayV1().
			Gateways("solo-service-entry-auto-waypoint-namespace").
			Get(context.Background(), "auto", metav1.GetOptions{})
		if err != nil {
			return err
		}
		assert.Equal(t, waypoint.Spec.GatewayClassName, "istio-waypoint")
		assert.Equal(t, waypoint.Annotations["solo.io/auto-waypoint"], "true")
		addresses := waypoint.Status.Addresses
		if len(addresses) == 0 {
			return fmt.Errorf("no addresses found for waypoint %s/%s", waypoint.Namespace, waypoint.Name)
		}
		return nil
	})
}

func TestSoloAutoWaypoint(t *testing.T) {
	framework.NewTest(t).
		Run(func(t framework.TestContext) {
			t.NewSubTest("solo namespace auto waypoint").Run(testNamespacedAutoWaypoint)
			t.NewSubTest("solo service auto waypoint").Run(testServiceAutoWaypoint)
			t.NewSubTest("solo service entry auto waypoint").Run(testServiceEntryAutoWaypoint)
		})
}
