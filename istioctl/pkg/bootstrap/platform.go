// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package bootstrap

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/istio/pkg/kube"
	"istio.io/istio/platform/discovery/ecs"
)

func validateECS(kc kube.CLIClient, p Printer, res BootstrapToken) error {
	sa, err := kc.Kube().CoreV1().ServiceAccounts(res.Namespace).Get(context.Background(), res.ServiceAccount, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("could not retrieve service account %v/%v: %v", res.Namespace, res.ServiceAccount, err)
	}
	role := sa.Annotations[ecs.SoloRoleArnAnnotation]
	if role == "" {
		role = sa.Annotations[ecs.AwsRoleArnAnnotation]
	}
	if role == "" {
		return fmt.Errorf("role not found in service account %v/%v (requires 'ecs.solo.io/role-arn' annotation)", res.Namespace, res.ServiceAccount)
	}
	p.Writef("Workload is authorized to run as role %q", role)
	return nil
}
