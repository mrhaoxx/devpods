// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GrantPermission is a management permission a grant gives (design
// 2026-11-20 §6).
//
// +kubebuilder:validation:Enum=setReservable;viewAudit;disconnectSessions;manageReservations
type GrantPermission string

const (
	// PermSetReservable: switch a machine reservable or not.
	PermSetReservable GrantPermission = "setReservable"
	// PermViewAudit: see the audit (every read-only record, live view
	// too) of the sessions on a machine.
	PermViewAudit GrantPermission = "viewAudit"
	// PermDisconnectSessions: cut the sessions on a machine; given only
	// together with PermViewAudit.
	PermDisconnectSessions GrantPermission = "disconnectSessions"
	// PermManageReservations: on a machine, what an admin may do with its
	// reservations — reserve for someone, change or end someone else's
	// (design 2026-11-24).
	PermManageReservations GrantPermission = "manageReservations"
)

// AccessGrantSpec: a condition — the access rules' language, on the user
// granted and the cluster and machine acted on — and what it grants.
type AccessGrantSpec struct {
	// Match: the condition (cluster, machine, user, group; and, or, not);
	// never empty — to grant everyone, write everyone.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2000
	Match string `json:"match"`
	// Permissions granted where the condition holds.
	//
	// +kubebuilder:validation:MinItems=1
	Permissions []GrantPermission `json:"permissions"`
	// Description: for people.
	//
	// +optional
	Description string `json:"description,omitempty"`
}

// AccessGrant gives management permissions on machines to users — one
// grant per object, in the console's cluster; grants add up, none takes
// away. Admins have every permission anyway.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type AccessGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AccessGrantSpec `json:"spec,omitempty"`
}

// AccessGrantList is a list of AccessGrant.
//
// +kubebuilder:object:root=true
type AccessGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AccessGrant{}, &AccessGrantList{})
}
