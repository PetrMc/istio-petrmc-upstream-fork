// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package bootstrap

import (
	"context"
	"errors"
	"net"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/slices"
)

const (
	ExposeIstiodLabel     = "istio.io/expose-istiod"
	ExposeIstiodModeLabel = "istio.io/expose-istiod-mode"
)

func fetchIstiodURL(kc kube.CLIClient, p Printer, external bool) (string, error) {
	// TODO: Read Gateway as well

	svcs, err := kc.Kube().CoreV1().Services(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{
		LabelSelector: ExposeIstiodLabel,
	})
	if err != nil {
		return "", err
	}

	// Sort services, so we prefer internal but allow external for internal workloads.
	// Note: in some environments, internal workloads cannot reach external services, but usually this is good enough.
	slices.SortBy(svcs.Items, func(s v1.Service) int {
		if s.Labels[ExposeIstiodModeLabel] == "internal" {
			return -1
		}
		return 0
	})
	for _, svc := range svcs.Items {
		if mode := svc.Labels[ExposeIstiodModeLabel]; mode != "" {
			if external && mode == "internal" {
				p.Detailsf("Service %q skipped; it is reachable internally only, but we are registering an external workload", svc.Name)
				continue
			}
		}
		addr := extractExternalAddress(&svc)
		if addr == "" {
			p.Detailsf("Service %q marked as providing Istiod access but has no address assigned", svc.Name)
		} else {
			port := svc.Labels[ExposeIstiodLabel]
			p.Detailsf("Service %q provides Istiod access on port %s", svc.Name, port)
			return "https://" + net.JoinHostPort(addr, port), nil
		}
	}

	// TODO: rev
	// TODO: system namespace
	istiod, err := kc.Kube().CoreV1().Services("istio-system").Get(context.Background(), "istiod", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	istiodAddress := extractExternalAddress(istiod)
	if istiodAddress == "" {
		p.Detailsf("Service 'istiod' is not exposed externally")
	} else {
		p.Detailsf("Service 'istiod' is exposed externally, connecting directly")
		// TODO: do not hardcode port?
		return "https://" + istiodAddress + ":15012", nil
	}

	return "", errors.New("istiod URL could not be detected")
}

func extractExternalAddress(istiod *v1.Service) string {
	var base string
	if len(istiod.Status.LoadBalancer.Ingress) > 0 {
		if ip := istiod.Status.LoadBalancer.Ingress[0].IP; ip != "" {
			base = ip
		} else if hn := istiod.Status.LoadBalancer.Ingress[0].Hostname; hn != "" {
			base = hn
		}
	} else if len(istiod.Spec.ExternalIPs) > 0 {
		base = istiod.Spec.ExternalIPs[0]
	}
	return base
}
