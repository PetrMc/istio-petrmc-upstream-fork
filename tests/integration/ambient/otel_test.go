//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

// Package otelcollector allows testing a variety of tracing solutions by
// employing an OpenTelemetry collector that exposes receiver endpoints for
// various protocols and forwards the spans to a Zipkin backend (for further
// querying and inspection).
package ambient

import (
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"istio.io/api/label"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/istioctl"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/components/opentelemetry"
	"istio.io/istio/pkg/test/framework/components/zipkin"
	"istio.io/istio/pkg/test/framework/resource/config/apply"
	kubetest "istio.io/istio/pkg/test/kube"
	"istio.io/istio/pkg/test/util/retry"
)

const (
	bookinfoDir     = "../../../samples/bookinfo/"
	bookinfoFile    = bookinfoDir + "platform/kube/bookinfo.yaml"
	defaultDestRule = bookinfoDir + "networking/destination-rule-all.yaml"
	bookinfoGateway = bookinfoDir + "networking/bookinfo-gateway.yaml"
	routingV1       = bookinfoDir + "networking/virtual-service-all-v1.yaml"
	headerRouting   = bookinfoDir + "networking/virtual-service-reviews-test-v2.yaml"
)

func TestDistributedTracing(t *testing.T) {
	framework.
		NewTest(t).
		Run(func(t framework.TestContext) {
			nsConfig, err := namespace.New(t, namespace.Config{
				Prefix: "bookinfo",
				Inject: false,
				Labels: map[string]string{
					label.IoIstioDataplaneMode.Name: constants.DataplaneModeAmbient,
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			setupBookinfo(t, nsConfig)
			applyDefaultRouting(t, nsConfig)
			applyFileOrFail(t, nsConfig.Name(), bookinfoDir+"networking/virtual-service-reviews-v3.yaml")
			t.ConfigIstio().YAML("istio-system", `apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: mesh-default
  namespace: istio-system
spec:
  tracing:
    - providers:
        - name: "zipkin"
      randomSamplingPercentage: 100`).ApplyOrFail(t, apply.CleanupConditionally)

			ingressClient := http.Client{}
			ingressInst := istio.DefaultIngressOrFail(t, t)
			addrs, ingrPorts := ingressInst.HTTPAddresses()
			ingressURL := fmt.Sprintf("http://%v:%v", addrs[0], ingrPorts[0])
			// get a closure capable of sending traffic to our ingress
			trafficToIngress := func(t framework.TestContext) {
				ingressTraffic(t, ingressClient, ingressURL)
			}

			zipkinInst, err := zipkin.New(t, zipkin.Config{Cluster: t.Clusters().Default(), IngressAddr: addrs[0]})
			if err != nil {
				t.Fatalf("error adding zipkin: %v", err)
			}

			_, err = opentelemetry.New(t, opentelemetry.Config{Cluster: t.Clusters().Default(), IngressAddr: addrs[0]})
			if err != nil {
				t.Fatalf("error adding an opentelemetry collector: %v", err)
			}

			t.NewSubTest("no waypoint").Run(func(t framework.TestContext) {
				// Test for spans from the ingress gateway
				t.NewSubTest("ingressgateway spans").Run(func(t framework.TestContext) {
					// Otel collector might not be ready to collect from the ingress gateway on the first request.
					// We send requests through the ingress until the trace appears in zipkin.
					// TODO: this can probably be replaced with the standard query now that it accepts a closure to send traffic to ingress
					retry.UntilSuccess(func() error {
						// Send some traffic through the ingress gateway
						ingressTraffic(t, ingressClient, ingressURL)
						// Check for traces in Zipkin. It might be trace from a previous request though which is fine
						// for the purpose of this test.
						queryTag := url.QueryEscape(fmt.Sprintf("istio.canonical_service=istio-ingressgateway and http.url=%s/productpage", ingressURL))
						traces, err := zipkinInst.QueryTraces(300, "", queryTag, "")
						t.Logf("got %d traces with tag [%s]", len(traces), queryTag)
						if err != nil {
							return fmt.Errorf("cannot get traces from zipkin: %v", err)
						}

						// if we're not seeing the traces we want reporting may be slow or istio may still be processing the telemetry API resource
						// error here so we can wait for things to begin syncing at the retry here
						if len(traces) == 0 {
							return fmt.Errorf("got 0 traces")
						}
						return nil
					}, retry.Delay(3*time.Second), retry.Timeout(180*time.Second))
				})

				// Test for spans from the ztunnels
				t.NewSubTest("ztunnel spans").Run(func(ctx framework.TestContext) {
					// likely not reported because of the parsing bug with long responses
					// queryTraces(t, zipkinInst, "component=ztunnel and url.path=/productpage")
					queryTraces(t, zipkinInst, "component=ztunnel and url.path=/details/0", trafficToIngress)
					queryTraces(t, zipkinInst, "component=ztunnel and url.path=/reviews/0", trafficToIngress)
					queryTraces(t, zipkinInst, "component=ztunnel and url.path=/ratings/0", trafficToIngress)
				})
			})

			t.NewSubTest("with waypoint(s)").Run(func(t framework.TestContext) {
				// Deploy a productpage waypint
				setupWaypoint(t, nsConfig)
				ingressTraffic(t, ingressClient, ingressURL)

				// Test for spans from the waypoint

				// ingress no longer sends to waypoint
				// t.NewSubTest("bookinfo-productpage waypoint spans").Run(func(ctx framework.TestContext) {
				// 	queryTraces(t, zipkinInst, "istio.canonical_service=bookinfo-productpage-istio-waypoint")
				// })

				// TODO: configure a waypoint just for reviews and assert that it reports trace? this namespace-scope one could be something other than reviews
				// Test for spans from the waypoint
				t.NewSubTest("bookinfo-reviews waypoint spans").Run(func(ctx framework.TestContext) {
					queryTraces(t, zipkinInst, "istio.canonical_service=waypoint", trafficToIngress)
				})
			})
		})
}

func queryTraces(t framework.TestContext, zipkinInst zipkin.Instance, annotationQuery string, sendTraffic func(t framework.TestContext)) {
	retry.UntilSuccessOrFail(t, func() error {
		queryTag := url.QueryEscape(annotationQuery)
		traces, err := zipkinInst.QueryTraces(300, "", queryTag, "")
		t.Logf("got %d traces with tag [%s]", len(traces), queryTag)
		if err != nil {
			sendTraffic(t)
			return fmt.Errorf("cannot get traces from zipkin: %v", err)
		}

		return nil
	}, retry.Delay(3*time.Second), retry.Timeout(90*time.Second))
}

func ingressTraffic(t framework.TestContext, ingressClient http.Client, ingressURL string) {
	t.NewSubTest("traffic to productpage through ingress").Run(func(t framework.TestContext) {
		retry.UntilSuccessOrFail(t, func() error {
			resp, err := ingressClient.Get(ingressURL + "/productpage")
			if err != nil {
				return fmt.Errorf("error fetching /productpage: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("expect status code %v, got %v", http.StatusFound, resp.StatusCode)
			}
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("error reading /productpage response: %v", err)
			}
			reviewsFound := strings.Contains(string(bodyBytes), "reviews-v3")
			detailsFound := strings.Contains(string(bodyBytes), "Book Details")
			if !reviewsFound || !detailsFound {
				return fmt.Errorf("productpage could not reach other service(s), reviews reached:%v details reached:%v", reviewsFound, detailsFound)
			}

			return nil
		}, retry.Delay(1*time.Second), retry.Timeout(30*time.Second))
	})
}

// These got removed by upstream... this test relies on them.
// TODO: Fix this!
func applyDefaultRouting(t framework.TestContext, nsConfig namespace.Instance) {
	applyFileOrFail(t, nsConfig.Name(), defaultDestRule)
	applyFileOrFail(t, nsConfig.Name(), bookinfoGateway)
	applyFileOrFail(t, nsConfig.Name(), routingV1)
}

func setupBookinfo(t framework.TestContext, nsConfig namespace.Instance) {
	applyFileOrFail(t, nsConfig.Name(), bookinfoFile)
	bookinfoErr := retry.UntilSuccess(func() error {
		if _, err := kubetest.CheckPodsAreReady(kubetest.NewPodFetch(t.AllClusters()[0], nsConfig.Name(), "")); err != nil {
			return fmt.Errorf("bookinfo pods are not ready: %v", err)
		}
		return nil
	}, retry.Timeout(time.Minute*2), retry.BackoffDelay(time.Millisecond*500))
	if bookinfoErr != nil {
		t.Fatal(bookinfoErr)
	}
}

func setupWaypoint(t framework.TestContext, nsConfig namespace.Instance) {
	istioctl.NewOrFail(t, istioctl.Config{}).InvokeOrFail(t, []string{
		"waypoint",
		"apply",
		"--namespace",
		nsConfig.Name(),
		"--enroll-namespace",
		"true",
		"--wait",
	})
}

// applyFileOrFail applys the given yaml file and deletes it during context cleanup
func applyFileOrFail(t framework.TestContext, ns, filename string) {
	t.Helper()
	if err := t.ConfigIstio().File(ns, filename).Apply(apply.NoCleanup); err != nil {
		t.Fatal(err)
	}
}
