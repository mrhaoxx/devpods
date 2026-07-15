// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BindingSpec carries the Kore binding block a template stamps onto
// DevPods created from it: the kore.zjusct.io/* annotations plus the
// container resources the binding implies. The webui validates the
// block against Kore's admission rules at template-save time and is
// the only writer of these annotations on non-admin DevPods.
type BindingSpec struct {
	// Annotations restricted to the kore.zjusct.io/* whitelist
	// (pin, pool, pool-size, numa-policy, memory-policy, placement,
	// smt-policy — NOT cpuset, which stays an admin escape hatch).
	Annotations map[string]string `json:"annotations"`

	// Resources the binding implies for the target container. For
	// pin templates: integer CPU with requests == limits.
	Resources corev1.ResourceRequirements `json:"resources"`
}

// PodPresetSpec fixes the user-visible knobs of a one-click preset.
type PodPresetSpec struct {
	// Image for the single "dev" container.
	//
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Resources for the "dev" container. A Binding on the same
	// template overrides overlapping keys.
	//
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Persistence default for DevPods created from this preset.
	//
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// Shell passed through to DevPodSpec.Shell.
	//
	// +optional
	// +kubebuilder:validation:Enum=bash;zsh;fish
	Shell string `json:"shell,omitempty"`

	// Tolerations added to the dev pod, e.g. to schedule onto a tainted
	// (experimental) node or into a partition guarded by a taint.
	//
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector added to the dev pod, e.g. to pin a preset to a
	// specific node class.
	//
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// AutomountServiceAccountToken opts the preset's pod into mounting the
	// namespace's ServiceAccount token. Absent (nil) means the platform's
	// secure default (no token in the container); set true only for presets
	// whose workload genuinely needs API access.
	//
	// +optional
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken,omitempty"`
}

// DevPodTemplateSpec defines an admin-curated template. At least one
// of Binding / PodPreset must be set: binding-only templates act as
// overlays users attach to custom DevPods; templates with PodPreset
// are one-click presets.
//
// +kubebuilder:validation:XValidation:rule="has(self.binding) || has(self.podPreset)",message="at least one of binding or podPreset must be set"
type DevPodTemplateSpec struct {
	// DisplayName is the human-readable name shown in the picker.
	//
	// +kubebuilder:validation:MinLength=1
	DisplayName string `json:"displayName"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	Binding *BindingSpec `json:"binding,omitempty"`

	// +optional
	PodPreset *PodPresetSpec `json:"podPreset,omitempty"`
}

// DevPodTemplate is an admin-curated create template. Cluster-scoped;
// read-only for ordinary users (via the webui), CRUD for admins.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=dpt
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DevPodTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DevPodTemplateSpec `json:"spec,omitempty"`
}

// DevPodTemplateList is a list of DevPodTemplate.
//
// +kubebuilder:object:root=true
type DevPodTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevPodTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DevPodTemplate{}, &DevPodTemplateList{})
}
