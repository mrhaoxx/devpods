// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UserGroupSpec: a group of users, named in access rules.
type UserGroupSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Members: user names.
	//
	// +optional
	Members []string `json:"members,omitempty"`
}

// UserGroup is a group of users the console's access rules can name; kept
// in the console's cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ug
// +kubebuilder:printcolumn:name="Members",type=string,JSONPath=`.spec.members`
type UserGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec UserGroupSpec `json:"spec,omitempty"`
}

// UserGroupList is a list of UserGroup.
//
// +kubebuilder:object:root=true
type UserGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UserGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UserGroup{}, &UserGroupList{})
}
