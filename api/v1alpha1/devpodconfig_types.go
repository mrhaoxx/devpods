// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DevPodConfigSpec is a saved DevPod manifest with variables (config
// center spec §2). Only the console reads and writes these; there is no
// controller and no status.
type DevPodConfigSpec struct {
	// Owner is the user the config belongs to: the only one who may
	// change it.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="owner is immutable"
	Owner string `json:"owner"`

	// Title shown in lists.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=80
	Title string `json:"title"`

	// +optional
	// +kubebuilder:validation:MaxLength=500
	Description string `json:"description,omitempty"`

	// Content is the DevPod YAML as the owner wrote it, variables
	// (${VAR}, ${VAR:-default}) included. It is text, not a DevPod: a
	// variable may stand where a number belongs.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=262144
	Content string `json:"content"`

	// SharedWith are the users who may see and use the config besides
	// its owner.
	//
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	SharedWith []string `json:"sharedWith,omitempty"`

	// Public: every signed-in user may see and use the config.
	//
	// +optional
	Public bool `json:"public,omitempty"`
}

// DevPodConfig is a saved, shareable DevPod manifest.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dpc
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="Public",type=boolean,JSONPath=`.spec.public`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DevPodConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DevPodConfigSpec `json:"spec,omitempty"`
}

// DevPodConfigList is a list of DevPodConfig.
//
// +kubebuilder:object:root=true
type DevPodConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevPodConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DevPodConfig{}, &DevPodConfigList{})
}
