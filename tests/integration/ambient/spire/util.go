//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package spire

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/test/framework/components/cluster"
	kubecluster "istio.io/istio/pkg/test/framework/components/cluster/kube"
	"istio.io/istio/pkg/test/framework/resource"
	kubetest "istio.io/istio/pkg/test/kube"
	"istio.io/istio/pkg/test/scopes"
	"istio.io/istio/pkg/test/shell"
	"istio.io/istio/pkg/test/util/retry"
)

const (
	SpireNamespace = "spire-server" // by convention
	Timeout        = 2 * time.Minute
)

const (
	IstioNamespace         = "istio-system"
	ReleasePrefix          = "istio-"
	BaseChart              = "base"
	CRDsFolder             = "crds"
	DiscoveryChartsDir     = "istio-discovery"
	BaseReleaseName        = ReleasePrefix + BaseChart
	RepoBaseChartPath      = BaseChart
	RepoDiscoveryChartPath = "istiod"
	RepoGatewayChartPath   = "gateway"
	IstiodReleaseName      = "istiod"
	IngressReleaseName     = "istio-ingress"
	ControlChartsDir       = "istio-control"
	GatewayChartsDir       = "gateway"
	CniChartsDir           = "istio-cni"
	ZtunnelChartsDir       = "ztunnel"
	RepoCniChartPath       = "cni"
	CniReleaseName         = ReleasePrefix + "cni"
	RepoZtunnelChartPath   = "ztunnel"
	ZtunnelReleaseName     = "ztunnel"

	RetryDelay   = 2 * time.Second
	RetryTimeOut = 5 * time.Minute
)

var DefaultNamespaceConfig = NewNamespaceConfig()

func NewNamespaceConfig(config ...types.NamespacedName) NamespaceConfig {
	result := make(nsConfig, len(config))
	for _, c := range config {
		result[c.Name] = c.Namespace
	}
	return result
}

type nsConfig map[string]string

func (n nsConfig) Get(name string) string {
	if ns, ok := n[name]; ok {
		return ns
	}
	return IstioNamespace
}

func (n nsConfig) Set(name, ns string) {
	n[name] = ns
}

func (n nsConfig) AllNamespaces() []string {
	return maps.Values(n)
}

type NamespaceConfig interface {
	Get(name string) string
	Set(name, ns string)
	AllNamespaces() []string
}

// Helm allows clients to interact with helm commands in their cluster
type Helm struct {
	kubeConfig string
}

// New returns a new instance of a helm object.
func NewHelm(kubeConfig string) *Helm {
	return &Helm{
		kubeConfig: kubeConfig,
	}
}

const SpireHelmRepo = "https://spiffe.github.io/helm-charts-hardened/"

// InstallSpire installs the spire CRDs/agent/server/CSI driver/registration controller,
// taken straight from https://artifacthub.io/packages/helm/spiffe/spire#install-instructions
func (h *Helm) InstallSpire(namespace, overridesFile string, timeout time.Duration) error {
	// CRDs first
	crdCommand := fmt.Sprintf("helm upgrade --install --namespace %s spire-crds spire-crds --repo %s -f %s --kubeconfig %s --create-namespace --timeout %s",
		namespace, SpireHelmRepo, overridesFile, h.kubeConfig, timeout)
	if err := execCommand(crdCommand); err != nil {
		return err
	}

	// Then the rest
	spireCommand := fmt.Sprintf("helm upgrade --install --namespace %s spire spire --repo %s -f %s --kubeconfig %s --timeout %s",
		namespace, SpireHelmRepo, overridesFile, h.kubeConfig, timeout)
	if err := execCommand(spireCommand); err != nil {
		return err
	}

	return nil
}

func (h *Helm) CreateClusterSPIFFEIDResources(t resource.Context, timeout time.Duration) error {
	ztRegStr := `
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: istio-ztunnel-reg
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      app: "ztunnel"
`

	wpRegStr := `
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: istio-waypoint-reg
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      istio.io/gateway-name: waypoint
`

	wlRegStr := `
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: istio-ambient-reg
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      istio.io/dataplane-mode: ambient
`
	// TODO apply these together
	err := t.Clusters().Default().ApplyYAMLContents(SpireNamespace, ztRegStr)
	if err != nil {
		return err
	}

	err = t.Clusters().Default().ApplyYAMLContents(SpireNamespace, wpRegStr)
	if err != nil {
		return err
	}

	err = t.Clusters().Default().ApplyYAMLContents(SpireNamespace, wlRegStr)
	if err != nil {
		return err
	}

	return nil
}

// DeleteSpire removes the spire CRDs/agent/server/CSI driver/registration controller,
// taken straight from https://artifacthub.io/packages/helm/spiffe/spire#install-instructions
func (h *Helm) DeleteSpire(namespace string, timeout time.Duration) error {
	// First the main bits
	spireCommand := fmt.Sprintf("helm delete spire --namespace %s --kubeconfig %s --timeout %s",
		namespace, h.kubeConfig, timeout)
	if err := execCommand(spireCommand); err != nil {
		return err
	}

	// CRDs finally
	crdCommand := fmt.Sprintf("helm delete spire-crds --namespace %s --kubeconfig %s --timeout %s",
		namespace, h.kubeConfig, timeout)
	if err := execCommand(crdCommand); err != nil {
		return err
	}
	return nil
}

func execCommand(cmd string) error {
	scopes.Framework.Infof("Applying helm command: %s", cmd)

	_, err := shell.Execute(true, cmd)
	if err != nil {
		scopes.Framework.Infof("(FAILED) Executing helm: %s (err: %v)", cmd, err)
		return fmt.Errorf("%v", err)
	}

	return nil
}

func DeploySpireWithOverrides(t resource.Context, overrideValuesStr string) error {
	workDir, err := t.CreateTmpDirectory("spire-install-test")
	if err != nil {
		return fmt.Errorf("failed to create test directory")
	}
	cs := t.Clusters().Default().(*kubecluster.Cluster)
	h := NewHelm(cs.Filename())

	overrideValuesFile := filepath.Join(workDir, "spire-values.yaml")
	if err := os.WriteFile(overrideValuesFile, []byte(overrideValuesStr), os.ModePerm); err != nil {
		return fmt.Errorf("failed to write spire override values file: %v", err)
	}

	if err = h.InstallSpire(SpireNamespace, overrideValuesFile, Timeout); err != nil {
		return err
	}

	if err = VerifySpireInstallation(cs); err != nil {
		return err
	}

	if err = h.CreateClusterSPIFFEIDResources(t, Timeout); err != nil {
		return err
	}

	return nil
}

func TeardownSpire(t resource.Context) error {
	cs := t.Clusters().Default().(*kubecluster.Cluster)
	h := NewHelm(cs.Filename())
	if err := h.DeleteSpire(SpireNamespace, Timeout); err != nil {
		return err
	}

	return nil
}

// VerifyPodsReady verify that the Helm installation is successful
func VerifyPodsReady(cs cluster.Cluster, ns, label string) error {
	return retry.UntilSuccess(func() error {
		if _, err := kubetest.CheckPodsAreReady(kubetest.NewPodFetch(cs, ns, label)); err != nil {
			return fmt.Errorf("%s pod is not ready: %v", label, err)
		}
		return nil
	}, retry.Timeout(RetryTimeOut), retry.Delay(RetryDelay))
}

// VerifyInstallation verify that the Helm installation is successful
func VerifySpireInstallation(cs cluster.Cluster) error {
	scopes.Framework.Infof("=== verifying spire installation === ")
	servErr := VerifyPodsReady(cs, SpireNamespace, "app.kubernetes.io/name=server")
	agentErr := VerifyPodsReady(cs, SpireNamespace, "app.kubernetes.io/name=agent")
	csiErr := VerifyPodsReady(cs, SpireNamespace, "app.kubernetes.io/name=spiffe-csi-driver")
	err := errors.Join(servErr, agentErr, csiErr)
	if err == nil {
		scopes.Framework.Infof("=== succeeded ===")
	} else {
		scopes.Framework.Infof("=== failed ===")
	}

	return err
}
