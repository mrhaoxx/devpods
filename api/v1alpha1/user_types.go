// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UserQuota caps aggregate resources across a User's DevPods. All
// fields are optional; absent fields fall back to the webui's global
// defaults. Enforced by the webui backend only — the controller and
// gateway ignore it entirely (UI-layer policy, not a security
// barrier; see the webui design spec §9).
type UserQuota struct {
	// MaxDevPods limits how many DevPod CRs the user may own.
	//
	// +optional
	MaxDevPods *int32 `json:"maxDevPods,omitempty"`

	// Compute caps the SUM of container resource limits (containers +
	// initContainers) across the user's *running* DevPods. Keys: cpu,
	// memory, and extended resources such as nvidia.com/gpu.
	//
	// +optional
	Compute corev1.ResourceList `json:"compute,omitempty"`

	// Storage caps the SUM of spec.persistence.size across ALL of the
	// user's DevPods, hibernated included (PVCs survive hibernation).
	//
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
}

// UserSpec defines the desired state of a DevPod user.
type UserSpec struct {
	// Pubkeys is the list of OpenSSH-format authorized public keys for
	// this user. May be empty: the webui auto-provisions keyless Users
	// on first OAuth login, and the gateway falls back to LDAP (or
	// denies) when no key matches.
	//
	// +optional
	Pubkeys []string `json:"pubkeys,omitempty"`

	// OIDCSubject is reserved for a future OIDC binding. The v1alpha1
	// controller does nothing with this value.
	//
	// +optional
	OIDCSubject string `json:"oidcSubject,omitempty"`

	// DisplayName is a cosmetic label for UIs.
	//
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Quota caps this user's aggregate DevPod resources. nil = webui
	// global defaults.
	//
	// +optional
	Quota *UserQuota `json:"quota,omitempty"`
}

// UserStatus reports observed state.
type UserStatus struct {
	// Conditions is the standard list of conditions.
	//
	// +optional
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// DevPodCount is the number of DevPods currently owned by this user.
	//
	// +optional
	DevPodCount int32 `json:"devPodCount,omitempty"`
}

// User is the schema for DevPod users.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=du
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="DevPods",type=integer,JSONPath=`.status.devPodCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9-]{1,32}$')",message="user name must match [a-z0-9-]{1,32}"
type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserSpec   `json:"spec,omitempty"`
	Status UserStatus `json:"status,omitempty"`
}

// UserList is a list of User.
//
// +kubebuilder:object:root=true
type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}

func init() {
	SchemeBuilder.Register(&User{}, &UserList{})
}
