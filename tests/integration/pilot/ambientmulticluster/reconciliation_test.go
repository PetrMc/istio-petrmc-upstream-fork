//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambientmulticluster

import (
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/util/retry"
)

func TestGatewayReconciliation(t *testing.T) {
	framework.
		NewTest(t).
		Run(func(t framework.TestContext) {
			cluster := t.Clusters().Default()

			ewgw, err := cluster.GatewayAPI().GatewayV1beta1().Gateways(peeringNamespace).Get(t.Context(), "istio-eastwest", v1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to get east west gateway: %v", err)
			}

			var serviceType string

			serviceType = "LoadBalancer"
			validateServiceType := func() error {
				service, err := cluster.Kube().CoreV1().Services(peeringNamespace).Get(t.Context(), "istio-eastwest", v1.GetOptions{})
				if err != nil {
					return err
				}

				if string(service.Spec.Type) != serviceType {
					return fmt.Errorf("service type incorrect: wanted %s, got %s", serviceType, service.Spec.Type)
				}

				return nil
			}

			// confirming the current east/west gateway service type
			retry.UntilSuccess(validateServiceType, retry.Timeout(20*time.Second))

			annotations := map[string]string{
				"networking.istio.io/service-type": "NodePort",
			}
			ewgw.Annotations = annotations
			_, err = cluster.GatewayAPI().GatewayV1beta1().Gateways(ewgw.GetNamespace()).Update(t.Context(), ewgw, v1.UpdateOptions{})
			if err != nil {
				t.Fatalf("Failed to update east west gateway: %v", err)
			}

			t.Cleanup(func() {
				ewgw, err := cluster.GatewayAPI().GatewayV1beta1().Gateways(peeringNamespace).Get(t.Context(), "istio-eastwest", v1.GetOptions{})
				if err != nil {
					t.Fatalf("Failed to get east west gateway for cleanup: %v", err)
				}

				ewgw.Annotations["networking.istio.io/service-type"] = "LoadBalancer"
				_, err = cluster.GatewayAPI().GatewayV1beta1().Gateways(ewgw.GetNamespace()).Update(t.Context(), ewgw, v1.UpdateOptions{})
				if err != nil {
					t.Fatalf("Failed to update east west gateway for cleanup: %v", err)
				}
			})

			serviceType = "NodePort"
			retry.UntilSuccess(validateServiceType, retry.Timeout(2*time.Minute))
		})
}
