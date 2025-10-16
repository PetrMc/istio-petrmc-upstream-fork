//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambientmulticluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	"sigs.k8s.io/yaml"

	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/cluster"
	"istio.io/istio/pkg/test/framework/components/crd"
	"istio.io/istio/pkg/test/framework/components/environment/kube"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/istioctl"
	"istio.io/istio/pkg/test/framework/components/namespace"
	testlabel "istio.io/istio/pkg/test/framework/label"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/tests/integration/pilot/ambientmulticluster/shared"
	"istio.io/istio/tests/integration/security/util/cert"
)

var (
	i istio.Instance

	// Below are various preconfigured echo deployments. Whenever possible, tests should utilize these
	// to avoid excessive creation/tear down of deployments. In general, a test should only deploy echo if
	// its doing something unique to that specific test.
	apps = &shared.EchoDeployments{}
)

// The ambientmulticluster is a test to exercise Solo's ambient multicluster flow.
// This is not under ambient/ since that runs with one cluster only, so would never be executed.
func TestMain(m *testing.M) {
	// force every cluster to act as a primary cluster (no primary-remote support)
	kube.ClusterFilter = func(c cluster.Config) (cluster.Config, bool) {
		c.ConfigClusterName = ""
		c.PrimaryClusterName = ""
		return c, true
	}
	// nolint: staticcheck
	framework.
		NewSuite(m).
		Label(testlabel.CustomSetup).
		RequireMinClusters(3).
		SkipIf("only support multi-primary", func(ctx resource.Context) bool {
			haveClusters := sets.New(slices.Map(ctx.Clusters(), func(c cluster.Cluster) string {
				return c.Name()
			})...)
			// TODO make the cluster names for tests dynamic
			wantClusters := sets.New(
				shared.LocalCluster,
				shared.RemoteFlatCluster,
				shared.RemoteNetworkCluster,
			)
			return len(haveClusters.Difference(wantClusters)) > 0
		}).
		Setup(func(t resource.Context) error {
			t.Settings().Ambient = true
			return nil
		}).
		Setup(istio.Setup(&i, func(ctx resource.Context, cfg *istio.Config) {
			ctx.Settings().SkipVMs()                  // can't deploy VMs in this mode
			cfg.DeployEastWestGW = false              // this is the sidecar/SNI eastwest
			cfg.SkipDeployCrossClusterSecrets = false // remote secrets existing must not break our multi-cluster
			cfg.SetUniqueTrustDomain = true
			cfg.ControlPlaneValues = `
profile: ambient
values:
  meshConfig:
    accessLogFile: "/dev/stdout"
    defaultHttpRetryPolicy:
      attempts: 0 # Do not hide failures with retries
  pilot:
    env:
      PILOT_ENABLE_IP_AUTOALLOCATE: "true"
      PILOT_SKIP_VALIDATE_TRUST_DOMAIN: "true"
      PEERING_ENABLE_FLAT_NETWORKS: "true"
      DISABLE_LEGACY_MULTICLUSTER: "true"
  cni:
    ambient:
      dnsCapture: true
  platforms:
    peering:
      enabled: true
  ztunnel:
    env:
      SKIP_VALIDATE_TRUST_DOMAIN: "true"
    platforms:
      peering:
        enabled: true
`
		}, cert.CreateCASecret)).
		Setup(func(ctx resource.Context) error {
			if err := crd.DeployGatewayAPI(ctx); err != nil {
				return err
			}

			kubeconfigs := []string{}
			contexts := []string{}
			for _, cl := range ctx.Clusters() {
				kubeconfigs = append(kubeconfigs, cl.MetadataValue("kubeconfig"))
				// This is not super generic, but should work enough
				contexts = append(contexts, "kind-"+cl.Name())
				ik, err := istioctl.New(ctx, istioctl.Config{Cluster: cl})
				if err != nil {
					return err
				}

				gwns, err := namespace.Claim(ctx, namespace.Config{Prefix: "istio-gateway"})
				if err != nil {
					return err
				}
				stdout, stderr, err := ik.Invoke([]string{"multicluster", "expose", "--wait", "-n", gwns.Name()})
				if err != nil {
					return err
				}
				log.Infof("istioctl expose: %s, %s", stdout, stderr)
			}

			// We need 1 kubeconfig with all contexts... join them.
			// Note: users can do	KUBECONFIG=a:b:C, but we don't want to do that in tests.
			cc := clientcmd.NewDefaultClientConfigLoadingRules()
			cc.Precedence = kubeconfigs
			ccx, err := cc.Load()
			if err != nil {
				return err
			}
			lcfg, err := latest.Scheme.ConvertToVersion(ccx, latest.ExternalVersion)
			if err != nil {
				return err
			}
			oy, err := yaml.Marshal(lcfg)
			if err != nil {
				return err
			}
			// Do not use ctx.CreateTmpDirectory else credentials will end up in test logs. Not an issue when using kind, but good to avoid
			dir, err := os.MkdirTemp("", "test-kubeconfig")
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), oy, 0o644); err != nil {
				return err
			}

			ictl := istioctl.NewKubeManualConfig(filepath.Join(dir, "kubeconfig"))

			stdout, stderr, err := ictl.Invoke([]string{"multicluster", "link", "--contexts", strings.Join(contexts, ","), "-n", "istio-gateway"})
			log.Infof("istioctl link: %s, %s", stdout, stderr)
			if err != nil {
				return err
			}

			// Note: this will only work for 1.27+
			return retry.UntilSuccess(func() error {
				stdout, stderr, err := ictl.Invoke([]string{"multicluster", "check"})
				log.Infof("istioctl mc check: %s, %s", stdout, stderr)
				if err != nil {
					return err
				}
				return nil
			}, retry.Timeout(1*time.Minute))
		}).
		Setup(func(t resource.Context) error {
			return shared.SetupApps(t, apps, false)
		}).
		Run()
}
