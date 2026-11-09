// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
)

// PolicyAction is an allow/deny switch for one audit policy rule.
//
// +kubebuilder:validation:Enum=Allow;Deny
type PolicyAction string

const (
	PolicyAllow PolicyAction = "Allow"
	PolicyDeny  PolicyAction = "Deny"
)

// FailMode decides what happens to sessions when audit storage cannot
// keep up.
//
// +kubebuilder:validation:Enum=Open;Closed
type FailMode string

const (
	// FailOpen keeps sessions running and records a gap.
	FailOpen FailMode = "Open"
	// FailClosed refuses new channels when the spool is nearly full and
	// disconnects sessions once it is full.
	FailClosed FailMode = "Closed"
)

// AuditSpec configures gateway session audit. The gateway reads it per
// session, so changes apply to new sessions without a restart.
type AuditSpec struct {
	// Enabled turns on session recording at the gateway.
	Enabled bool `json:"enabled"`

	// Notice is prepended to the login banner while audit is enabled.
	// Empty uses the default notice.
	//
	// +optional
	Notice string `json:"notice,omitempty"`

	// Policy is the cluster default audit policy. Users may override
	// individual fields via User.spec.auditPolicy.
	//
	// +optional
	Policy AuditPolicy `json:"policy,omitempty"`
}

// AuditPolicy is a set of optional hard rules enforced by the gateway.
// Every field is optional; an unset field inherits the cluster default,
// and an unset default means Allow (FailMode: Open).
type AuditPolicy struct {
	// Download denies transferring files from the DevPod to the client
	// over SFTP, legacy scp (-f) and ZMODEM (sz).
	//
	// +optional
	Download *PolicyAction `json:"download,omitempty"`

	// Upload denies transferring files from the client into the DevPod
	// over SFTP, legacy scp (-t) and ZMODEM (rz).
	//
	// +optional
	Upload *PolicyAction `json:"upload,omitempty"`

	// MaxFileSize rejects any single recognised file transfer larger
	// than this size.
	//
	// +optional
	MaxFileSize *resource.Quantity `json:"maxFileSize,omitempty"`

	// Forwarding controls TCP port forwarding.
	//
	// +optional
	Forwarding *ForwardingPolicy `json:"forwarding,omitempty"`

	// AgentForwarding controls ssh-agent forwarding.
	//
	// +optional
	AgentForwarding *PolicyAction `json:"agentForwarding,omitempty"`

	// X11Forwarding controls X11 forwarding.
	//
	// +optional
	X11Forwarding *PolicyAction `json:"x11Forwarding,omitempty"`

	// FailMode decides whether sessions continue (Open) or are refused
	// (Closed) when audit storage cannot keep up.
	//
	// +optional
	FailMode *FailMode `json:"failMode,omitempty"`
}

// ForwardingPolicy controls TCP port forwarding.
type ForwardingPolicy struct {
	// Local controls client-requested (direct-tcpip) forwarding. Denying
	// it makes VS Code Remote-SSH unusable.
	//
	// +optional
	Local *PolicyAction `json:"local,omitempty"`

	// Remote controls server-side (tcpip-forward) forwarding.
	//
	// +optional
	Remote *PolicyAction `json:"remote,omitempty"`

	// RecordContent records the raw bytes of forwarded channels. By
	// default only metadata (target, byte counts, duration) is recorded.
	//
	// +optional
	RecordContent *bool `json:"recordContent,omitempty"`
}
