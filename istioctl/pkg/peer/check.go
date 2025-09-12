// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/label"
	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/licensing"
)

var verbose bool

func Check(ctx cli.Context) *cobra.Command {
	var precheck bool
	var contexts []string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check the multicluster status of the current cluster",
		Args: func(cmd *cobra.Command, args []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				cliClients []kube.CLIClient
				err        error
			)
			if len(contexts) > 0 {
				cliClients, err = ctx.CLIClientsForContexts(contexts)
			} else {
				var cliClient kube.CLIClient
				cliClient, err = ctx.CLIClient()
				cliClients = []kube.CLIClient{cliClient}
			}
			if err != nil {
				return fmt.Errorf("failed to create k8s client: %w", err)
			}

			allSuccess := true
			for _, cliClient := range cliClients {
				if len(contexts) > 1 {
					fmt.Printf("\n\n=== Cluster: %s ===\n\n", cliClient.ClusterID())
				}

				allSuccess = checkLicense(ctx, cliClient) && allSuccess

				allSuccess = checkPods(cmd, cliClient) && allSuccess

				allSuccess = checkGateways(cliClient) && allSuccess

				allSuccess = checkPeers(cliClient, precheck) && allSuccess
			}
			if !allSuccess {
				if !verbose {
					color.Yellow("Run with --verbose flag to see details")
				}
				return fmt.Errorf("multicluster check found issues")
			}
			return nil
		},
	}
	cmd.PersistentFlags().StringSliceVar(&contexts, "contexts", contexts, "list of contexts to check")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Print extra information about each check")
	cmd.Flags().BoolVarP(&precheck, "precheck", "p", false, "Check for multicluster readiness, ignoring missing multicluster resources")
	return cmd
}

func checkLicense(ctx cli.Context, cliClient kube.CLIClient) (success bool) {
	verbosePrintf("--- License Check ---\n\n")

	responses, err := cliClient.AllDiscoveryDo(context.TODO(), ctx.IstioNamespace(), "/debug/license")
	if err != nil {
		// don't fail outright on older versions of istio without this debug endpoint (handling http 404)
		if strings.Contains(err.Error(), "404") {
			color.Yellow("⚠️  License Check is not supported on Istio versions <1.27")
			return true
		}
		color.Red("❌ License Check: %v", err)
		return false
	}

	allValid := true
	for _, res := range responses {
		info := licensing.LicenseInfo{}
		err := json.Unmarshal(res, &info)
		if err != nil {
			info.Product = "unknown"
			info.State = "unknown"
		}

		valid := licensing.CheckLicenseByInfo(info, licensing.FeatureMultiCluster, false)
		if !valid {
			allValid = false
		}
	}

	if allValid {
		color.Green("✅ License Check: license is valid for multicluster")
	} else {
		color.Yellow("⚠️  License Check: found invalid license for multicluster")
	}

	return allValid
}

func checkPods(cmd *cobra.Command, cliClient kube.CLIClient) (success bool) {
	var (
		allHealthy   = true
		podSelectors = []struct {
			name string
			opt  metav1.ListOptions
		}{
			{"istiod", metav1.ListOptions{LabelSelector: "app=istiod"}},
			{"ztunnel", metav1.ListOptions{LabelSelector: "app=ztunnel"}},
			{"eastwest gateway", metav1.ListOptions{LabelSelector: label.ServiceCanonicalName.Name + "=istio-eastwest"}},
		}
	)
	for _, s := range podSelectors {
		verbosePrintf("\n\n--- Pod Check (%s) ---\n\n", s.name)

		healthy := true
		pods, err := cliClient.GetIstioPods(context.TODO(), metav1.NamespaceAll, s.opt)
		if err != nil {
			color.Red("❌ Pod Check (%s): %v", s.name, err)
			return false
		}
		if len(pods) == 0 {
			color.Yellow("⚠️  Pod Check (%s): no pods found", s.name)
			continue
		}
		w := new(tabwriter.Writer).Init(cmd.OutOrStdout(), 0, 8, 5, ' ', 0)
		verboseFprintf(w, "NAME\tREADY\tSTATUS\tRESTARTS\tAGE\n")
		for _, pod := range pods {
			if pod.Status.Phase != v1.PodRunning {
				healthy = false
			}

			ready, readyString, restarts := getContainerStats(pod.Status.ContainerStatuses)
			if !ready {
				healthy = false
			}

			verboseFprintf(w, "%v\t%v\t%v\t%v\t%v\n",
				pod.Name, readyString, pod.Status.Phase, restarts, time.Since(pod.CreationTimestamp.Time).Round(time.Second),
			)
		}
		w.Flush()

		verbosePrint("\n")
		if healthy {
			color.Green("✅ Pod Check (%s): all pods healthy", s.name)
		} else {
			allHealthy = false
			color.Yellow("⚠️  Pod Check (%s): found unhealthy pods", s.name)
		}
	}

	return allHealthy
}

func getContainerStats(statuses []v1.ContainerStatus) (ready bool, readyString string, restarts int32) {
	readyCount := 0
	for _, s := range statuses {
		if s.Ready {
			readyCount++
		}
		restarts += s.RestartCount
	}
	return readyCount == len(statuses), fmt.Sprintf("%d/%d", readyCount, len(statuses)), restarts
}

func checkGateways(cliClient kube.CLIClient) (success bool) {
	verbosePrintf("\n\n--- Gateway Check ---\n\n")

	gwc := cliClient.GatewayAPI().GatewayV1().Gateways(v1.NamespaceAll)
	gwl, err := gwc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		color.Red("❌ Gateway Check: %v", err)
		return false
	}

	var (
		found         bool
		allProgrammed = true
	)
	for _, gw := range gwl.Items {
		if gw.Spec.GatewayClassName != "istio-eastwest" {
			continue
		}
		found = true

		verbosePrintf("Gateway: %s\n", gw.Name)
		verbosePrint("Addresses:\n")
		for _, address := range gw.Status.Addresses {
			verbosePrintf("- %s\n", address.Value)
		}
		for _, condition := range gw.Status.Conditions {
			if condition.Type != "Programmed" {
				continue
			}
			if condition.Status == metav1.ConditionTrue {
				verbosePrint(color.GreenString("Status: programmed ✅\n"))
			} else {
				verbosePrint(color.YellowString("Status: not programmed ⚠️\n"))
				allProgrammed = false
			}
			break
		}
		verbosePrint("\n")
	}

	if found {
		if allProgrammed {
			color.Green("✅ Gateway Check: all eastwest gateways programmed")
		} else {
			color.Yellow("⚠️  Gateway Check: found unprogrammed eastwest gateway(s)")
		}
	} else {
		color.Yellow("⚠️  Gateway Check: no configured eastwest gateways")
	}

	return found && allProgrammed
}

func checkPeers(cliClient kube.CLIClient, precheck bool) (success bool) {
	verbosePrintf("\n\n--- Peers Check ---\n\n")

	gwc := cliClient.GatewayAPI().GatewayV1().Gateways(v1.NamespaceAll)
	gwl, err := gwc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		color.Red("❌ Peers Check: %v", err)
		return false
	}

	var (
		found        bool
		allConnected = true
	)
	for _, gw := range gwl.Items {
		if gw.Spec.GatewayClassName != "istio-remote" {
			continue
		}
		found = true

		peerName, ok := gw.Labels[label.TopologyNetwork.Name]
		if !ok {
			peerName = "unknown"
		}
		verbosePrintf("Cluster: %s\n", peerName)
		verbosePrint("Addresses:\n")
		for _, address := range gw.Status.Addresses {
			verbosePrintf("- %s\n", address.Value)
		}

		verbosePrint("Conditions:\n")
		connected := true
		for _, condition := range gw.Status.Conditions {
			if condition.Status == metav1.ConditionTrue {
				verbosePrint(color.GreenString("- %s: %v\n", condition.Type, condition.Status))
			} else {
				verbosePrint(color.YellowString("- %s: %v\n", condition.Type, condition.Status))
			}
			if condition.Type == constants.SoloConditionPeeringSucceeded || condition.Type == constants.SoloConditionPeerConnected {
				connected = connected && condition.Status == metav1.ConditionTrue
			}
		}
		if connected {
			verbosePrint(color.GreenString("Status: connected ✅\n"))
		} else {
			allConnected = false
			verbosePrint(color.YellowString("Status: disconnected ⚠️\n"))
		}
		verbosePrint("\n")
	}

	if found {
		if allConnected {
			color.Green("✅ Peers Check: all clusters connected")
		} else {
			color.Yellow("⚠️  Peers Check: found disconnected cluster(s)")
		}
	} else {
		color.Yellow("⚠️  Peers Check: no configured peers")
	}

	if precheck {
		return true
	}
	return found && allConnected
}

// some simple wrappers around fmt.Print* to make handling the verbose flag easier

func verbosePrint(a ...any) {
	if verbose {
		fmt.Print(a...)
	}
}

func verbosePrintf(format string, a ...any) {
	if verbose {
		fmt.Printf(format, a...)
	}
}

func verboseFprintf(w io.Writer, format string, a ...any) {
	if verbose {
		fmt.Fprintf(w, format, a...)
	}
}
