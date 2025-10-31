// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/ptr"
)

// Kubernetes objects metadata
const (
	// ResourceAnnotation indicates the full (internal) resource name of the object's source
	ResourceAnnotation = "ecs.solo.io/resource"
	// ARNAnnotation indicates the full ARN name of the object's source
	ARNAnnotation = "ecs.solo.io/arn"
	// DiscoveredByAnnotation indicates the full ARN name of the role which discovered the object's source.
	// As a special case, this can be "default" which refers to the default role used when no role-chaining
	// is performed.
	DiscoveredByAnnotation = "ecs.solo.io/discovered-by"
	// ClusterAnnotation indicates the ECS cluster this resource is a part of.
	ClusterAnnotation = "ecs.solo.io/cluster"

	// ServiceLabel indicates the ECS Service name
	ServiceLabel      = "ecs.solo.io/service"
	HostnameLabel     = "ecs.solo.io/hostname"
	WorkloadNameLabel = "service.istio.io/workload-name"
)

// ECS Tag
const (
	// AWSManagedServiceNameTag is automatically added to tasks in some cases
	// (https://docs.aws.amazon.com/AmazonECS/latest/developerguide/ecs-using-tags.html#managed-tags)
	AWSManagedServiceNameTag = "aws:ecs:serviceName"
	// HostnameTag allows overriding the hostname the service/task can be reached at. Since the hostname is used
	// as the WorkloadSelector on the ServiceEntry, this can be used to create routing patterns like load balancing
	// between tasks in different services, clusters, accounts, or even between ECS tasks and k8s pods.
	//
	// Defaults to '<service>.<cluster>.<account_domain>'.
	HostnameTag = "ecs.solo.io/hostname"
	// NamespaceTag allows overriding the namespace that ServiceEntries and WorkloadEntries will be created in.
	//
	// Defaults to 'istio-system'.
	NamespaceTag = "ecs.solo.io/namespace"
	// ServicePortsTag allows overriding the ports for the service.
	// Accepts a forward slash-separated list of name:port[:targetPort] pairs.
	//
	// Example: 'http:80:8080/tcp:9090'. This will expose 80 (forwarding to port 8080 locally) and 9090 (directly)
	//
	// Default is 'http:80'.
	ServicePortsTag = "ecs.solo.io/ports"
	// ServiceAccountTag allows overriding the service account for the service.
	//
	// Default is 'default'.
	ServiceAccountTag = "ecs.solo.io/service-account"
	// DiscoveryEnabledTag enables discovery on an ECS cluster.
	//
	// Default is 'false'.
	DiscoveryEnabledTag = "ecs.solo.io/discovery-enabled"

	// Also available: any tag with '*.istio.io/' naming will be passed to labels
)

type EcsCluster struct {
	Account string
	Name    string
}

type EcsTask struct {
	Group   string
	Address netip.Addr
	// AWS availability zone
	Zone string
	// whether or not the workload is a part of the mesh
	Mesh bool
}

// ServiceName returns the name of the service/group the of the discovered resource. This is set in precedence of:
//  1. If of type Service, the Service name
//  2. The "aws:ecs:serviceName" tag on the ECS Task (AWS managed)
//  3. The Group name of the ECS Task (could be service or task group)
func (d EcsDiscovered) ServiceName() string {
	// if service, return name
	if d.Service != nil {
		return d.Service.Name
	}

	// aws defined
	if d.Tags.AWSManagedServiceName != "" {
		return d.Tags.AWSManagedServiceName
	}
	// fallback: the Task group takes the form "<group_type>:<group_name>" where a Task
	// that is part of Service will be "service:<service_name>"
	_, service, _ := strings.Cut(d.Task.Group, ":")
	return service
}

type EcsService struct {
	Name    string
	Address netip.Addr
}

type Port struct {
	Protocol    string
	ServicePort int
	TargetPort  int
}

type EcsTags struct {
	Hostname              string
	Namespace             string
	ServiceAccount        string
	Ports                 []Port
	AWSManagedServiceName string
	// Used for cluster discovery to enable/disable clusters being monitored by istiod
	// TODO: also use this to enable/disable Services and Tasks?
	DiscoveryTag bool
	// Labels we will passthrough to the underlying objects
	Passthrough map[string]string
}

type EcsDiscovered struct {
	Domain  string
	Cluster string
	ARN     string
	Tags    EcsTags

	// One or other
	Task    *EcsTask
	Service *EcsService
}

func (d EcsDiscovered) ResourceName() string {
	if d.Task != nil {
		return fmt.Sprintf("task/%s/%s", d.Namespace(), d.ARN)
	}
	return fmt.Sprintf("service/%s/%s", d.Namespace(), d.ARN)
}

func (d EcsDiscovered) Hostname() string {
	return ptr.NonEmptyOrDefault(d.Tags.Hostname, fmt.Sprintf("%s.%s.%s", d.ServiceName(), d.Cluster, d.Domain))
}

// ParseResourceName converts the string representation of a ResourceName into the struct representation
func ParseResourceName(name string) (resourceType string, namespace string, a arn.ARN, err error) {
	nameSegs := strings.SplitN(name, "/", 3)
	if len(nameSegs) != 3 {
		return resourceType, namespace, a, fmt.Errorf("invalid resource name: %v", name)
	}
	a, err = arn.Parse(nameSegs[2])
	if err != nil {
		return resourceType, namespace, a, fmt.Errorf("invalid resource ARN %v: %v", nameSegs[2], err)
	}
	return nameSegs[0], nameSegs[1], a, nil
}

func ParseARNResource(resource string) (resourceType string, cluster string, identifier string, err error) {
	segs := strings.SplitN(resource, "/", 3)
	if len(segs) != 3 {
		return resourceType, cluster, identifier, fmt.Errorf("invalid ARN resource: %v", resource)
	}
	return segs[0], segs[1], segs[2], nil
}

// Namespace defaults to istio-system if not set in tags
func (d EcsDiscovered) Namespace() string {
	return ptr.NonEmptyOrDefault(d.Tags.Namespace, constants.IstioSystemNamespace)
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
		case HostnameTag:
			ret.Hostname = v
		case NamespaceTag:
			ret.Namespace = v
		case ServiceAccountTag:
			ret.ServiceAccount = v
		case AWSManagedServiceNameTag:
			ret.AWSManagedServiceName = v
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
		case DiscoveryEnabledTag:
			enabled, err := strconv.ParseBool(v)
			if err != nil {
				// this will spam logs when invalid so only log at debug
				log.Debugf("failed to parse %s tag with value '%s': %v", DiscoveryEnabledTag, v, err)
			}
			ret.DiscoveryTag = enabled
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
