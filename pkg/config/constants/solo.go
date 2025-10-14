// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package constants

const (
	// Solo status condition types
	SoloConditionPeeringSucceeded = "gloo.solo.io/PeeringSucceeded"
	SoloConditionPeerConnected    = "gloo.solo.io/PeerConnected"

	// Solo annotations
	// applied to a istio-eastwest gateway resource, indicates the type of data plane service to use for peering.
	// istio sends e/w gateway k8s service hbone NodePort and Node IP as Workload to peer.
	SoloAnnotationPeeringDataPlaneServiceType = "peering.solo.io/data-plane-service-type"
	// applied to a istio-remote gateway resource, indicates the preferred type of data plane service to use for peering.
	// istio configures dataplane based on peered Node IPs and e/w gateway NodePort info.
	SoloAnnotationPeeringPreferredDataPlaneServiceType = "peering.solo.io/preferred-data-plane-service-type"
)
