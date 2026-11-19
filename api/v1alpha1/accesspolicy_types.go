// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AccessPolicyName is the name of the one AccessPolicy the console reads.
const AccessPolicyName = "default"

// AccessPolicySpec is who may use which clusters and machines, and how:
// rules in order, the first whose resource and subject match deciding;
// Default when none does.
type AccessPolicySpec struct {
	// +optional
	Rules []AccessRule `json:"rules,omitempty"`
	// Default: the effect when no rule matches (the last rule, always there).
	Default AccessEffect `json:"default"`
}

// AccessRule: a condition on the cluster, the machine and the user, and
// what those it holds for get. Match is the condition; a rule without it
// is in the old form (Cluster, Machines, Subject), read as the condition
// they make.
type AccessRule struct {
	// Match: the condition — tests of cluster, machine, user and group
	// joined by and, or, not and parentheses (cluster in (st2, a5) and
	// machine != st5); empty = everything. Set, the old fields below are
	// ignored.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=2000
	Match string `json:"match,omitempty"`
	// Cluster (the old form): a cluster's name; empty = any cluster.
	//
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// Machines (the old form): empty = every machine (of Cluster); else
	// any of these matches — a node name, "*" globs allowed, or a label
	// "key=value".
	//
	// +optional
	Machines []string `json:"machines,omitempty"`
	// Subject (the old form): who the rule is about; none = everyone.
	//
	// +optional
	Subject *AccessSubject `json:"subject,omitempty"`
	Effect  AccessEffect   `json:"effect"`
}

// AccessSubject: everyone, or some users and UserGroups.
type AccessSubject struct {
	// Kind: "everyone", or "some" — the users named in Users and the
	// members of the groups named in Groups, any of them.
	//
	// +kubebuilder:validation:Enum=everyone;some
	Kind string `json:"kind"`
	// +optional
	Users []string `json:"users,omitempty"`
	// +optional
	Groups []string `json:"groups,omitempty"`
}

// AccessEffect: deny (Allow false), or allow with what comes with it.
type AccessEffect struct {
	Allow bool `json:"allow"`
	// Privileged (the old form): all of Privileges. Ignored when
	// Privileges is set.
	//
	// +optional
	Privileged bool `json:"privileged,omitempty"`
	// Privileges: what beyond an ordinary container may be asked for.
	//
	// +optional
	Privileges *AccessPrivileges `json:"privileges,omitempty"`
	// HostPaths: the host directories a DevPod may mount (its home, put
	// there by the controller, aside).
	//
	// +optional
	HostPaths []HostPathGrant `json:"hostPaths,omitempty"`
	// NeedsReservation: only while the DevPod's owner holds a
	// reservation of the machine.
	//
	// +optional
	NeedsReservation bool `json:"needsReservation,omitempty"`
}

// AccessPrivileges: each of what makes a container more than an
// ordinary one, granted on its own.
type AccessPrivileges struct {
	// Privileged: privileged containers (every capability with them).
	//
	// +optional
	Privileged bool `json:"privileged,omitempty"`
	// Capabilities that may be added (NET_ADMIN, SYS_PTRACE; with or
	// without CAP_, any case); "*" = any, ALL included.
	//
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`
	// +optional
	HostPID bool `json:"hostPID,omitempty"`
	// +optional
	HostIPC bool `json:"hostIPC,omitempty"`
	// +optional
	HostNetwork bool `json:"hostNetwork,omitempty"`
}

// HostPathGrant: a host directory and what is below it; read-only unless
// Writable. A writable one is no security boundary (a symlink in it can
// lead anywhere on the host): granting it is trusting the user.
type HostPathGrant struct {
	Path string `json:"path"`
	// +optional
	Writable bool `json:"writable,omitempty"`
}

// DefaultAccessEffect: everything allowed, as before access control.
func DefaultAccessEffect() AccessEffect {
	return AccessEffect{
		Allow:      true,
		Privileges: &AccessPrivileges{Privileged: true, Capabilities: []string{"*"}, HostPID: true, HostIPC: true, HostNetwork: true},
		HostPaths:  []HostPathGrant{{Path: "/", Writable: true}},
	}
}

// AccessPolicy is the console's access control: one, named "default", in
// the console's cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=acl
type AccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AccessPolicySpec `json:"spec,omitempty"`
}

// AccessPolicyList is a list of AccessPolicy.
//
// +kubebuilder:object:root=true
type AccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AccessPolicy{}, &AccessPolicyList{})
}
