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
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gateway "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/ptr"
)

func Expose(ctx cli.Context) *cobra.Command {
	const waitTimeout = 90 * time.Second
	var waitReady bool
	var generate bool
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
				return fmt.Errorf("failed to find network")
			}
			gw := makeExposeGateway(ctx, network, !generate)
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
				fmt.Fprint(cmd.OutOrStdout(), res)
				return nil
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
					return fmt.Errorf("missing Kubernetes Gateway CRDs need to be installed before applying a gateway: %s", err)
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
			fmt.Fprintf(cmd.OutOrStdout(), "Gateway %v/%v applied\n", gw.Namespace, gw.Name)

			return nil
		},
	}
	cmd.Flags().BoolVarP(&waitReady, "wait", "w", false, "Wait for the gateway to be ready")
	cmd.Flags().BoolVar(&generate, "generate", generate, "Instead of applying the gateway, just emit the YAML configuration")
	return cmd
}

func makeExposeGateway(ctx cli.Context, network string, forApply bool) *gateway.Gateway {
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
			Name:      "istio-eastwest",
			Namespace: ns,
			Labels: map[string]string{
				"topology.istio.io/network": network,
				"istio.io/expose-istiod":    "15012",
			},
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
