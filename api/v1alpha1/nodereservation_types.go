// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeReservationSpec is a reservation as the console keeps it: of a
// node of one of its clusters.
type NodeReservationSpec struct {
	// Cluster: the node's cluster ("" in a console of one cluster).
	//
	// +optional
	Cluster string `json:"cluster,omitempty"`

	ReservationSpec `json:",inline"`
}

// NodeReservation is one user's hold on one whole node for a period,
// kept by the console in its own cluster — not in the node's — and
// carried out by the console: it hibernates other users' DevPods on the
// node when the period starts and keeps them off it until it ends. Its
// status is the console's record of what it did.
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

	Spec   NodeReservationSpec `json:"spec,omitempty"`
	Status ReservationStatus   `json:"status,omitempty"`
}

// NodeReservationList is a list of NodeReservation.
//
// +kubebuilder:object:root=true
type NodeReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeReservation `json:"items"`
}

// Reservation is it as a Reservation: what the reservation rules (which
// take Reservations) work on.
func (n *NodeReservation) Reservation() Reservation {
	return Reservation{ObjectMeta: n.ObjectMeta, Spec: n.Spec.ReservationSpec, Status: n.Status}
}

func init() {
	SchemeBuilder.Register(&NodeReservation{}, &NodeReservationList{})
}
