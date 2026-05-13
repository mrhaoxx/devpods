// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UserSpec defines the desired state of a DevPod user.
type UserSpec struct {
	// Pubkeys is the list of OpenSSH-format authorized public keys for this
	// user. At least one key is required.
	//
	// +kubebuilder:validation:MinItems=1
	Pubkeys []string `json:"pubkeys"`

	// OIDCSubject is reserved for a future OIDC binding. The v1alpha1
	// controller does nothing with this value.
	//
	// +optional
	OIDCSubject string `json:"oidcSubject,omitempty"`

	// DisplayName is a cosmetic label for UIs.
	//
	// +optional
	DisplayName string `json:"displayName,omitempty"`
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
