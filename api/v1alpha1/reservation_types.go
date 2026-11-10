// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ReservableNodeLabel marks a node users may reserve.
	ReservableNodeLabel = "devpod.io/reservable"
	// HibernatedByAnnotation on a DevPod names what hibernated it,
	// e.g. "reservation/<name>".
	HibernatedByAnnotation = "devpod.io/hibernated-by"

	// DevPod condition types set by the reservation machinery.
	ConditionNodeReserved        = "NodeReserved"        // pinned to a node someone else holds: no Pod
	ConditionReservationClearing = "ReservationClearing" // on a node about to be someone else's
	ConditionReservationEnding   = "ReservationEnding"   // the holder's DevPod, reservation about to end
)

// ReservationPhase is where a reservation is on its timeline.
// +kubebuilder:validation:Enum=Upcoming;Active;Ended;Cancelled
type ReservationPhase string

const (
	ReservationUpcoming  ReservationPhase = "Upcoming"
	ReservationActive    ReservationPhase = "Active"
	ReservationEnded     ReservationPhase = "Ended"
	ReservationCancelled ReservationPhase = "Cancelled"
)

// ReservationCancel records who cancelled (or released early) and when.
type ReservationCancel struct {
	By string      `json:"by"`
	At metav1.Time `json:"at"`
}

// ReservationSpec: one user holds one whole node for [Start, End).
// During that time only that user's DevPods may run on the node.
type ReservationSpec struct {
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`
	// +kubebuilder:validation:MinLength=1
	User  string      `json:"user"`
	Start metav1.Time `json:"start"`
	End   metav1.Time `json:"end"`

	// Cancelled is written by the webui on cancel or early release; the
	// controller then treats the reservation as over.
	//
	// +optional
	Cancelled *ReservationCancel `json:"cancelled,omitempty"`

	// Extends names the reservation this one prolongs: an extension is a
	// new segment whose Start equals that one's End.
	//
	// +optional
	Extends string `json:"extends,omitempty"`
}

// ReservationStatus is written by the controller.
type ReservationStatus struct {
	// +optional
	Phase ReservationPhase `json:"phase,omitempty"`
	// NotifiedClearAt: the clearing notice went out to the DevPods then
	// on the node.
	//
	// +optional
	NotifiedClearAt *metav1.Time `json:"notifiedClearAt,omitempty"`
	// ClearedDevPods: other users' DevPods hibernated at the start.
	//
	// +optional
	ClearedDevPods []string `json:"clearedDevPods,omitempty"`
	// HibernatedAtEnd: the holder's DevPods hibernated at the end.
	//
	// +optional
	HibernatedAtEnd []string `json:"hibernatedAtEnd,omitempty"`
}

// Reservation is a node reservation.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=rsv
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.node`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.user`
// +kubebuilder:printcolumn:name="Start",type=string,JSONPath=`.spec.start`
// +kubebuilder:printcolumn:name="End",type=string,JSONPath=`.spec.end`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type Reservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReservationSpec   `json:"spec,omitempty"`
	Status ReservationStatus `json:"status,omitempty"`
}

// ReservationList is a list of Reservation.
//
// +kubebuilder:object:root=true
type ReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Reservation `json:"items"`
}

// Overlaps reports whether the reservation is live (not cancelled) and
// its [Start, End) intersects [from, to).
func (r *Reservation) Overlaps(from, to metav1.Time) bool {
	return r.Spec.Cancelled == nil && r.Spec.Start.Time.Before(to.Time) && from.Time.Before(r.Spec.End.Time)
}

// HeldAt reports whether the reservation holds its node at t.
func (r *Reservation) HeldAt(t metav1.Time) bool {
	return r.Spec.Cancelled == nil && !t.Time.Before(r.Spec.Start.Time) && t.Time.Before(r.Spec.End.Time)
}

func init() {
	SchemeBuilder.Register(&Reservation{}, &ReservationList{})
}
