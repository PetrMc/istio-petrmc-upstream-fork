// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peer

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"
	"istio.io/istio/pkg/kube"
)

type testWE struct {
	name          string
	sourceCluster string
	podNs         string
	podName       string
}

func TestFindStaleWorkloadsInner(t *testing.T) {
	tests := []struct {
		name                string
		c1Pods              []string // "namespace/name"
		c1WEs               []testWE
		c2Pods              []string // "namespace/name"
		c2WEs               []testWE
		wantStale           map[string][]string // clusterID -> []"namespace/name" of testWE
		wantMissingClusters map[string][]string // targetCluster -> []missingSourceClusters
	}{
		{
			name:   "no stale workloads - all pods exist",
			c1Pods: []string{"default/test-pod"},
			c2WEs: []testWE{
				{name: "autogenflat-test-pod", sourceCluster: "cluster-1", podNs: "default", podName: "test-pod"},
			},
			wantStale:           map[string][]string{},
			wantMissingClusters: map[string][]string{},
		},
		{
			name:   "stale workload - pod does not exist",
			c1Pods: []string{}, // No pods
			c2WEs: []testWE{
				{name: "autogenflat-missing-pod", sourceCluster: "cluster-1", podNs: "default", podName: "missing-pod"},
			},
			wantStale: map[string][]string{
				"cluster-2": {"istio-system/autogenflat-missing-pod"},
			},
			wantMissingClusters: map[string][]string{},
		},
		{
			name: "mixed - some valid, some stale",
			c1Pods: []string{
				"default/valid-pod",
				"app-ns/app-pod",
			},
			c2WEs: []testWE{
				{name: "autogenflat-valid-pod", sourceCluster: "cluster-1", podNs: "default", podName: "valid-pod"},
				{name: "autogenflat-stale-pod", sourceCluster: "cluster-1", podNs: "default", podName: "stale-pod"},
				{name: "autogenflat-app-pod", sourceCluster: "cluster-1", podNs: "app-ns", podName: "app-pod"},
			},
			wantStale: map[string][]string{
				"cluster-2": {"istio-system/autogenflat-stale-pod"},
			},
			wantMissingClusters: map[string][]string{},
		},
		{
			name:   "skip non-autogenflat workload entries",
			c1Pods: []string{"default/test-pod"},
			c2WEs: []testWE{
				{name: "autogenflat-test-pod", sourceCluster: "cluster-1", podNs: "default", podName: "test-pod"},
			},
			wantStale:           map[string][]string{},
			wantMissingClusters: map[string][]string{},
		},
		{
			name:   "missing source cluster",
			c1Pods: []string{},
			c2WEs: []testWE{
				{name: "autogenflat-from-cluster-3", sourceCluster: "cluster-3", podNs: "default", podName: "some-pod"},
			},
			wantStale: map[string][]string{},
			wantMissingClusters: map[string][]string{
				"cluster-2": {"cluster-3"},
			},
		},
		{
			name: "multiple namespaces",
			c1Pods: []string{
				"default/default-pod",
				"kube-system/kube-pod",
			},
			c2WEs: []testWE{
				{name: "autogenflat-default-pod", sourceCluster: "cluster-1", podNs: "default", podName: "default-pod"},
				{name: "autogenflat-kube-pod", sourceCluster: "cluster-1", podNs: "kube-system", podName: "kube-pod"},
				{name: "autogenflat-stale-in-default", sourceCluster: "cluster-1", podNs: "default", podName: "stale-pod"},
			},
			wantStale: map[string][]string{
				"cluster-2": {"istio-system/autogenflat-stale-in-default"},
			},
			wantMissingClusters: map[string][]string{},
		},
		{
			name: "bidirectional - both clusters have pods and WEs",
			c1Pods: []string{
				"default/pod-a",
			},
			c1WEs: []testWE{
				{name: "autogenflat-pod-b", sourceCluster: "cluster-2", podNs: "default", podName: "pod-b"},
			},
			c2Pods: []string{
				"default/pod-b",
			},
			c2WEs: []testWE{
				{name: "autogenflat-pod-a", sourceCluster: "cluster-1", podNs: "default", podName: "pod-a"},
			},
			wantStale:           map[string][]string{},
			wantMissingClusters: map[string][]string{},
		},
		{
			name: "bidirectional - one cluster has stale",
			c1Pods: []string{
				"default/pod-a",
			},
			c1WEs: []testWE{
				{name: "autogenflat-pod-b", sourceCluster: "cluster-2", podNs: "default", podName: "pod-b"},
				{name: "autogenflat-pod-c", sourceCluster: "cluster-2", podNs: "default", podName: "pod-c"},
			},
			c2Pods: []string{
				"default/pod-b",
			},
			c2WEs: []testWE{
				{name: "autogenflat-pod-a", sourceCluster: "cluster-1", podNs: "default", podName: "pod-a"},
			},
			wantStale: map[string][]string{
				"cluster-1": {"istio-system/autogenflat-pod-c"},
			},
			wantMissingClusters: map[string][]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build cluster-1 objects
			cluster1Objects := make([]runtime.Object, 0)
			for _, podName := range tt.c1Pods {
				cluster1Objects = append(cluster1Objects, makePod(podName))
			}
			for _, we := range tt.c1WEs {
				cluster1Objects = append(cluster1Objects, makeWorkloadEntry(we))
			}

			// Build cluster-2 objects
			cluster2Objects := make([]runtime.Object, 0)
			for _, podName := range tt.c2Pods {
				cluster2Objects = append(cluster2Objects, makePod(podName))
			}
			for _, we := range tt.c2WEs {
				cluster2Objects = append(cluster2Objects, makeWorkloadEntry(we))
			}

			// Create fake clients
			cluster1Client := kube.NewFakeClient(cluster1Objects...)
			cluster2Client := kube.NewFakeClient(cluster2Objects...)

			// Build cluster map
			clusterMap := map[string]kube.CLIClient{
				"cluster-1": cluster1Client,
				"cluster-2": cluster2Client,
			}
			clusterIDs := []string{"cluster-1", "cluster-2"}

			// Call the inner function
			result, err := findStaleWorkloadsInner(clusterMap, clusterIDs, "istio-system")
			if err != nil {
				t.Fatalf("findStaleWorkloadsInner() error = %v", err)
			}

			// Convert result to map[clusterID][]"namespace/name"
			gotStale := make(map[string][]string)
			for clusterID, staleList := range result.staleWorkloads {
				var names []string
				for _, stale := range staleList {
					names = append(names, fmt.Sprintf("%s/%s", stale.we.Namespace, stale.we.Name))
				}
				sort.Strings(names)
				gotStale[clusterID] = names
			}

			// Compare stale workloads
			if !equalStringSliceMap(gotStale, tt.wantStale) {
				t.Errorf("stale workloads mismatch:\ngot:  %v\nwant: %v", gotStale, tt.wantStale)
			}

			// Check missing clusters
			for targetCluster, wantMissingSources := range tt.wantMissingClusters {
				gotMissingSources := result.missingClusters[targetCluster]
				if len(gotMissingSources) != len(wantMissingSources) {
					t.Errorf("cluster %s missing sources count = %v, want %v", targetCluster, len(gotMissingSources), len(wantMissingSources))
					continue
				}
				for _, wantSource := range wantMissingSources {
					if _, ok := gotMissingSources[wantSource]; !ok {
						t.Errorf("cluster %s missing source cluster %s not found", targetCluster, wantSource)
					}
				}
			}
		})
	}
}

// makePod creates a Pod from "namespace/name" format
func makePod(nsName string) *corev1.Pod {
	parts := strings.Split(nsName, "/")
	if len(parts) != 2 {
		panic(fmt.Sprintf("invalid pod format: %s, expected namespace/name", nsName))
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      parts[1],
			Namespace: parts[0],
		},
	}
}

// makeWorkloadEntry creates a WorkloadEntry from testWE
func makeWorkloadEntry(we testWE) *networkingv1.WorkloadEntry {
	return &networkingv1.WorkloadEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      we.name,
			Namespace: "istio-system",
			Labels: map[string]string{
				"solo.io/source-workload": fmt.Sprintf("autogenflat.%s.%s.%s", we.sourceCluster, we.podNs, we.podName),
				"solo.io/source-cluster":  we.sourceCluster,
			},
			Annotations: map[string]string{
				"solo.io/peered-workload-uid": fmt.Sprintf("%s//Pod/%s/%s", we.sourceCluster, we.podNs, we.podName),
			},
		},
	}
}

// equalStringSliceMap compares two maps of string slices
func equalStringSliceMap(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if len(va) != len(vb) {
			return false
		}
		for i := range va {
			if va[i] != vb[i] {
				return false
			}
		}
	}
	return true
}
