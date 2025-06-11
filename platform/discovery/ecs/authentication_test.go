// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"testing"

	"istio.io/istio/pkg/test/util/assert"
)

func TestParseRoleARN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		out  string
	}{
		{
			name: "assumed role",
			in:   "arn:aws:sts::111111111111:assumed-role/my-role/session",
			out:  "arn:aws:iam::111111111111:role/my-role",
		},
		{
			name: "assumed role complex",
			in:   "arn:aws:sts::111111111111:assumed-role/my-role/with/slashes/session",
			out:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRoleARN(tt.in)
			if err != nil {
				t.Logf("got err %v", err)
			}
			assert.Equal(t, tt.out, got)
		})
	}
}

func TestNormalizeRoleARN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		out  string
	}{
		{
			name: "direct role",
			in:   "arn:aws:iam::111111111111:role/my-role",
			out:  "arn:aws:iam::111111111111:role/my-role",
		},
		{
			name: "direct role complex",
			in:   "arn:aws:iam::111111111111:role/my-role/with/slashes",
			out:  "arn:aws:iam::111111111111:role/slashes",
		},
		{
			name: "bad type",
			in:   "arn:aws:iam::111111111111:not-role/blah",
			out:  "arn:aws:iam::111111111111:not-role/blah",
		},
		{
			name: "bogus",
			in:   "arn:aws:iam::111111111111:x",
			out:  "arn:aws:iam::111111111111:x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeRoleARN(tt.in)
			assert.Equal(t, Role(tt.out), got)
		})
	}
}
