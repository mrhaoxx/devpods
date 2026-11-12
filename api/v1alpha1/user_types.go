// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UserQuota caps aggregate resources across a User's DevPods. All
// fields are optional; absent fields fall back to the webui's global
// defaults. Enforced by the webui backend only — the controller and
// gateway ignore it entirely (UI-layer policy, not a security
// barrier; see the webui design spec §9).
type UserQuota struct {
	// MaxDevPods limits how many DevPod CRs the user may own.
	//
	// +optional
	MaxDevPods *int32 `json:"maxDevPods,omitempty"`

	// Compute caps the SUM of container resource limits (containers +
	// initContainers) across the user's *running* DevPods. Keys: cpu,
	// memory, and extended resources such as nvidia.com/gpu.
	//
	// +optional
	Compute corev1.ResourceList `json:"compute,omitempty"`

	// Storage caps the SUM of spec.persistence.size across ALL of the
	// user's DevPods, hibernated included (PVCs survive hibernation).
	//
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
}

// APIToken is a personal access token for the REST API. The token string
// (dpt_<id>_<secret>) is shown once at creation; only the SHA-256 of the
// secret is stored. Written only by the webui.
type APIToken struct {
	// ID is the public part of the token; it names the token in the UI.
	//
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`

	// Name is the user's label for it.
	//
	// +optional
	Name string `json:"name,omitempty"`

	// Hash is the hex SHA-256 of the token's secret part.
	Hash string `json:"hash"`

	// Scopes lists what the token may do (e.g. "devpods:read",
	// "devpods:power"); anything not listed is refused.
	//
	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// DevPods restricts the token to these DevPods by name. Empty = every
	// DevPod its owner can reach.
	//
	// +optional
	DevPods []string `json:"devpods,omitempty"`

	// Clusters restricts the token to these clusters, in a console of
	// several. Empty = every cluster.
	//
	// +optional
	Clusters []string `json:"clusters,omitempty"`

	CreatedAt metav1.Time `json:"createdAt"`

	// ExpiresAt ends the token's validity. nil = never.
	//
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// RegistryCredentialsSecretPrefix + <user> names the Secret (in the
// DevPod namespace) holding a user's registry credentials.
const (
	RegistryCredentialsSecretPrefix = "devpod-registry-"
	RegistryCredentialsLabel        = "devpod.io/registry-credentials"
	OwnerLabel                      = "devpod.io/owner"
)

// GatewayKeysConfigMap is the ConfigMap (DevPod namespace) in which a
// cluster's agent publishes the internal keys of the gateways that serve
// the cluster (key GatewayKeysKey, authorized_keys lines); the cluster's
// DevPods trust them besides the local gateway's.
const (
	GatewayKeysConfigMap = "devpod-gateway-keys"
	GatewayKeysKey       = "authorized_keys"
)

// RegistryCredentialsSecretName is the user's registry credentials Secret.
func RegistryCredentialsSecretName(user string) string { return RegistryCredentialsSecretPrefix + user }

// UserRegistry is a registry the user saved credentials for (no password).
type UserRegistry struct {
	// Host is registry[:port], lowercase.
	//
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// +optional
	Username string `json:"username,omitempty"`
}

// UserRegistryAuth carries a credential that is not in the Secret yet.
// The user controller merges it into the user's registry Secret and
// clears it: the plaintext exists for one reconcile.
type UserRegistryAuth struct {
	// +kubebuilder:validation:MinLength=1
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// UserSpec defines the desired state of a DevPod user.
type UserSpec struct {
	// Pubkeys is the list of OpenSSH-format authorized public keys for
	// this user. May be empty: the webui auto-provisions keyless Users
	// on first OAuth login, and the gateway falls back to LDAP (or
	// denies) when no key matches.
	//
	// +optional
	Pubkeys []string `json:"pubkeys,omitempty"`

	// OIDCSubject is reserved for a future OIDC binding. The v1alpha1
	// controller does nothing with this value.
	//
	// +optional
	OIDCSubject string `json:"oidcSubject,omitempty"`

	// DisplayName is a cosmetic label for UIs.
	//
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Quota caps this user's aggregate DevPod resources. nil = webui
	// global defaults.
	//
	// +optional
	Quota *UserQuota `json:"quota,omitempty"`

	// Quotas are this user's caps in single clusters, by cluster name, in
	// a console of several. A field unset here falls back to Quota, then
	// to the cluster's default, then to the console's.
	//
	// +optional
	Quotas map[string]UserQuota `json:"quotas,omitempty"`

	// PasswordHash is a bcrypt hash of the user's password. Set = this
	// user may log in to the web UI with a password. Never returned by
	// the API. Written only by the webui (admin create/reset, or the
	// user's own change). The controller and gateway ignore it.
	//
	// +optional
	PasswordHash string `json:"passwordHash,omitempty"`

	// Admin grants web UI admin rights on any login path. Written ONLY
	// by an operator via kubectl — the webui treats it as read-only.
	// Effective admin = the webui's --admins allowlist OR this field.
	//
	// +optional
	Admin bool `json:"admin,omitempty"`

	// APITokens are the user's personal access tokens for the REST API.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=50
	APITokens []APIToken `json:"apiTokens,omitempty"`

	// Registries are the image registries the user saved credentials
	// for: used to pull private images into their DevPods and to push
	// snapshots. The credentials live in the Secret
	// devpod-registry-<user>, written only by the user controller.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Registries []UserRegistry `json:"registries,omitempty"`

	// RegistryAuth holds credentials on their way into the Secret; the
	// user controller clears it.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=20
	RegistryAuth []UserRegistryAuth `json:"registryAuth,omitempty"`

	// AuditPolicy overrides fields of the cluster default audit policy
	// (GatewayConfig.spec.audit.policy) for sessions this user starts.
	// nil = cluster default.
	//
	// +optional
	AuditPolicy *AuditPolicy `json:"auditPolicy,omitempty"`
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
