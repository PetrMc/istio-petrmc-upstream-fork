// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peer

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/label"
	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/kube"
	networkid "istio.io/istio/pkg/network"
)

func Cmd(ctx cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "multicluster",
		Short: "Multicluster commands",
		Example: `  # Generate a bootstrap token for the 'productpage' service account and use it to launch a ztunnel instance.
  BOOTSTRAP_TOKEN=$(istioctl bootstrap productpage) ztunnel

  # Generate a bootstrap token for the 'productpage' service account in the namespace 'bookinfo'.
  istioctl bootstrap productpage --namespace bookinfo

  # Generate a bootstrap token for the 'productpage' service account.
  # Our workload will run somewhere without direct network connectivity. Note: this requires an east-west gateway setup.
  istioctl bootstrap productpage --external
`,
	}
	cmd.AddCommand(Expose(ctx))
	cmd.AddCommand(Link(ctx))
	return cmd
}

func fetchNetwork(istioNamespace string, kc kube.CLIClient) (networkid.ID, error) {
	systemNS, err := kc.Kube().CoreV1().Namespaces().Get(context.Background(), istioNamespace, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	network := systemNS.Labels[label.TopologyNetwork.Name]
	if network == "" {
		return "", fmt.Errorf("no %q label found", label.TopologyNetwork.Name)
	}
	return networkid.ID(network), nil
}

func fetchCluster(istioNamespace string, kc kube.CLIClient, revision string) (cluster.ID, error) {
	ls := "app=istiod"
	if revision != "" {
		ls += label.IoIstioRev.Name + "=" + revision
	}
	istiodDeployment, err := kc.Kube().AppsV1().Deployments(istioNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: ls,
	})
	if err != nil {
		return "", err
	}
	for _, deployment := range istiodDeployment.Items {
		for _, c := range deployment.Spec.Template.Spec.Containers {
			if c.Name != "discovery" {
				continue
			}
			for _, env := range c.Env {
				if env.Name == "CLUSTER_ID" {
					if env.Value == "Kubernetes" {
						// Do not let them use the default name, else we would likely end up with duplicates
						continue
					}
					return cluster.ID(env.Value), nil
				}
			}
		}
	}

	return "", fmt.Errorf("no cluster found")
}

func fetchTrustDomain(kc kube.CLIClient, istioNamespace string, revision string) (string, error) {
	meshConfigMap, err := getMeshConfigMap(istioNamespace, kc, revision)
	if err != nil {
		return "", err
	}
	// values in the data are strings, while proto might use a
	// different data type.  therefore, we have to get a value by a
	// key
	configYaml, exists := meshConfigMap.Data["mesh"]
	if !exists {
		return "", fmt.Errorf("missing configuration map key %q", "mesh")
	}
	cfg, err := mesh.ApplyMeshConfigDefaults(configYaml)
	if err != nil {
		return "", err
	}
	return cfg.TrustDomain, err
}

func getMeshConfigMap(istioNamespace string, kc kube.CLIClient, revision string) (*corev1.ConfigMap, error) {
	if revision != "" {
		cmName := "istio"
		if revision != "default" {
			cmName = fmt.Sprintf("istio-%s", revision)
		}
		meshConfigMap, err := kc.Kube().CoreV1().ConfigMaps(istioNamespace).Get(context.TODO(), cmName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("could not read valid configmap %q from namespace %q: %v", cmName, istioNamespace, err)
		}
		return meshConfigMap, nil
	}
	// First try 'gloo' revision for ILM support.
	// https://github.com/istio/istio/issues/54518 will automatically handle this in the future.
	meshConfigMap, err := kc.Kube().CoreV1().ConfigMaps(istioNamespace).Get(context.TODO(), "istio-gloo", metav1.GetOptions{})
	if !kerrors.IsNotFound(err) {
		if err != nil {
			return nil, fmt.Errorf("could not read valid configmap %q from namespace %q: %v", "istio-gloo", istioNamespace, err)
		}
		return meshConfigMap, nil
	}
	cmName := "istio"
	meshConfigMap, err = kc.Kube().CoreV1().ConfigMaps(istioNamespace).Get(context.TODO(), cmName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not read valid configmap %q from namespace %q: %v", cmName, istioNamespace, err)
	}
	return meshConfigMap, nil
}
