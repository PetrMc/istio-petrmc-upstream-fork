// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

// nolint:lll
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:categories={solo-io,admin},shortName=seg
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Domain",type="string",JSONPath=".spec.domain",description="DNS domain for the segment"
// +kubebuilder:printcolumn:name="Local",type="string",JSONPath=".status.conditions[?(@.type=='Local')].status",description="Whether the segment is used by the local cluster"
// +kubebuilder:printcolumn:name="Connected",type="integer",JSONPath=".status.connectedPeers",description="Number of connected peers"
// +kubebuilder:printcolumn:name="Rejected",type="integer",JSONPath=".status.rejectedPeers",description="Number of rejected peers"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Server time when this object was created."
// +kubebuilder:validation:Optional

// Segment defines a network segment with its own DNS domain
type Segment struct {
	metav1.TypeMeta   `json:",inline"` // nolint: revive
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SegmentSpec   `json:"spec,omitempty"`
	Status SegmentStatus `json:"status,omitempty"`
}

// SegmentSpec defines the desired state of Segment
type SegmentSpec struct {
	// Domain is the DNS domain for this segment.
	// Must be a valid DNS name and cannot be "cluster.local".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Domain string `json:"domain"`
}

// SegmentStatus defines the observed state of Segment
type SegmentStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Peers lists all peer clusters that have declared this segment
	// +optional
	Peers []PeerStatus `json:"peers,omitempty"`

	// ConnectedPeers is the count of peers with Status=Connected
	// +optional
	ConnectedPeers int32 `json:"connectedPeers,omitempty"`

	// RejectedPeers is the count of peers with Status=Rejected
	// +optional
	RejectedPeers int32 `json:"rejectedPeers,omitempty"`
}

// PeerStatus describes the observed status of a peer cluster using this segment
type PeerStatus struct {
	// ClusterName is the name of the peer cluster
	ClusterName string `json:"clusterName"`

	// Hostname is the domain the peer is using for this segment
	Hostname string `json:"hostname"`

	// Status indicates the peer's connection status (Connected, Rejected, etc)
	Status string `json:"status"`

	// Reason explains why the peer is in the current status (e.g., HostnameMismatch)
	// +optional
	Reason string `json:"reason,omitempty"`

	// LastTransitionTime is when the status last changed
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SegmentList contains a list of Segment
type SegmentList struct {
	metav1.TypeMeta `json:",inline"` // nolint: revive
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Segment `json:"items"`
}
