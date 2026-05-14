// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretRef names a Secret in a specific namespace.
type SecretRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ProxyProtocolSpec configures the PROXY-protocol v2 listener.
//
// +kubebuilder:validation:XValidation:rule="!self.enabled || size(self.trustedCIDRs) > 0",message="trustedCIDRs must be non-empty when proxyProtocol.enabled is true"
type ProxyProtocolSpec struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// TrustedCIDRs lists source CIDRs from which a PROXY header is honored.
	// Required when Enabled is true.
	//
	// +optional
	TrustedCIDRs []string `json:"trustedCIDRs,omitempty"`
}

// ListenSpec configures the gateway listener.
type ListenSpec struct {
	// +kubebuilder:default=22
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// +optional
	ProxyProtocol ProxyProtocolSpec `json:"proxyProtocol,omitempty"`
}

// TrustedProxyKey is an SSH-terminating outer proxy that may impersonate
// users when its pubkey matches.
type TrustedProxyKey struct {
	// +kubebuilder:validation:MinLength=1
	Alias string `json:"alias"`

	// +kubebuilder:validation:MinLength=1
	Pubkey string `json:"pubkey"`
}

// LDAPSpec configures a single external LDAP identity source queried
// after the User CRD source. Disabled when GatewayConfigSpec.LDAP is
// nil.
type LDAPSpec struct {
	// URL is the LDAP server URL. Only ldaps:// is supported
	// (plaintext ldap:// is refused at admission).
	//
	// +kubebuilder:validation:Pattern=`^ldaps://[^/\s]+(:[0-9]+)?$`
	URL string `json:"url"`

	// CASecretRef points to a Secret whose key "ca.crt" holds the
	// PEM-encoded LDAP server CA bundle. The system trust store is
	// never consulted, so a CA rotation here is an explicit,
	// observable event rather than a silent drift.
	CASecretRef SecretRef `json:"caSecretRef"`

	// BindDN is the DN used for simple bind.
	//
	// +kubebuilder:validation:MinLength=1
	BindDN string `json:"bindDN"`

	// BindPasswordSecretRef points to a Secret whose key "password"
	// holds the bind password.
	BindPasswordSecretRef SecretRef `json:"bindPasswordSecretRef"`

	// BaseDN is the search base for user entries.
	//
	// +kubebuilder:validation:MinLength=1
	BaseDN string `json:"baseDN"`

	// UserFilter is a Go text/template for the LDAP search filter,
	// rendered against {.Username string}. Username has already
	// passed the DevPod login-name regex ([a-z0-9-]{1,32}); the
	// gateway additionally RFC 4515-escapes it before substitution
	// as defense-in-depth.
	//
	// Default: `(&(objectClass=posixAccount)(uid={{.Username}}))`
	//
	// +optional
	UserFilter string `json:"userFilter,omitempty"`

	// PubkeyAttribute is the LDAP attribute that carries
	// OpenSSH-format authorized_keys lines. Defaults to
	// "sshPublicKey" (the OpenSSH schema OID
	// 1.3.6.1.4.1.24552.500.1.1.1.13). Multi-valued attributes are
	// supported; each value is parsed independently.
	//
	// +optional
	// +kubebuilder:default=sshPublicKey
	PubkeyAttribute string `json:"pubkeyAttribute,omitempty"`

	// RequestTimeoutSeconds bounds a single LDAP search round-trip.
	//
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	RequestTimeoutSeconds int32 `json:"requestTimeoutSeconds,omitempty"`

	// CacheTTLSeconds is the positive-cache lifetime. Within this
	// window, a successful prior lookup is served without touching
	// LDAP.
	//
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=10
	CacheTTLSeconds int32 `json:"cacheTTLSeconds,omitempty"`

	// NegativeCacheTTLSeconds is the lifetime of a "this user is not
	// in LDAP" cache entry.
	//
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=0
	NegativeCacheTTLSeconds int32 `json:"negativeCacheTTLSeconds,omitempty"`

	// StaleGraceSeconds is the soft-fail window. When LDAP is
	// currently failing, a cache entry whose age is between
	// CacheTTLSeconds and CacheTTLSeconds+StaleGraceSeconds is still
	// served (audit: served_stale=true). Beyond that, the entry is
	// treated as evicted.
	//
	// +optional
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=0
	StaleGraceSeconds int32 `json:"staleGraceSeconds,omitempty"`
}

// HomeDirSpec configures automatic home directory injection from a
// shared host filesystem (NFS, AFS, etc.) mounted on every node.
type HomeDirSpec struct {
	// HostPathPrefix is the base path on the node. The owner name is
	// appended: {hostPathPrefix}/{owner}. Example: /data/afs/home
	//
	// +kubebuilder:validation:MinLength=1
	HostPathPrefix string `json:"hostPathPrefix"`

	// MountPrefix is the base mount path inside the container. The
	// owner name is appended: {mountPrefix}/{owner}.
	// Example: /home → /home/alice
	//
	// +kubebuilder:default=/home
	MountPrefix string `json:"mountPrefix,omitempty"`
}

// GatewayConfigSpec configures the gateway and the cluster-wide DevPod runtime.
type GatewayConfigSpec struct {
	// DevPodNamespace is where every DevPod-owned object lives.
	//
	// +kubebuilder:default=devpods
	// +kubebuilder:validation:MinLength=1
	DevPodNamespace string `json:"devPodNamespace,omitempty"`

	// HostKeyRef points to a Secret holding the gateway's externally
	// presented SSH host key. The Secret must contain
	// `ssh_host_ed25519_key` (PEM-encoded private) and `ssh_host_ed25519_key.pub`.
	HostKeyRef SecretRef `json:"hostKeyRef"`

	// InternalKeyRef points to a Secret holding the gateway's outbound
	// key used when dialing backend sshd. Same format as HostKeyRef.
	InternalKeyRef SecretRef `json:"internalKeyRef"`

	// +optional
	Listen ListenSpec `json:"listen,omitempty"`

	// +optional
	TrustedProxyKeys []TrustedProxyKey `json:"trustedProxyKeys,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=0
	DefaultIdleTimeoutSeconds int32 `json:"defaultIdleTimeoutSeconds,omitempty"`

	// SupervisorImage is the container image holding the init-container
	// payload for each Pod-backed DevPod: static OpenSSH (sshd,
	// sftp-server) and the devpod-supervisor binary. A `cp`-style
	// initContainer copies its contents into a shared emptyDir, after
	// which the user's target container runs the supervisor as PID 1.
	//
	// +kubebuilder:validation:MinLength=1
	SupervisorImage string `json:"supervisorImage"`

	// BackendPort is the TCP port the in-container sshd listens on
	// inside each DevPod Pod. Defaults to 2222 so the user container
	// can bind it without CAP_NET_BIND_SERVICE regardless of UID. The
	// gateway dials this port via DevPod.status.endpoint.
	//
	// +optional
	// +kubebuilder:default=2222
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	BackendPort int32 `json:"backendPort,omitempty"`

	// Banner is the pre-auth SSH banner the gateway sends to every
	// incoming connection. Rendered as a Go text/template against a
	// fixed field set that is available WITHOUT any cluster lookup
	// (banner runs before authentication, so the gateway refuses to
	// trust the login name beyond syntactic parsing):
	//
	//   .Now       time.Time — current server time
	//   .ClientIP  string    — remote peer addr (after PROXY-protocol
	//                          if enabled)
	//   .Login     string    — raw conn.User() (e.g. "alice+smoke")
	//   .User      string    — parsed user part, or "" on bad format
	//   .Pod       string    — parsed pod part, or "" on bad format
	//   .Host      string    — gateway pod hostname
	//
	// Empty string disables the banner. Invalid templates fail fast
	// at gateway startup.
	//
	// +optional
	Banner string `json:"banner,omitempty"`

	// HomeDir, when set, injects a hostPath volume into every
	// Pod-backed DevPod so each owner gets a persistent home
	// directory from the node's shared filesystem.
	//
	// +optional
	HomeDir *HomeDirSpec `json:"homeDir,omitempty"`

	// IsolateNetwork enables tenant network isolation via
	// NetworkPolicy. When true, the controller installs a
	// namespace-wide default-deny policy and per-owner allow
	// policies that restrict egress to DNS + public internet
	// only. When false (default), no egress restrictions are
	// applied — DevPod pods can reach any cluster service.
	//
	// +optional
	IsolateNetwork bool `json:"isolateNetwork,omitempty"`

	// LDAP, when non-nil, registers a secondary identity source
	// queried after the User CRD source. Disabled when nil.
	//
	// +optional
	LDAP *LDAPSpec `json:"ldap,omitempty"`
}

// GatewayConfigStatus reports observed state.
type GatewayConfigStatus struct {
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GatewayConfig is the cluster-singleton runtime configuration for DevPod.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=dgc
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="GatewayConfig must be named 'default'"
type GatewayConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayConfigSpec   `json:"spec,omitempty"`
	Status GatewayConfigStatus `json:"status,omitempty"`
}

// GatewayConfigList is a list of GatewayConfig.
//
// +kubebuilder:object:root=true
type GatewayConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GatewayConfig{}, &GatewayConfigList{})
}
