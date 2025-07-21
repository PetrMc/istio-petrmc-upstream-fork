// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package peer

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
		},
	}
}

func TestLocalityFromNodes(t *testing.T) {
	tests := []struct {
		name       string
		nodes      []corev1.Node
		wantRegion string
		wantZone   string
		wantWarn   bool
	}{
		{
			name: "no region labels",
			nodes: []corev1.Node{
				node(map[string]string{}),
			},
		},
		{
			name: "single region single zone",
			nodes: []corev1.Node{
				node(map[string]string{
					"topology.kubernetes.io/region": "us-east1",
					"topology.kubernetes.io/zone":   "us-east1-b",
				}),
			},
			wantRegion: "us-east1",
			wantZone:   "us-east1-b",
		},
		{
			name: "multiple regions",
			nodes: []corev1.Node{
				node(map[string]string{
					"topology.kubernetes.io/region": "us-east1",
				}),
				node(map[string]string{
					"topology.kubernetes.io/region": "us-west1",
				}),
			},
			wantWarn: true,
		},
		{
			name: "single region multiple zones",
			nodes: []corev1.Node{
				node(map[string]string{
					"topology.kubernetes.io/region": "us-east1",
					"topology.kubernetes.io/zone":   "us-east1-a",
				}),
				node(map[string]string{
					"topology.kubernetes.io/region": "us-east1",
					"topology.kubernetes.io/zone":   "us-east1-b",
				}),
			},
			wantRegion: "us-east1",
			wantWarn:   true,
		},
		{
			name: "region label without zone label",
			nodes: []corev1.Node{
				node(map[string]string{
					"topology.kubernetes.io/region": "us-central1",
				}),
			},
			wantRegion: "us-central1",
			wantZone:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			region, zone, warn, err := localityFromNodes(tt.nodes)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if region != tt.wantRegion {
				t.Fatalf("region = %q, want %q", region, tt.wantRegion)
			}
			if zone != tt.wantZone {
				t.Fatalf("zone = %q, want %q", zone, tt.wantZone)
			}
			if (warn != "") != tt.wantWarn {
				t.Fatalf("warn = %q, wantWarn %v", warn, tt.wantWarn)
			}
		})
	}
}
