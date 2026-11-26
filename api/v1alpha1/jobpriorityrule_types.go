// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JobPriorityRuleSpec caps the priority some users may give their jobs:
// a user's cap in a cluster is the highest MaxPriority of the rules whose
// Condition holds for them there (none: 0; admins have none).
type JobPriorityRuleSpec struct {
	// Condition: who and where, in the access rules' language (user,
	// group, cluster), e.g. `group = lab and cluster = st`.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Condition string `json:"condition"`
	// MaxPriority: the highest priority they may give.
	MaxPriority int32 `json:"maxPriority"`
}

// JobPriorityRule is one priority cap, kept by the console in its own
// cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Condition",type=string,JSONPath=`.spec.condition`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxPriority`
type JobPriorityRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec JobPriorityRuleSpec `json:"spec,omitempty"`
}

// JobPriorityRuleList is a list of JobPriorityRule.
//
// +kubebuilder:object:root=true
type JobPriorityRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JobPriorityRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&JobPriorityRule{}, &JobPriorityRuleList{})
}
