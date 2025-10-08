// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"

	"istio.io/api/annotation"
	clientnetworking "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/kubetypes"
)

type TargetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// PrometheusBridge exposes ECS task information to Prometheus.
// Prometheus does not have native ECS service discovery. However, we do ECS SD already as part of normal operations!
// Prometheus can do SD over HTTP (https://prometheus.io/docs/prometheus/latest/http_sd/).
// PrometheusBridge serves these HTTP requests to Prometheus to allow it to discovery the ECS tasks.
// Prometheus config should look something like:
//
//	 scrape_configs:
//	- job_name: ecs
//	  http_sd_configs:
//	  - url: https://istiod.istio-system.svc/prometheus-discovery
//	    refresh_interval: 60s
//	    tls_config:
//	      insecure_skip_verify: true
type PrometheusBridge struct {
	client          kube.Client
	workloadEntries kclient.Client[*clientnetworking.WorkloadEntry]
}

// AddPrometheusServiceDiscovery exposes the /prometheus-discovery endpoint to the mux.
func AddPrometheusServiceDiscovery(client kube.Client, mux *http.ServeMux) {
	c := PrometheusBridge{
		client: client,
		workloadEntries: kclient.NewFiltered[*clientnetworking.WorkloadEntry](client, kubetypes.Filter{
			ObjectFilter: kubetypes.NewStaticObjectFilter(func(raw any) bool {
				// We only want to handle ECS WorkloadEntry that are mesh enabled (have a ztunnel)
				obj := controllers.ExtractObject(raw)
				_, lf := obj.GetLabels()[ServiceLabel]
				mesh := obj.GetAnnotations()[annotation.AmbientRedirection.Name] == constants.AmbientRedirectionEnabled

				return lf && mesh
			}),
		}),
	}
	mux.HandleFunc("/prometheus-discovery", c.Serve)
}

func (c *PrometheusBridge) Serve(w http.ResponseWriter, req *http.Request) {
	groups := []TargetGroup{}
	for _, w := range c.workloadEntries.List(metav1.NamespaceAll, klabels.Everything()) {
		if w.Spec.Address == "" {
			continue
		}
		// TODO: filter cross-network?
		labels := map[string]string{
			"__meta_ecs_service": w.GetLabels()[ServiceLabel],
		}
		_, cluster, namespace, arn, err := ParseResourceName(w.Annotations[ResourceAnnotation])
		if err == nil {
			labels["__meta_ecs_arn"] = arn
			labels["__meta_ecs_cluster"] = cluster
			labels["__meta_ecs_namespace"] = namespace
			sp := strings.Split(arn, "/")
			labels["__meta_ecs_task"] = sp[len(sp)-1]
		}
		g := TargetGroup{
			// Assume hardcoded 15020 for ztunnel metrics.
			Targets: []string{net.JoinHostPort(w.Spec.Address, "15020")},
			Labels:  labels,
		}
		groups = append(groups, g)
	}
	w.Header().Add("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(groups); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}
