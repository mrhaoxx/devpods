// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeReservationSpec is one user's share of one machine (of one of the
// console's clusters) for a while — some of its cards and cores, or all of
// it — guaranteed: a machine's reservations never add up to more than it
// has. Where the access rules say so, it is also the permit to use it.
type NodeReservationSpec struct {
	// Cluster: the node's cluster ("" in a console of one cluster).
	//
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`
	// +kubebuilder:validation:MinLength=1
	User  string      `json:"user"`
	Start metav1.Time `json:"start"`
	End   metav1.Time `json:"end"`
	// GPU: the cards reserved.
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	GPU *int32 `json:"gpu,omitempty"`
	// CPU: the cores reserved. Both absent: the whole machine, which
	// overlaps no other reservation; one alone: none of the other.
	//
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// Purpose: what it is for, as its holder says — one line.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=200
	Purpose string `json:"purpose,omitempty"`
	// Cancelled: ended early (or never started) — by whom, when.
	//
	// +optional
	Cancelled *ReservationCancel `json:"cancelled,omitempty"`
}

// NodeReservationStatus is the console's record of a reservation's end.
type NodeReservationStatus struct {
	// Ended: its end was carried out.
	//
	// +optional
	Ended bool `json:"ended,omitempty"`
	// HibernatedAtEnd: the holder's DevPods hibernated then.
	//
	// +optional
	HibernatedAtEnd []string `json:"hibernatedAtEnd,omitempty"`
	// Cleared: other users' DevPods hibernated to give it its share.
	//
	// +optional
	Cleared []string `json:"cleared,omitempty"`
}

// NodeReservation is one user's share of one machine for a period, kept
// by the console in its own cluster: when it starts the console clears
// whoever is in its way (after a notice); when it ends the console
// hibernates the holder's DevPods there that only it let run.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nrsv
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.node`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.user`
// +kubebuilder:printcolumn:name="Start",type=string,JSONPath=`.spec.start`
// +kubebuilder:printcolumn:name="End",type=string,JSONPath=`.spec.end`
type NodeReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeReservationSpec   `json:"spec,omitempty"`
	Status NodeReservationStatus `json:"status,omitempty"`
}

// NodeReservationList is a list of NodeReservation.
//
// +kubebuilder:object:root=true
type NodeReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeReservation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeReservation{}, &NodeReservationList{})
}
