//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package shared

import (
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/cluster"
	"istio.io/istio/platform/discovery/peering"
)

func CreateSegmentOrFail(t framework.TestContext, name, domain string, clusters ...cluster.Cluster) {
	targetClusters := clusters
	if len(targetClusters) == 0 {
		targetClusters = t.Clusters()
	}

	for _, c := range targetClusters {
		t.ConfigKube(c).Eval("istio-system", map[string]string{
			"name":   name,
			"domain": domain,
		}, `
apiVersion: admin.solo.io/v1alpha1
kind: Segment
metadata:
  name: {{.name}}
  namespace: istio-system
spec:
  domain: {{.domain}}
`).ApplyOrFail(t)
	}
}

func UseSegmentForTest(t framework.TestContext, segmentName string, clusters ...cluster.Cluster) {
	SetLabelForTest(t, "istio-system", peering.SegmentLabel, segmentName, clusters...)
}
