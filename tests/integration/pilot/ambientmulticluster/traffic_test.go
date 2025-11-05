//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambientmulticluster

import (
	"testing"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/tests/integration/pilot/ambientmulticluster/shared"
)

func TestTraffic(t *testing.T) {
	framework.
		NewTest(t).
		Run(func(t framework.TestContext) {
			shared.RunAllTrafficTests(t, apps)
		})
}
