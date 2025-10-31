// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/istioctl/pkg/bootstrap"
	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/model"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	ecsplatform "istio.io/istio/platform/discovery/ecs"
)

func Cmd(ctx cli.Context) *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   "ecs",
		Short: "Interactions with AWS ECS",
	}
	cmd.PersistentFlags().StringVar(&cluster, "cluster", "", "ECS cluster name to use")
	cmd.AddCommand(AddServiceCmd(ctx, cluster))
	cmd.AddCommand(ListRolesCmd(ctx))
	return cmd
}

func ListRolesCmd(ctx cli.Context) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list-roles",
		Short: "List the mapping of ECS roles to Service Accounts",
		Example: `  # List the mapping of ECS roles to Service Accounts
  istioctl ecs list-roles
`,
		RunE: func(c *cobra.Command, args []string) error {
			ns := ctx.Namespace()
			kc, err := ctx.CLIClient()
			if err != nil {
				return err
			}
			sas, err := kc.Kube().CoreV1().ServiceAccounts(ns).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return err
			}
			w := new(tabwriter.Writer).Init(c.OutOrStdout(), 0, 8, 5, ' ', 0)
			fmt.Fprintf(w, "NAMESPACE\tSERVICE ACCOUNT\tROLE\n")
			for _, sa := range sas.Items {
				role := getRole(&sa)
				if role == "" && !all {
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%v\n", sa.Namespace, sa.Name, ptr.NonEmptyOrDefault(role, "<none>"))
			}
			return w.Flush()
		},
	}
	cmd.PersistentFlags().BoolVar(&all, "all", all, "Include all service accounts")
	return cmd
}

func AddServiceCmd(ctx cli.Context, cluster string) *cobra.Command {
	var (
		profile        string
		external       bool
		serviceAccount string
		platform       string
		hostname       string
		ports          string
	)
	cmd := &cobra.Command{
		Use:   "add-service",
		Short: "Enroll an ECS Service onto the mesh",
		Example: `  Enroll the 'demo' service in the 'my-ecs-cluster' cluster into the mesh.
  Once enrolled, it can be reached at 'demo.my-ecs-cluster.<domain>' where <domain> is the domain
  associated with the AWS account as registered with istiod.

  istioctl ecs add-service demo --cluster my-ecs-cluster
`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("one arguments expected (the service name)")
			}
			if cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			printer := bootstrap.NewPrinter(c.OutOrStderr())
			service := args[0]
			namespace := ctx.Namespace()
			if namespace == "" {
				namespace = "istio-system"
			}

			kc, err := ctx.CLIClient()
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes client: %v", err)
			}
			ec, awsRegion, err := buildECSClient(c, profile)
			if err != nil {
				return err
			}

			// Our goal here is to update the service with the additional ztunnel container.
			// This requires creating a new task definition revision, and updating the service to use it.
			current, err := fetchService(ec, service, cluster)
			if err != nil {
				return err
			}

			td, err := taskDefinitionForService(ec, current)
			if err != nil {
				return err
			}
			exRole := td.TaskDefinition.TaskRoleArn
			if exRole == nil {
				return fmt.Errorf("task definition %q does not have an 'executionRoleArn' defined, which is required", *current.TaskDefinition)
			}

			var newContainerDefinitions []ecstypes.ContainerDefinition
			switch platform {
			case bootstrap.PlatformECS:
				image, err := fetchZtunnelImage(kc, ctx)
				if err != nil {
					return fmt.Errorf("failed to find ztunnel image: %v", err)
				}

				// Now we need to generate a bootstrap token for them.
				// Since this uses platform=ecs, there is no auth credentials in it, so safe to just put directly into the task definition.
				bootstrapToken, err := fetchBootstrapToken(kc, printer, namespace, serviceAccount, *exRole, external)
				if err != nil {
					return fmt.Errorf("failed to generate bootstrap token: %v", err)
				}

				newContainerDefinitions = updateContainerDefinitions(td, printer, bootstrapToken, image, awsRegion)
			case bootstrap.PlatformECSEC2:
				// Just mark each container as needed 'ambient' dataplane.
				newContainerDefinitions = slices.Map(
					filterZtunnelContainers(td.TaskDefinition.ContainerDefinitions),
					func(definition ecstypes.ContainerDefinition) ecstypes.ContainerDefinition {
						definition.DockerLabels["istio.io/dataplane-mode"] = "ambient"
						return definition
					})
				// TODO: we still need to modify the service account
			default:
				return fmt.Errorf("unknown platform: %v", err)
			}

			o, err := updateTaskDefinition(td, ec, newContainerDefinitions)
			if err != nil {
				return fmt.Errorf("failed to register task definition: %v", err)
			}
			printer.Writef("Created task definition %v", *o.TaskDefinition.TaskDefinitionArn)

			updatedService, err := updateService(ec, service, cluster, serviceAccount, namespace, hostname, ports, o)
			if err != nil {
				return err
			}
			printer.Writef("Successfully enrolled service %q (%v) to the mesh", service, *updatedService.Service.ServiceArn)
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&profile, "profile", profile, "The AWS profile to use if using a shared config.")
	cmd.PersistentFlags().BoolVar(&external, "external", external, "The workload is external to the network")
	cmd.PersistentFlags().StringVarP(&serviceAccount, "service-account", "s", "default", "The service account the workload will run as.")
	cmd.PersistentFlags().StringVarP(&platform, "platform", "p", bootstrap.PlatformECS, "The runtime platform we want to use. Valid options: [ecs, ecs-ec2]")
	cmd.PersistentFlags().StringVar(&hostname, "hostname", "", "The DNS name to expose the service as. Defaults to <service name>.<cluster name>.local")
	cmd.PersistentFlags().StringVar(&ports, "ports", "",
		"Port configuration for the service. Accepts a forward slash-separated list of protocol:port[:targetPort] pairs. Example: 'http:80:8080/tcp:9090'")
	cmd.PersistentFlags().StringVar(&cluster, "cluster", "", "ECS cluster name to use")
	return cmd
}

func updateContainerDefinitions(
	td *ecs.DescribeTaskDefinitionOutput,
	printer bootstrap.Printer,
	bootstrapToken string,
	image string,
	region string,
) []ecstypes.ContainerDefinition {
	// Remove any existing Ztunnel containers
	cd := filterZtunnelContainers(td.TaskDefinition.ContainerDefinitions)
	// Mark everything else as dependent on ztunnel
	logGroup := ""
	cd = slices.Map(cd, func(definition ecstypes.ContainerDefinition) ecstypes.ContainerDefinition {
		definition.DependsOn = append(definition.DependsOn, ecstypes.ContainerDependency{
			Condition:     ecstypes.ContainerConditionHealthy,
			ContainerName: ptr.Of("ztunnel"),
		})
		if logGroup == "" && definition.LogConfiguration != nil {
			logGroup = definition.LogConfiguration.Options["awslogs-group"]
		}
		return definition
	})
	if len(cd) != len(td.TaskDefinition.ContainerDefinitions) {
		printer.Warnf("Task definition already has a 'ztunnel' container; updating it")
	}
	var logs *ecstypes.LogConfiguration
	if logGroup != "" {
		// TODO: do not hardcode?
		logs = &ecstypes.LogConfiguration{
			LogDriver: "awslogs",
			Options: map[string]string{
				"awslogs-create-group":  "true",
				"awslogs-group":         logGroup,
				"awslogs-region":        region,
				"awslogs-stream-prefix": "ztunnel",
			},
		}
	}
	cd = append(cd, ecstypes.ContainerDefinition{
		Environment: []ecstypes.KeyValuePair{{
			Name:  ptr.Of("BOOTSTRAP_TOKEN"),
			Value: ptr.Of(bootstrapToken),
		}},
		HealthCheck: &ecstypes.HealthCheck{
			Command:     []string{"ztunnel", "healthcheck"},
			Interval:    ptr.Of(int32(5)),
			Retries:     ptr.Of(int32(3)),
			StartPeriod: ptr.Of(int32(10)),
			Timeout:     ptr.Of(int32(5)),
		},
		Image:            ptr.Of(image),
		LogConfiguration: logs,
		Name:             ptr.Of("ztunnel"),
	})

	return cd
}

func filterZtunnelContainers(td []ecstypes.ContainerDefinition) []ecstypes.ContainerDefinition {
	td = slices.Filter(td, func(definition ecstypes.ContainerDefinition) bool {
		return *definition.Name != "ztunnel"
	})
	td = slices.Map(td, func(definition ecstypes.ContainerDefinition) ecstypes.ContainerDefinition {
		definition.DependsOn = slices.Filter(definition.DependsOn, func(dependency ecstypes.ContainerDependency) bool {
			return !ptr.Equal(dependency.ContainerName, ptr.Of("ztunnel"))
		})
		return definition
	})
	return td
}

func updateService(
	ec *ecs.Client,
	service string,
	cluster string,
	serviceAccount string,
	namespace string,
	hostname string,
	ports string,
	o *ecs.RegisterTaskDefinitionOutput,
) (*ecs.UpdateServiceOutput, error) {
	updatedService, err := ec.UpdateService(context.Background(), &ecs.UpdateServiceInput{
		Service:        ptr.Of(service),
		Cluster:        ptr.Of(cluster),
		TaskDefinition: o.TaskDefinition.TaskDefinitionArn,
		PropagateTags:  ecstypes.PropagateTagsService,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update service: %v", err)
	}
	tags := []ecstypes.Tag{
		{
			Key:   ptr.Of(ecsplatform.ServiceAccountTag),
			Value: ptr.Of(serviceAccount),
		},
		{
			Key:   ptr.Of(ecsplatform.NamespaceTag),
			Value: ptr.Of(namespace),
		},
	}
	if hostname != "" {
		tags = append(tags, ecstypes.Tag{
			Key:   ptr.Of(ecsplatform.HostnameTag),
			Value: ptr.Of(hostname),
		})
	}
	if ports != "" {
		tags = append(tags, ecstypes.Tag{
			Key:   ptr.Of(ecsplatform.ServicePortsTag),
			Value: ptr.Of(ports),
		})
	}
	tagResponse, err := ec.TagResource(context.Background(), &ecs.TagResourceInput{
		ResourceArn: updatedService.Service.ServiceArn,
		Tags:        tags,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update service tags: %v", err)
	}
	_ = tagResponse
	return updatedService, nil
}

func updateTaskDefinition(
	td *ecs.DescribeTaskDefinitionOutput,
	ec *ecs.Client,
	newContainerDefinitions []ecstypes.ContainerDefinition,
) (*ecs.RegisterTaskDefinitionOutput, error) {
	t := td.TaskDefinition
	tags := td.Tags
	if len(tags) == 0 {
		tags = nil
	}
	o, err := ec.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions:    newContainerDefinitions,
		Family:                  t.Family,
		Cpu:                     t.Cpu,
		EphemeralStorage:        t.EphemeralStorage,
		ExecutionRoleArn:        t.ExecutionRoleArn,
		InferenceAccelerators:   t.InferenceAccelerators,
		IpcMode:                 t.IpcMode,
		Memory:                  t.Memory,
		NetworkMode:             t.NetworkMode,
		PidMode:                 t.PidMode,
		PlacementConstraints:    t.PlacementConstraints,
		ProxyConfiguration:      t.ProxyConfiguration,
		RequiresCompatibilities: t.RequiresCompatibilities,
		RuntimePlatform:         t.RuntimePlatform,
		Tags:                    tags,
		TaskRoleArn:             t.TaskRoleArn,
		Volumes:                 t.Volumes,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to register task definition: %v", err)
	}
	return o, nil
}

func taskDefinitionForService(ec *ecs.Client, current ecstypes.Service) (*ecs.DescribeTaskDefinitionOutput, error) {
	td, err := ec.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: current.TaskDefinition,
		Include:        []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find task definition %q: %v", *current.TaskDefinition, err)
	}
	return td, nil
}

func buildECSClient(c *cobra.Command, profile string) (*ecs.Client, string, error) {
	// Load the Shared AWS Configuration (~/.aws/config)
	cfg, err := config.LoadDefaultConfig(c.Context(), config.WithSharedConfigProfile(profile))
	if err != nil {
		return nil, "", fmt.Errorf("failed to initialize ECS client: %v", err)
	}

	ec := ecs.NewFromConfig(cfg)
	return ec, cfg.Region, nil
}

func fetchService(ec *ecs.Client, service string, cluster string) (ecstypes.Service, error) {
	svc, err := ec.DescribeServices(context.Background(), &ecs.DescribeServicesInput{
		Services: []string{service},
		Cluster:  ptr.Of(cluster),
		Include:  nil,
	})
	if err != nil {
		return ecstypes.Service{}, fmt.Errorf("failed to find service %q: %v", service, err)
	}
	if len(svc.Services) != 1 {
		return ecstypes.Service{}, fmt.Errorf("service %q has %d services, expected %d", service, len(svc.Services), 1)
	}
	current := svc.Services[0]
	return current, nil
}

func fetchBootstrapToken(kc kube.CLIClient, printer bootstrap.Printer, ns string, sa string, role string, external bool) (string, error) {
	serviceAccount := model.GetOrDefault(sa, "default") // TODO: is it okay to default this way or should we force a service account?
	sac := kc.Kube().CoreV1().ServiceAccounts(ns)
	curSa, err := sac.Get(context.Background(), serviceAccount, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get service account %v/%v: %v", ns, serviceAccount, err)
	}
	curRole := getRole(curSa)
	if curRole != "" && curRole != role {
		return "", fmt.Errorf("service account %v/%v already has been configured for role %q, but we are trying to attach it to %q",
			ns, serviceAccount, curRole, role)
	}

	patchOpts := metav1.PatchOptions{}
	patchData := fmt.Sprintf(`{"metadata":{"annotations": {%q: %q}}}`, ecsplatform.SoloRoleArnAnnotation, role)
	if _, err := sac.Patch(context.Background(), serviceAccount, types.StrategicMergePatchType, []byte(patchData), patchOpts); err != nil {
		return "", fmt.Errorf("failed to associate role to service account: %v", err)
	}

	args := bootstrap.BootstrapArgs{
		Printer:        printer,
		External:       external,
		Platform:       "ecs",
		ServiceAccount: serviceAccount,
		Namespace:      ns,
	}
	t, err := bootstrap.GenerateToken(kc, args)
	if err != nil {
		return "", fmt.Errorf("failed to generate token: %v", err)
	}
	return t.Encode(), nil
}

func getRole(curSa *corev1.ServiceAccount) string {
	curRole := curSa.Annotations[ecsplatform.SoloRoleArnAnnotation]
	if curRole == "" {
		curRole = curSa.Annotations[ecsplatform.AwsRoleArnAnnotation]
	}
	return curRole
}

func fetchZtunnelImage(kc kube.CLIClient, ctx cli.Context) (string, error) {
	zt, err := kc.Kube().AppsV1().DaemonSets(ctx.IstioNamespace()).Get(context.Background(), "ztunnel", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get Ztunnel image: %v", err)
	}
	ztc := slices.FindFunc(zt.Spec.Template.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == "istio-proxy"
	})
	if ztc == nil {
		return "", errors.New("failed to get Ztunnel image: ztunnel daemonset found but does not have 'istio-proxy' container")
	}
	return ztc.Image, nil
}
