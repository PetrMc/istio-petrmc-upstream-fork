// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/label"
	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/licensing"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
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

				allSuccess = checkIncompatibleEnvs(ctx, cliClient) && allSuccess

				allSuccess = checkLicense(ctx, cliClient) && allSuccess

				allSuccess = checkPods(cmd, cliClient) && allSuccess

				allSuccess = checkGateways(cliClient) && allSuccess

				allSuccess = checkPeers(cliClient, precheck) && allSuccess
			}

			allSuccess = checkStaleWorkloads(ctx, cliClients) && allSuccess
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

// env -> VALID value
var envsToCheck = map[string]struct {
	wantValue   string
	badValue    string
	mustBeSet   bool
	explanation string
}{
	"K8S_SELECT_WORKLOAD_ENTRIES": {
		wantValue:   "true",
		mustBeSet:   false,
		explanation: "K8S_SELECT_WORKLOAD_ENTRIES cannot be disabled when using peering for multicluster service discovery.",
	},
	"ENABLE_PEERING_DISCOVERY": {
		wantValue:   "true",
		mustBeSet:   true,
		explanation: "ENABLE_PEERING_DISCOVERY must be enabled for multicluster service discovery.",
	},
}

func checkIncompatibleEnvs(ctx cli.Context, cliClient kube.CLIClient) (success bool) {
	verbosePrintf("\n\n--- Incompatible Environment Variable Check ---\n\n")

	allValid := true
	envs, err := fetchIstiodEnvs(ctx.IstioNamespace(), cliClient, "")
	if err != nil {
		color.Red("❌ Failed checking environment variables for istiod: %v", err)
		return false
	}
	for envName, check := range envsToCheck {
		value, isSet := envs[envName]
		valid := true
		if check.mustBeSet && !isSet {
			valid = false
		} else if isSet {
			if check.badValue != "" && value == check.badValue {
				valid = false
			} else if check.wantValue != "" && value != check.wantValue {
				valid = false
			}
		}

		if valid {
			verbosePrintf("✅ Incompatible Environment Variable Check: %s is valid (%q)\n", envName, value)
		} else {
			allValid = false
			color.Yellow("⚠️  Incompatible Environment Variable Check: %s is invalid (%q). %s", envName, value, check.explanation)
		}
	}
	if allValid {
		color.Green("✅ Incompatible Environment Variable Check: all relevant environment variables are valid")
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

type weInfo struct {
	we            *networkingv1.WorkloadEntry
	podNamespace  string
	podName       string
	sourceCluster string
	peeredCluster string
}

// isAutogenflatWE checks if a WorkloadEntry is an autogenflat workload
func isAutogenflatWE(we *networkingv1.WorkloadEntry) bool {
	sourceWorkload, ok := we.Labels["solo.io/source-workload"]
	return ok && strings.HasPrefix(sourceWorkload, "autogenflat.")
}

// parseWorkloadUID parses the peered-workload-uid annotation to extract pod namespace and name
// Expected format: <cluster>/<group>/<kind>/<namespace>/<name>
// Returns namespace, name, and ok (true if valid format for a Pod)
func parseWorkloadUID(uid string) (namespace, name string, ok bool) {
	parts := strings.Split(uid, "/")
	if len(parts) < 5 || parts[2] != "Pod" {
		return "", "", false
	}
	return parts[3], parts[4], true
}

type staleWorkloadsResult struct {
	// staleWorkloads[targetClusterID][]weInfo
	staleWorkloads map[string][]weInfo
	// missingClusters[targetClusterID][missingSourceClusterID]
	missingClusters map[string]map[string]struct{}
	totalChecked    int
}

func findStaleWorkloadsInner(clusterMap map[string]kube.CLIClient, clusterIDs []string, istioNamespace string) (*staleWorkloadsResult, error) {
	// Track which target cluster has resources from which missing source cluster
	// missingClusters[targetClusterID][missingSourceClusterID]
	missingClusters := make(map[string]map[string]struct{})

	// Pass 1: Collect all WorkloadEntries organized by source cluster and namespace
	// peeredWorkloads[sourceClusterID][namespace][]weInfo
	peeredWorkloads := make(map[string]map[string][]weInfo)

	for _, clusterID := range clusterIDs {
		targetClient := clusterMap[clusterID]

		// Get all WorkloadEntries in peering namespace
		weList, err := targetClient.Istio().NetworkingV1().WorkloadEntries(istioNamespace).
			List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list WorkloadEntries in %s: %w", clusterID, err)
		}

		for _, we := range weList.Items {
			// Filter for autogenflat WEs
			if !isAutogenflatWE(we) {
				continue
			}

			sourceCluster, ok := we.Labels["solo.io/source-cluster"]
			if !ok {
				continue
			}

			// Skip if source cluster not in our contexts
			if _, found := clusterMap[sourceCluster]; !found {
				if missingClusters[clusterID] == nil {
					missingClusters[clusterID] = make(map[string]struct{})
				}
				missingClusters[clusterID][sourceCluster] = struct{}{}
				continue
			}

			// Parse the UID annotation to get Pod namespace/name
			uid, ok := we.Annotations["solo.io/peered-workload-uid"]
			if !ok {
				continue
			}

			podNamespace, podName, ok := parseWorkloadUID(uid)
			if !ok {
				continue
			}

			// Add to peeredWorkloads
			if peeredWorkloads[sourceCluster] == nil {
				peeredWorkloads[sourceCluster] = make(map[string][]weInfo)
			}
			peeredWorkloads[sourceCluster][podNamespace] = append(
				peeredWorkloads[sourceCluster][podNamespace],
				weInfo{
					we:            we,
					podNamespace:  podNamespace,
					podName:       podName,
					sourceCluster: sourceCluster,
					peeredCluster: clusterID,
				},
			)
		}
	}

	// Pass 2: For each source cluster/namespace, List pods and check for stale WEs
	// staleWorkloads[targetClusterID][]weInfo
	staleWorkloads := make(map[string][]weInfo)
	totalChecked := 0

	for sourceCluster, namespaceMap := range peeredWorkloads {
		sourceClient := clusterMap[sourceCluster]

		for namespace, wes := range namespaceMap {
			// Find existing pods for this namespace
			podList, err := sourceClient.Kube().CoreV1().Pods(namespace).
				List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				// Skip this namespace on error, but continue with others
				continue
			}
			existingPods := sets.New(slices.Map(podList.Items, func(p v1.Pod) string {
				return p.GetName()
			})...)

			// Then check each WE for existence of corresponding pod
			for _, weInfo := range wes {
				totalChecked++
				if _, exists := existingPods[weInfo.podName]; !exists {
					staleWorkloads[weInfo.peeredCluster] = append(
						staleWorkloads[weInfo.peeredCluster],
						weInfo,
					)
				}
			}
		}
	}

	return &staleWorkloadsResult{
		staleWorkloads:  staleWorkloads,
		missingClusters: missingClusters,
		totalChecked:    totalChecked,
	}, nil
}

func checkStaleWorkloads(ctx cli.Context, cliClients []kube.CLIClient) (success bool) {
	verbosePrintf("\n\n--- Stale Workloads Check ---\n\n")

	// Skip if only one context
	if len(cliClients) <= 1 {
		color.Yellow("⚠️  Stale Workloads Check: skipped (requires contexts for multiple clusters)")
		return true
	}

	// Work in terms of cluster IDs, not context names
	var clusterIDs []string
	clusterMap := make(map[string]kube.CLIClient, len(cliClients))
	for _, client := range cliClients {
		// Get cluster ID from client or from istiod deployment
		clusterID, err := fetchCluster(ctx.IstioNamespace(), client, "")
		if err != nil {
			verbosePrintf("Warning: could not determine cluster ID: %v\n", err)
			continue
		}
		clusterMap[string(clusterID)] = client
		clusterIDs = append(clusterIDs, string(clusterID))
	}

	// Find stale workloads
	result, err := findStaleWorkloadsInner(clusterMap, clusterIDs, ctx.IstioNamespace())
	if err != nil {
		color.Red("❌ Stale Workloads Check: %v", err)
		return false
	}

	staleWorkloads := result.staleWorkloads
	missingClusters := result.missingClusters
	totalChecked := result.totalChecked

	// Count total stale workloads
	staleCount := 0
	for _, staleList := range staleWorkloads {
		staleCount += len(staleList)
	}

	if totalChecked == 0 {
		color.Yellow("⚠️  Stale Workloads Check: no autogenflat workload entries found")
		return true
	}

	allValid := staleCount == 0

	if allValid {
		color.Green("✅ Stale Workloads Check: no stale workloads found (%d checked)", totalChecked)
	} else {
		color.Yellow("⚠️  Stale Workloads Check: found %d stale workload(s) out of %d checked", staleCount, totalChecked)

		// Output stale workloads in deterministic order
		verbosePrintf("\nStale workloads by cluster:\n")
		for _, clusterID := range clusterIDs {
			staleList := staleWorkloads[clusterID]
			if len(staleList) == 0 {
				continue
			}

			// Sort by namespace, then name
			sort.Slice(staleList, func(i, j int) bool {
				if staleList[i].we.Namespace != staleList[j].we.Namespace {
					return staleList[i].we.Namespace < staleList[j].we.Namespace
				}
				return staleList[i].we.Name < staleList[j].we.Name
			})

			verbosePrintf("\nCluster %s:\n", clusterID)
			for _, stale := range staleList {
				verbosePrintf("  - %s/%s (Pod %s/%s missing from %s)\n",
					stale.we.Namespace, stale.we.Name,
					stale.podNamespace, stale.podName,
					stale.sourceCluster)
			}
		}
	}

	if len(missingClusters) > 0 {
		// Print relationships in deterministic order
		for _, targetCluster := range clusterIDs {
			missingSources := missingClusters[targetCluster]
			if len(missingSources) == 0 {
				continue
			}

			// Sort missing source clusters alphabetically
			sourceList := make([]string, 0, len(missingSources))
			for source := range missingSources {
				sourceList = append(sourceList, source)
			}
			sort.Strings(sourceList)

			for _, sourceCluster := range sourceList {
				color.Yellow("⚠️  Cluster %s has resources from cluster %s, but cluster %s is not in the list of --contexts",
					targetCluster, sourceCluster, sourceCluster)
			}
		}
	}

	return allValid
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
