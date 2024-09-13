// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/log"
)

func NewPrinter(discard io.Writer) Printer {
	return Printer{w: discard}
}

type Printer struct {
	w io.Writer
}

func (p Printer) Writef(format string, args ...any) {
	_, _ = fmt.Fprintln(p.w, "• "+color.BlueString(format, args...))
}

func (p Printer) Warnf(format string, args ...any) {
	_, _ = fmt.Fprintln(p.w, "• "+color.YellowString(format, args...))
}

func (p Printer) Detailsf(format string, args ...any) {
	_, _ = fmt.Fprintln(p.w, "  • "+color.HiBlackString(format, args...))
}

const (
	// PlatformECS gives generic access to ECS usage as a sidecar, typically on Fargate
	PlatformECS = "ecs"
	// PlatformECSEC2 is a specialized EC2-only mode for ECS, where we run as a Daemon.
	PlatformECSEC2 = "ecs-ec2"
)

func Cmd(ctx cli.Context) *cobra.Command {
	var external bool
	var platform string
	var serviceAccount string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Generate a bootstrap token to run ambient mode anywhere.",
		// nolint: lll
		Long: `
bootstrap generates a single 'token' that can be used to connect Ztunnel to the mesh on any platform.

The command takes a single argument -- the name of the service account we wish to use -- and outputs a single token.
This can then be transferred to the machine we wish to run on, or used directly on the same machine.

The command aims to automatically infer as much information as possible, and makes the following assumptions:
* Istiod's CA certificate is stored in the 'istio-ca-root-cert' ConfigMap. This is the case by default, but advanced use cases
  may use a load balancer terminating TLS; these cases are not yet supported.
* Istiod is exposed externally either via the 'istiod' Service being of type LoadBalancer, or a via LoadBalancer Service with the label 'istio.io/expose-istiod: <port number>'.
`,
		Example: `  # Generate a bootstrap token for the 'productpage' service account and use it to launch a ztunnel instance.
  BOOTSTRAP_TOKEN=$(istioctl bootstrap productpage) ztunnel

  # Generate a bootstrap token for the 'productpage' service account in the namespace 'bookinfo'.
  istioctl bootstrap productpage --namespace bookinfo

  # Generate a bootstrap token for the 'productpage' service account.
  # Our workload will run somewhere without direct network connectivity. Note: this requires an east-west gateway setup.
  istioctl bootstrap productpage --external
`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("zero arguments expected")
			}
			switch platform {
			case PlatformECS:
				if serviceAccount == "" {
					return fmt.Errorf("--service-account must be specified for --platform=ecs")
				}
			case PlatformECSEC2:
				if serviceAccount == "" {
					return fmt.Errorf("--service-account must be specified for --platform=ecs-ec2")
				}
			case "":
			default:
				return fmt.Errorf("unknown platform: %v", platform)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, a []string) error {
			kc, err := ctx.CLIClient()
			if err != nil {
				return err
			}

			args := BootstrapArgs{
				Printer:        Printer{w: cmd.OutOrStderr()},
				External:       external, // Assume external if they don't tell us otherwise
				Platform:       platform,
				ServiceAccount: model.GetOrDefault(serviceAccount, "default"),
				Namespace:      ctx.NamespaceOrDefault(ctx.Namespace()),
			}
			bs, err := GenerateToken(kc, args)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), bs.Encode())
			return nil
		},
	}
	cmd.PersistentFlags().StringVarP(&serviceAccount, "service-account", "s", serviceAccount, "The service account the workload will run as.")
	cmd.PersistentFlags().BoolVar(&external, "external", external, "The workload is external to the network")
	cmd.PersistentFlags().StringVar(&platform, "platform", platform, "The platform this workload will run on. Supported values: [ecs]")
	return cmd
}

type BootstrapArgs struct {
	Printer        Printer
	External       bool
	Platform       string
	ServiceAccount string
	Namespace      string
}

type BootstrapToken struct {
	// URL to reach Istiod at. Required.
	URL string `json:"url,omitempty"`
	// CaCert to use for TLS verification of Istiod. Required (for now, we should drop this for trusted cert usage).
	CaCert string `json:"caCert,omitempty"`

	// Kubernetes Namespace to associate this workload to. Required.
	Namespace string `json:"namespace,omitempty"`
	// Kubernetes ServiceAccount to associate this workload to. Required.
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// Platform. Support: generic (default), ecs.
	Platform string `json:"platform,omitempty"`

	// Token based authentication
	Token string `json:"token,omitempty"`

	Network string `json:"network,omitempty"`
	Remote  bool   `json:"remote,omitempty"`
}

func (t BootstrapToken) Encode() string {
	by, err := json.Marshal(t)
	if err != nil {
		log.Fatal("json should not panic")
	}
	r1 := base64.StdEncoding.EncodeToString(by)
	return base64.StdEncoding.EncodeToString([]byte(r1))
}

func GenerateToken(kc kube.CLIClient, args BootstrapArgs) (BootstrapToken, error) {
	serviceAccount := args.ServiceAccount
	namespace := args.Namespace
	p := args.Printer
	p.Writef("Generating a bootstrap token for %s/%s...", namespace, serviceAccount)
	res := BootstrapToken{
		Namespace:      namespace,
		ServiceAccount: serviceAccount,
		Platform:       args.Platform,
	}
	var err error
	res.CaCert, err = fetchIstiodRootCert(kc, namespace)
	if err != nil {
		return res, fmt.Errorf("root cert: %v", err)
	}
	p.Writef("Fetched Istiod Root Cert")

	res.Network, err = fetchNetwork(kc)
	if err != nil {
		return res, fmt.Errorf("network: %v", err)
	}
	if res.Network != "" {
		p.Writef("Fetched Istio network (%s)", res.Network)
	}

	p.Writef("Fetching Istiod URL...")
	res.URL, err = fetchIstiodURL(kc, p, args.External)
	if err != nil {
		return res, fmt.Errorf("url: %v", err)
	}
	p.Writef("Fetching Istiod URL (%s)", res.URL)

	switch args.Platform {
	case PlatformECS, PlatformECSEC2:
		if err := validateECS(kc, p, res); err != nil {
			return res, err
		}
	default:
		res.Token, err = fetchAuthenticationToken(kc, serviceAccount, namespace)
		if err != nil {
			return res, fmt.Errorf("token: %v", err)
		}
		p.Writef("Generated authentication token for %s/%s", namespace, serviceAccount)
		p.Warnf("Bootstrap token contains private authentication information; ensure it is kept private")
	}

	if args.External {
		p.Writef("Marking this workload as external to the network (pass --internal to override)")
		res.Remote = true
	}

	return res, nil
}

func fetchNetwork(kc kube.CLIClient) (string, error) {
	systemNS, err := kc.Kube().CoreV1().Namespaces().Get(context.Background(), "istio-system", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return systemNS.Labels["topology.istio.io/network"], nil
}

func fetchIstiodRootCert(kc kube.CLIClient, namespace string) (string, error) {
	rootCert, err := kc.Kube().CoreV1().ConfigMaps(namespace).Get(context.Background(), controller.CACertNamespaceConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return rootCert.Data[constants.CACertNamespaceConfigMapDataName], nil
}
