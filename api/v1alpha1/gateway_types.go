// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GatewaySpec registers an SSH and port-link gateway with the console.
// A gateway dials out to the console and asks it who may log in where;
// the agents of the clusters dial out to the gateway, which reaches
// DevPods over those connections. Nobody dials a cluster.
type GatewaySpec struct {
	// DisplayName shown in the console.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=80
	DisplayName string `json:"displayName,omitempty"`

	// SSHAddress is what users connect to: host or host:port.
	//
	// +optional
	SSHAddress string `json:"sshAddress,omitempty"`

	// HTTPBaseDomain: port links of DevPods served by this gateway are
	// https://{id}-{port}{HTTPSuffix}.<HTTPBaseDomain>[:HTTPPort].
	//
	// +optional
	HTTPBaseDomain string `json:"httpBaseDomain,omitempty"`
	// +optional
	HTTPSuffix string `json:"httpSuffix,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	HTTPPort int32 `json:"httpPort,omitempty"`

	// AgentAddress is where the clusters' agents connect to this gateway,
	// e.g. https://gw.example.com (they dial <AgentAddress>/agent/connect).
	//
	// +optional
	AgentAddress string `json:"agentAddress,omitempty"`

	// PublicKey is the gateway's identity, an authorized_keys line,
	// recorded when it joins with a one-time token.
	//
	// +optional
	PublicKey string `json:"publicKey,omitempty"`

	// Paused: the gateway is refused by the console and by the agents.
	//
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// GatewayStatus is what the console last heard of the gateway.
type GatewayStatus struct {
	// LastSeen: the gateway's latest question to the console.
	//
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
	// Clusters whose agents are connected to this gateway.
	//
	// +optional
	// +listType=set
	Clusters []string `json:"clusters,omitempty"`
}

// Gateway is an SSH and port-link gateway known to this console.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dpgw
// +kubebuilder:printcolumn:name="SSH",type=string,JSONPath=`.spec.sshAddress`
// +kubebuilder:printcolumn:name="Last seen",type=date,JSONPath=`.status.lastSeen`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 32",message="a gateway name is at most 32 characters"
type Gateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewaySpec   `json:"spec,omitempty"`
	Status GatewayStatus `json:"status,omitempty"`
}

// GatewayList is a list of Gateway.
//
// +kubebuilder:object:root=true
type GatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Gateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Gateway{}, &GatewayList{})
}
