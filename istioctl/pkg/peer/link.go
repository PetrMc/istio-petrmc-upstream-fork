// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peer

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"istio.io/istio/istioctl/pkg/bootstrap"
	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/util/sets"
)

func Link(ctx cli.Context) *cobra.Command {
	var contexts []string
	var to []string
	var from []string
	var generate bool
	var revision string

	cmd := &cobra.Command{
		Use:   "link",
		Short: "link clusters together for multi-cluster discovery",
		Example: `	# Link three clusters (kind-alpha, kind-beta, and kind-gamma) together
	istioctl multicluster link --contexts=kind-alpha,kind-beta,kind-gamma

	# Link one cluster to another
	# This means cluster alpha can reach cluster beta, but not the other direction
	istioctl multicluster link --from alpha --to beta

	# Just generate the config to link to the current cluster, which can then be applied to another cluster
	# This will enable 'source-cluster' to reach 'target-cluster'.
	istioctl multicluster link --generate --context target-cluster | kubectl apply -f - --context=source-cluster
`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("expected zero args")
			}
			if len(contexts) > 0 && (len(to) > 0 || len(from) > 0) {
				return fmt.Errorf("--contexts and --to/--from cannot be used together")
			}
			if (len(to) > 0) != (len(from) > 0) {
				return fmt.Errorf("--to requires --from")
			}
			if ((len(contexts) + len(to) + len(from)) == 0) && !generate {
				return fmt.Errorf("at least one of --generate, --contexts, or --to/--from must be provided")
			}
			if ((len(contexts) + len(to) + len(from)) > 0) && generate {
				return fmt.Errorf("--generate cannot be used with --contexts, --to, or --from")
			}
			ctxCopy := contexts[:]
			slices.Sort(ctxCopy)
			ctxCopy = slices.Compact(ctxCopy)
			if len(ctxCopy) < len(contexts) {
				return fmt.Errorf("--contexts cannot contain duplicate contexts")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if generate {
				return linkGenerate(ctx, cmd, revision)
			}
			if len(contexts) > 0 {
				from = contexts
				to = contexts
			}
			clusterClients, err := buildClients(from, to, ctx)
			if err != nil {
				return err
			}
			for _, dstName := range to {
				dst := clusterClients[dstName]
				gwName, addr, err := findGateway(dst)
				if err != nil {
					return fmt.Errorf("in cluster %v: %v", dstName, err)
				}
				network, err := fetchNetwork(ctx.IstioNamespace(), dst)
				if err != nil {
					return fmt.Errorf("in cluster %v: %v", dstName, err)
				}
				td, err := fetchTrustDomain(dst, ctx.IstioNamespace(), revision)
				if err != nil {
					return fmt.Errorf("in cluster %v: %v", dstName, err)
				}
				for _, srcName := range from {
					src := clusterClients[srcName]
					if dstName == srcName {
						// Do not link to ourselves
						continue
					}
					gw := makeLinkGateway(ctx, gwName, addr, network, td, true)
					gwc := src.GatewayAPI().GatewayV1().Gateways(ctx.NamespaceOrDefault(ctx.Namespace()))
					b, err := yaml.Marshal(gw)
					if err != nil {
						return err
					}
					_, err = gwc.Patch(context.Background(), gw.Name, types.ApplyPatchType, b, metav1.PatchOptions{
						Force:        nil,
						FieldManager: "istioctl",
					})
					if err != nil {
						if kerrors.IsNotFound(err) {
							return fmt.Errorf("in cluster %v: missing Kubernetes Gateway CRDs need to be installed before applying a gateway: %s", srcName, err)
						}
						return fmt.Errorf("in cluster %v: %v", srcName, err)
					}
					fmt.Fprintf(cmd.OutOrStdout(),
						"Gateway %v/%v applied to cluster %q pointing to cluster %q (network %q)\n",
						gw.Namespace, gw.Name, srcName, dstName, network)
				}
			}
			return nil
		},
	}
	cmd.PersistentFlags().StringSliceVar(&contexts, "contexts", contexts, "list of contexts to join bi-directionally")
	cmd.PersistentFlags().StringSliceVar(&to, "to", to, "list of contexts to link each 'from' cluster to")
	cmd.PersistentFlags().StringSliceVar(&from, "from", from, "list of contexts that will be linked to each 'to' cluster")
	cmd.PersistentFlags().BoolVar(&generate, "generate", generate, "just generate the config required to connect to this cluster")
	cmd.PersistentFlags().StringVarP(&revision, "revision", "r", "", "The revision to use")
	return cmd
}

func linkGenerate(ctx cli.Context, cmd *cobra.Command, revision string) error {
	clt, err := ctx.CLIClient()
	if err != nil {
		return err
	}
	gwName, addr, err := findGateway(clt)
	if err != nil {
		return err
	}
	network, err := fetchNetwork(ctx.IstioNamespace(), clt)
	if err != nil {
		return err
	}
	td, err := fetchTrustDomain(clt, ctx.IstioNamespace(), revision)
	if err != nil {
		return err
	}
	gw := makeLinkGateway(ctx, gwName, addr, network, td, false)
	b, err := yaml.Marshal(gw)
	if err != nil {
		return err
	}
	// strip junk
	res := strings.ReplaceAll(string(b), `  creationTimestamp: null
`, "")
	res = strings.ReplaceAll(res, `status: {}
`, "")
	fmt.Fprint(cmd.OutOrStdout(), res)
	return nil
}

func buildClients(from []string, to []string, ctx cli.Context) (map[string]kube.CLIClient, error) {
	allClusters := sets.New[string](from...).InsertAll(to...)
	clusterNames := sets.SortedList(allClusters)
	clusterClients := map[string]kube.CLIClient{}
	clusters, err := ctx.CLIClientsForContexts(clusterNames)
	if err != nil {
		return nil, err
	}
	if len(clusters) != len(clusterNames) {
		return nil, fmt.Errorf("bug: cluster count unexpected")
	}
	for i, clt := range clusters {
		n := clusterNames[i]
		clusterClients[n] = clt
	}
	return clusterClients, nil
}

func makeLinkGateway(ctx cli.Context, gwName string, addr gateway.GatewaySpecAddress, network string, trustDomain string, forApply bool) *gateway.Gateway {
	ns := ctx.NamespaceOrDefault(ctx.Namespace())
	if ctx.Namespace() == "" && !forApply {
		ns = ""
	}
	gw := gateway.Gateway{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.KubernetesGateway_v1.Kind,
			APIVersion: gvk.KubernetesGateway_v1.GroupVersion(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-remote-peer-" + network,
			Namespace: ns,
			Annotations: map[string]string{
				// TODO: more precise fetching...
				"gateway.istio.io/service-account": gwName,
				"gateway.istio.io/trust-domain":    trustDomain,
			},
			Labels: map[string]string{
				"topology.istio.io/network": network,
			},
		},
		Spec: gateway.GatewaySpec{
			GatewayClassName: "istio-remote",
			Addresses:        []gateway.GatewaySpecAddress{addr},
			Listeners: []gateway.Listener{
				{
					Name:     "cross-network",
					Port:     gateway.PortNumber(15008),
					Protocol: "HBONE",
					TLS:      &gateway.GatewayTLSConfig{Mode: ptr.Of(gateway.TLSModePassthrough)},
				},
				{
					Name:     "xds-tls",
					Port:     gateway.PortNumber(15012),
					Protocol: gateway.TLSProtocolType,
					TLS:      &gateway.GatewayTLSConfig{Mode: ptr.Of(gateway.TLSModePassthrough)},
				},
			},
		},
	}

	return &gw
}

func findGateway(src kube.CLIClient) (string, gateway.GatewaySpecAddress, error) {
	svcs, err := src.Kube().CoreV1().Services(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{
		LabelSelector: bootstrap.ExposeIstiodLabel,
	})
	if err != nil {
		return "", gateway.GatewaySpecAddress{}, err
	}
	if len(svcs.Items) == 0 {
		return "", gateway.GatewaySpecAddress{}, fmt.Errorf("no services with %q label", bootstrap.ExposeIstiodLabel)
	}
	for _, svc := range svcs.Items {
		if lb := svc.Status.LoadBalancer.Ingress; len(lb) > 0 {
			if lb[0].IP != "" {
				return svc.Name, gateway.GatewaySpecAddress{
					Type:  ptr.Of(gateway.IPAddressType),
					Value: lb[0].IP,
				}, nil
			}
			if lb[0].Hostname != "" {
				return svc.Name, gateway.GatewaySpecAddress{
					Type:  ptr.Of(gateway.HostnameAddressType),
					Value: lb[0].Hostname,
				}, nil
			}
		}
	}
	return "", gateway.GatewaySpecAddress{}, fmt.Errorf("services found with %q label, but none have a load balancer", bootstrap.ExposeIstiodLabel)
}
