// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant
package peering

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"istio.io/istio/pkg/workloadapi"
)

func TestMergedServicesForWorkload(t *testing.T) {
	svc1 := makeSvc("default", "a")
	svc2 := makeSvc("default", "b")
	svc3 := makeSvc("default", "c")

	tests := []struct {
		name string
		in   []servicesForWorkload
		want []servicesForWorkload
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "single selector",
			in: []servicesForWorkload{{
				selector: map[string]string{"app": "web"},
				services: []serviceForWorkload{svc1},
			}},
			want: []servicesForWorkload{{
				selector: map[string]string{"app": "web"},
				services: []serviceForWorkload{svc1},
			}},
		},
		{
			name: "subset merged",
			in: []servicesForWorkload{
				{selector: map[string]string{"app": "web"}, services: []serviceForWorkload{svc1}},
				{selector: map[string]string{"app": "web", "tier": "frontend"}, services: []serviceForWorkload{svc2}},
			},
			want: []servicesForWorkload{{
				selector: map[string]string{"app": "web", "tier": "frontend"},
				services: []serviceForWorkload{svc1, svc2},
			}},
		},
		{
			name: "conflicts kept, subset removed",
			in: []servicesForWorkload{
				{selector: map[string]string{"app": "web"}, services: []serviceForWorkload{svc1}},
				{selector: map[string]string{"app": "web", "version": "v1", "flavor": "vanilla"}, services: []serviceForWorkload{svc2}},
				{selector: map[string]string{"app": "web", "version": "v1", "flavor": "chocolate"}, services: []serviceForWorkload{svc3}},
			},
			want: []servicesForWorkload{
				{selector: map[string]string{"app": "web", "version": "v1", "flavor": "vanilla"}, services: []serviceForWorkload{svc1, svc2}},
				{selector: map[string]string{"app": "web", "version": "v1", "flavor": "chocolate"}, services: []serviceForWorkload{svc1, svc3}},
			},
		},
		{
			name: "duplicates removed",
			in: []servicesForWorkload{
				{selector: map[string]string{"env": "prod"}, services: []serviceForWorkload{svc1}},
				{selector: map[string]string{"env": "prod"}, services: []serviceForWorkload{svc2}},
			},
			want: []servicesForWorkload{
				{selector: map[string]string{"env": "prod"}, services: []serviceForWorkload{svc1, svc2}},
			},
		},
	}

	for _, tt := range tests {
		got := mergedServicesForWorkload(tt.in)
		sortSvcForWorkload(got)
		sortSvcForWorkload(tt.want)

		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s:\nwant %#v\ngot  %#v", tt.name, tt.want, got)
		}
	}
}

func canonicalSel(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteRune('=')
		b.WriteString(m[k])
		b.WriteByte(';')
	}
	return b.String()
}

func sortSvcForWorkload(arr []servicesForWorkload) {
	sort.Slice(arr, func(i, j int) bool {
		return canonicalSel(arr[i].selector) < canonicalSel(arr[j].selector)
	})
	for k := range arr {
		sort.Slice(arr[k].services, func(i, j int) bool {
			return arr[k].services[i].federatedName() < arr[k].services[j].federatedName()
		})
	}
}

func makeSvc(ns, name string) serviceForWorkload {
	return serviceForWorkload{
		federated: &RemoteFederatedService{
			Service: &workloadapi.FederatedService{
				Namespace: ns,
				Name:      name,
			},
		},
	}
}
