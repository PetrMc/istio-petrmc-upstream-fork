// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"istio.io/istio/pkg/ptr"
)

// ECSClient abstracts the portions of ECS client we use for testing purposes.
type ECSClient interface {
	ecs.ListTasksAPIClient
	ecs.DescribeTasksAPIClient

	ecs.ListServicesAPIClient
	ecs.DescribeServicesAPIClient
}

// Kubernetes objects metadata
const (
	// ResourceAnnotation indicates the full (internal) resource name of the object's source
	ResourceAnnotation = "ecs.solo.io/resource"
	// ARNAnnotation indicates the full ARN name of the object's source
	ARNAnnotation = "ecs.solo.io/arn"
	// ServiceLabel indicates the ECS Service name
	ServiceLabel = "ecs.solo.io/service"
)

// ECS Tag
const (
	// AWSManagedServiceNameTag is automatically added to tasks in some cases
	// (https://docs.aws.amazon.com/AmazonECS/latest/developerguide/ecs-using-tags.html#managed-tags)
	AWSManagedServiceNameTag = "aws:ecs:serviceName"

	// ServiceHostnameTag allows overriding the hostname the service can be reached at.
	// Defaults to <name>.<cluster>.local.
	ServiceHostnameTag = "ecs.solo.io/hostname"
	// ServiceNamespaceTag allows overriding the namespace the objects exist in.
	// Currently, this MUST apply to both the Service and Task, which differs from other tags.
	// Defaults to 'ecs'.
	ServiceNamespaceTag = "ecs.solo.io/namespace"
	// ServicePortsTag allows overriding the ports for the service.
	// Accepts a forward slash-separated list of name:port[:targetPort] pairs.
	// Example: 'http:80:8080/tcp:9090'. This will expose 80 (forwarding to port 8080 locally) and 9090 (directly)
	// Default is 'http:80'.
	ServicePortsTag = "ecs.solo.io/ports"
	// ServiceAccountTag allows overriding the service account for the service.
	// Default is 'default'.
	ServiceAccountTag = "ecs.solo.io/service-account"
	// Also available: any tag with '*.istio.io/' naming will be passed to labels
)

type EcsWorkload struct {
	ARN     string
	Group   string
	Address netip.Addr
	Zone    string
	Mesh    bool
	Tags    EcsTags
}

func (w EcsWorkload) GroupName() string {
	// More reliable method: aws told us the service name
	if w.Tags.ServiceName != "" {
		return w.Tags.ServiceName
	}
	// Probably less reliable but seems good enough in practice: guess from the 'Group' format
	_, service, _ := strings.Cut(w.Group, ":")
	return service
}

func (w EcsWorkload) HasService() bool {
	groupKind, _, _ := strings.Cut(w.Group, ":")
	return groupKind == "service"
}

type EcsService struct {
	Name    string
	Address netip.Addr
	Tags    EcsTags
}

type Port struct {
	Protocol    string
	ServicePort int
	TargetPort  int
}

type EcsTags struct {
	Hostname       string
	Namespace      string
	ServiceAccount string
	Ports          []Port
	ServiceName    string
	// Labels we will passthrough to the underlying objects
	Passthrough map[string]string
}

type EcsDiscovered struct {
	Cluster string
	// One or other
	Workload *EcsWorkload
	Service  *EcsService
}

func (d EcsDiscovered) ResourceName() string {
	if d.Workload != nil {
		return fmt.Sprintf("workload/%s/%s/%s/%s", d.Cluster, d.Namespace(), d.Workload.GroupName(), d.Workload.ARN)
	}
	return fmt.Sprintf("service/%s/%s/%s", d.Cluster, d.Namespace(), d.Service.Name)
}

func ParseResourceName(name string) (types string, cluster string, namespace string, identifier string, err error) {
	segs := strings.SplitN(name, "/", 4)
	if len(segs) != 4 {
		return "", "", "", "", fmt.Errorf("invalid resource name: %v", name)
	}
	return segs[0], segs[1], segs[2], segs[3], nil
}

func (d EcsDiscovered) Type() string {
	if d.Workload != nil {
		return "workload"
	}
	return "service"
}

// Namespace defaults to cluster name if not set in tags
func (d EcsDiscovered) Namespace() string {
	if d.Workload != nil {
		return ptr.NonEmptyOrDefault(d.Workload.Tags.Namespace, d.Cluster)
	}
	return ptr.NonEmptyOrDefault(d.Service.Tags.Namespace, d.Cluster)
}

func ParseTags(tag []ecstypes.Tag) EcsTags {
	ret := EcsTags{}
	for _, t := range tag {
		if t.Key == nil || t.Value == nil {
			// I don't think Key can actually be nil, but value can; either way, we only care about set values so we can skip those
			continue
		}
		v := *t.Value
		k := *t.Key
		switch k {
		case ServiceHostnameTag:
			ret.Hostname = v
		case ServiceNamespaceTag:
			ret.Namespace = v
		case ServiceAccountTag:
			ret.ServiceAccount = v
		case AWSManagedServiceNameTag:
			ret.ServiceName = v
		case ServicePortsTag:
			for _, ports := range strings.Split(v, "/") {
				parts := strings.Split(ports, ":")
				var protocol, sp, tp string
				if len(parts) != 2 && len(parts) != 3 {
					log.Warnf("failed to parse %v: %v", k, v)
					continue
				}
				protocol = parts[0]
				sp = parts[1]
				if len(parts) == 3 {
					tp = parts[2]
				}
				port := Port{Protocol: strings.ToUpper(protocol)}
				spt, err := strconv.Atoi(sp)
				if err != nil {
					log.Warnf("failed to parse %v: %v", k, v)
					continue
				}
				port.ServicePort = spt
				if tp != "" {
					tpt, err := strconv.Atoi(tp)
					if err != nil {
						log.Warnf("failed to parse %v: %v", k, v)
						continue
					}
					port.TargetPort = tpt
				}
				ret.Ports = append(ret.Ports, port)
			}
		}
		if len(validation.IsQualifiedName(k)) == 0 && len(validation.IsValidLabelValue(v)) == 0 {
			// If label k/v are valid in Kubernetes we will pass through
			if ret.Passthrough == nil {
				ret.Passthrough = make(map[string]string)
			}
			ret.Passthrough[k] = v
		}
	}
	return ret
}
