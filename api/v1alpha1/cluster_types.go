// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterSpec registers a cluster with the console that manages it. The
// cluster's agent dials out to the console and proves itself with the
// key recorded here; the console never dials a cluster.
type ClusterSpec struct {
	// DisplayName shown in the console.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=80
	DisplayName string `json:"displayName,omitempty"`

	// AgentPublicKey is the agent's identity, an authorized_keys line. It
	// is recorded when the agent joins with a one-time token; empty until
	// then.
	//
	// +optional
	AgentPublicKey string `json:"agentPublicKey,omitempty"`

	// Gateway names the Gateway whose SSH address and port-link domain
	// are shown for this cluster's DevPods. Empty: the console's own.
	//
	// +optional
	Gateway string `json:"gateway,omitempty"`

	// DefaultQuota applies in this cluster to users without an entry of
	// their own for it.
	//
	// +optional
	DefaultQuota *UserQuota `json:"defaultQuota,omitempty"`

	// Paused: the agent's connections are refused and dropped.
	//
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// ClusterStatus is what the console last heard from the cluster's agent.
type ClusterStatus struct {
	Connected bool `json:"connected"`

	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`
	// +optional
	ControllerVersion string `json:"controllerVersion,omitempty"`
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// Gateways the cluster's agent is connected to.
	//
	// +optional
	// +listType=set
	Gateways []string `json:"gateways,omitempty"`
	// Capabilities the cluster offers: snapshots, reservations, telemetry…
	//
	// +optional
	// +listType=set
	Capabilities []string `json:"capabilities,omitempty"`
}

// Cluster is a cluster of DevPods managed by this console. Its name is
// what users write after a DevPod's name when two clusters hold one of
// the same name, so it cannot be "devpod" (the CLI's login name).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dpcl
// +kubebuilder:printcolumn:name="Connected",type=boolean,JSONPath=`.status.connected`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.status.agentVersion`
// +kubebuilder:printcolumn:name="Last seen",type=date,JSONPath=`.status.lastSeen`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name != 'devpod' && size(self.metadata.name) <= 32",message="a cluster name is at most 32 characters and cannot be 'devpod'"
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

// ClusterList is a list of Cluster.
//
// +kubebuilder:object:root=true
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{})
}
