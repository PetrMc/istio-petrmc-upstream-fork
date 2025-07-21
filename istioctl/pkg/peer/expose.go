// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"istio.io/api/label"
	"istio.io/istio/istioctl/pkg/cli"
	labelutil "istio.io/istio/pilot/pkg/serviceregistry/util/label"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/maps"
	networkid "istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
)

func getFlagIfSet[T any](name string, val T, fs *pflag.FlagSet) *T {
	if fs.Changed(name) {
		return &val
	}
	return nil
}

func Expose(ctx cli.Context) *cobra.Command {
	const waitTimeout = 90 * time.Second
	var waitReady bool
	var generate bool
	var region string
	var zone string
	var revision string

	cmd := &cobra.Command{
		Use:   "expose",
		Short: "Expose the cluster for remote access",
		RunE: func(cmd *cobra.Command, args []string) error {
			kubeClient, err := ctx.CLIClient()
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes client: %v", err)
			}
			network, err := fetchNetwork(ctx.IstioNamespace(), kubeClient)
			if err != nil {
				return fmt.Errorf("failed to find network: %v", err)
			}
			clusterID, err := fetchCluster(ctx.IstioNamespace(), kubeClient, revision)
			if err != nil {
				clusterID = cluster.ID(network)
			}
			region, zone, suggestion, err := fetchLocality(
				getFlagIfSet("region", region, cmd.Flags()),
				getFlagIfSet("zone", zone, cmd.Flags()),
				kubeClient)
			if err != nil {
				return fmt.Errorf("failed to determine locality: %v", err)
			}
			gw := makeExposeGateway(ctx, network, clusterID, region, zone, !generate)
			if generate {
				b, err := yaml.Marshal(gw)
				if err != nil {
					return err
				}
				// strip junk
				res := strings.ReplaceAll(string(b), `  creationTimestamp: null
`, "")
				res = strings.ReplaceAll(res, `status: {}
`, "")
				if suggestion != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "# %s\n", suggestion)
				}
				_, _ = fmt.Fprint(cmd.OutOrStdout(), res)
				return nil
			}
			if suggestion != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", suggestion)
			}
			gwc := kubeClient.GatewayAPI().GatewayV1().Gateways(ctx.NamespaceOrDefault(ctx.Namespace()))
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
					return fmt.Errorf("missing Kubernetes Gateway CRDs need to be installed before applying a gateway: %v", err)
				}
				return err
			}

			if waitReady {
				startTime := time.Now()
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()
				for range ticker.C {
					programmed := false
					gwc, err := kubeClient.GatewayAPI().GatewayV1().Gateways(ctx.NamespaceOrDefault(ctx.Namespace())).Get(context.TODO(), gw.Name, metav1.GetOptions{})
					if err == nil {
						// Check if gateway has Programmed condition set to true
						for _, cond := range gwc.Status.Conditions {
							if cond.Type == string(gateway.GatewayConditionProgrammed) && string(cond.Status) == "True" {
								programmed = true
								break
							}
						}
					}
					if programmed {
						break
					}
					if time.Since(startTime) > waitTimeout {
						return errorWithMessage("timed out while waiting for waypoint", gwc, err)
					}
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Gateway %v/%v applied\n", gw.Namespace, gw.Name)

			return nil
		},
	}
	cmd.Flags().BoolVarP(&waitReady, "wait", "w", false, "Wait for the gateway to be ready")
	cmd.Flags().BoolVar(&generate, "generate", generate, "Instead of applying the gateway, just emit the YAML configuration")
	cmd.Flags().StringVar(&region, "region", "", "Region to mark this cluster as. If not set, will be auto-detected")
	cmd.Flags().StringVar(&zone, "zone", "", "Zone to mark this cluster as. Should only be set if the cluster runs in a single zone.")
	cmd.Flags().StringVarP(&revision, "revision", "r", "", "The revision to use")
	return cmd
}

func fetchLocality(region *string, zone *string, client kube.CLIClient) (string, string, string, error) {
	if region != nil {
		// Region is set, use it. Use zone if it is set too, else nothing
		return *region, ptr.OrDefault(zone, ""), "", nil
	}
	if zone != nil {
		return "", "", "", fmt.Errorf("--region must be set when using --zone")
	}

	nodes, err := client.Kube().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return "", "", "", fmt.Errorf("failed to list nodes: %v", err)
	}
	return localityFromNodes(nodes.Items)
}

func localityFromNodes(nodes []corev1.Node) (string, string, string, error) {
	regions := map[string]int{}
	regionZones := map[string]int{}
	for _, node := range nodes {
		reg, f := node.Labels[labelutil.LabelTopologyRegion]
		if !f {
			continue
		}
		z := node.Labels[labelutil.LabelTopologyZone]
		regions[reg]++
		regionZones[reg+"/"+z]++
	}
	if len(regions) == 0 {
		// no regions detected at all, we will leave everything unset
		return "", "", "", nil
	}
	if len(regions) > 1 {
		warning := fmt.Sprintf("multiple regions found, so could not auto detect region: %+v", regions)
		return "", "", warning, nil
	}
	selectedRegion := maps.Keys(regions)[0]
	if len(regionZones) > 1 {
		warning := fmt.Sprintf("multiple zones found, so did not set a zone. This can be overridden with --zone. Detected: %+v", regionZones)
		return selectedRegion, "", warning, nil
	}
	selectedRegionZone := maps.Keys(regionZones)[0]
	_, selectedZone, _ := strings.Cut(selectedRegionZone, "/")
	return selectedRegion, selectedZone, "", nil
}

func makeExposeGateway(
	ctx cli.Context,
	network networkid.ID,
	clusterid cluster.ID,
	region, zone string,
	forApply bool,
) *gateway.Gateway {
	ns := ctx.NamespaceOrDefault(ctx.Namespace())
	if ctx.Namespace() == "" && !forApply {
		ns = ""
	}
	lbls := map[string]string{
		label.TopologyNetwork.Name:  string(network),
		label.TopologyCluster.Name:  string(clusterid),
		constants.ExposeIstiodLabel: "15012",
	}
	if region != "" {
		lbls[labelutil.LabelTopologyRegion] = region
	}
	if zone != "" {
		lbls[labelutil.LabelTopologyZone] = zone
	}
	gw := gateway.Gateway{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.KubernetesGateway_v1.Kind,
			APIVersion: gvk.KubernetesGateway_v1.GroupVersion(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-eastwest",
			Namespace: ns,
			Labels:    lbls,
		},
		Spec: gateway.GatewaySpec{
			GatewayClassName: "istio-eastwest",
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

func errorWithMessage(errMsg string, gwc *gateway.Gateway, err error) error {
	errorMsg := fmt.Sprintf("%s\t%v/%v", errMsg, gwc.Namespace, gwc.Name)
	if err != nil {
		errorMsg += fmt.Sprintf(": %s", err)
	}
	return errors.New(errorMsg)
}
