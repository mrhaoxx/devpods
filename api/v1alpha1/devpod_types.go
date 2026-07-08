// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DevPodPhase enumerates lifecycle states observable on a DevPod.
//
// +kubebuilder:validation:Enum=Pending;Running;Stopped;Failed
type DevPodPhase string

const (
	DevPodPending DevPodPhase = "Pending"
	DevPodRunning DevPodPhase = "Running"
	DevPodStopped DevPodPhase = "Stopped"
	DevPodFailed  DevPodPhase = "Failed"
)

// PersistenceSpec opts a DevPod into a persistent home volume.
type PersistenceSpec struct {
	// Size of the home PVC.
	Size resource.Quantity `json:"size"`

	// StorageClassName for the home PVC. Empty string uses the cluster default.
	//
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// AccessModes for the home PVC. Defaults to [ReadWriteOnce]. The
	// chosen StorageClass must support every mode listed here; otherwise
	// PVC binding fails and the DevPod stays in Pending.
	//
	// +optional
	// +kubebuilder:default={ReadWriteOnce}
	// +listType=set
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// MountPath is where the PVC is mounted inside the target container.
	// Required when persistence is set. Must be absolute and must not
	// collide with any user-supplied volumeMount.mountPath on the target
	// container — the validating webhook enforces this.
	//
	// +kubebuilder:validation:Pattern=`^/[^\s]*$`
	MountPath string `json:"mountPath"`

	// TargetContainer names which container in spec.pod.spec.containers
	// receives the home mount. Defaults to
	// spec.pod.spec.containers[0].name. Must reference an existing
	// container; the webhook enforces this.
	//
	// +optional
	TargetContainer string `json:"targetContainer,omitempty"`
}

// PodMetadata is the whitelisted metadata that may be set on the
// rendered Pod. Using a small struct instead of metav1.ObjectMeta
// because controller-gen strips the schema of an embedded ObjectMeta
// to an empty object, causing strict field validation (k8s 1.25+) to
// reject labels/annotations as unknown fields.
type PodMetadata struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PodWorkloadSpec carries a passthrough PodTemplateSpec. The controller
// renders a Pod from this, overlaying the supervisor and sshd entry point.
type PodWorkloadSpec struct {
	// +optional
	Metadata PodMetadata `json:"metadata,omitempty"`

	Spec corev1.PodSpec `json:"spec"`
}

// VMWorkloadSpec carries a passthrough KubeVirt VirtualMachine.spec. The
// controller renders a VirtualMachine from this, overlaying a cloud-init
// disk with the gateway pubkey.
//
// The body is held as a RawExtension so this API package does not have a
// hard build-time dependency on the KubeVirt types. The controller decodes
// it on demand and rejects when KubeVirt is not present in the cluster.
type VMWorkloadSpec struct {
	// +kubebuilder:pruning:PreserveUnknownFields
	Raw runtime.RawExtension `json:",inline"`
}

// DevPodSpec defines the desired state of a DevPod.
//
// Exactly one of Pod or VM must be set.
//
// The single XValidation below replaces the cheap branch of what
// used to be a dedicated validating-admission webhook. Iteration-
// based rules (devpod- name prefix, mountPath collision, target
// container existence) were dropped because the apiserver's CEL
// cost-estimator forbids unbounded list traversals on
// corev1.PodSpec.containers/volumes. The render layer enforces
// those invariants at apply time with hard errors, so a malformed
// DevPod fails fast on the operator's first reconcile rather than
// at admission, which is an acceptable downgrade.
//
// +kubebuilder:validation:XValidation:rule="!has(self.pod) || size(self.pod.spec.containers) > 0",message="spec.pod.spec.containers must contain at least one container"
type DevPodSpec struct {
	// Owner names the User that owns this DevPod. Immutable.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="owner is immutable"
	Owner string `json:"owner"`

	// Collaborators lists additional Users (by name) who may SSH into
	// this DevPod. The owner is always implicitly authorized; collaborators
	// only gain SSH access — they cannot mutate spec (the webhook will
	// enforce this in a follow-up plan).
	//
	// +optional
	// +listType=set
	Collaborators []string `json:"collaborators,omitempty"`

	// Running governs the workload lifecycle. true creates / preserves the
	// workload; false hibernates (deletes Pod/VM, preserves PVC).
	//
	// +kubebuilder:default=true
	Running bool `json:"running"`

	// IdleTimeoutSeconds enables auto-hibernation. 0 disables.
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`

	// Persistence opts in to a per-DevPod home volume.
	//
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// ExitOnUserCommandExit controls the in-container supervisor's
	// behavior when the user's main command process exits. Default
	// false: the supervisor keeps the in-container sshd alive so an
	// operator can ssh in to debug — `kubectl get pod` continues to
	// show Running. Set true for service-oriented workloads where
	// the user command IS the application and its death should
	// trigger a kubelet-driven container restart.
	//
	// Only meaningful when spec.pod.spec.containers[<target>] has a
	// command/args (otherwise the supervisor runs sshd alone and
	// this flag is moot).
	//
	// +optional
	ExitOnUserCommandExit bool `json:"exitOnUserCommandExit,omitempty"`

	// Shell selects the interactive login shell sshd exec's inside the
	// user container. When empty, the supervisor falls back to the user
	// image's /etc/passwd shell if it exists and is executable; if not
	// (typical for distroless images), it falls back to /opt/devpod/bin/bash.
	// The named shells are provided by the supervisor bundle at
	// /opt/devpod/bin/.
	//
	// +optional
	// +kubebuilder:validation:Enum=bash;zsh;fish
	Shell string `json:"shell,omitempty"`

	// Pod, if set, materializes the workload as a Pod.
	//
	// +optional
	Pod *PodWorkloadSpec `json:"pod,omitempty"`

	// VM, if set, materializes the workload as a KubeVirt VirtualMachine.
	//
	// +optional
	VM *VMWorkloadSpec `json:"vm,omitempty"`
}

// WorkloadRef points to the resource currently materializing this DevPod.
type WorkloadRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// LocalObjectRef names an object in the same namespace.
type LocalObjectRef struct {
	Name string `json:"name"`
}

// DevPodStatus reports observed state.
type DevPodStatus struct {
	// +optional
	Phase DevPodPhase `json:"phase,omitempty"`

	// Endpoint is the in-cluster address (ip:port) at which the gateway
	// should dial backend sshd.
	//
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// WorkloadRef points to the Pod or VirtualMachine the controller created.
	//
	// +optional
	WorkloadRef *WorkloadRef `json:"workloadRef,omitempty"`

	// PersistentVolumeClaimRef is set when Persistence is enabled.
	//
	// +optional
	PersistentVolumeClaimRef *LocalObjectRef `json:"persistentVolumeClaimRef,omitempty"`

	// LastActivityTime is patched by the gateway on every session
	// open / close / heartbeat. The controller reads it to drive
	// idle hibernation.
	//
	// +optional
	LastActivityTime *metav1.Time `json:"lastActivityTime,omitempty"`

	// HibernatedAt records when Running last transitioned to false.
	//
	// +optional
	HibernatedAt *metav1.Time `json:"hibernatedAt,omitempty"`

	// +optional
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// DevPod is the schema for development environments.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="(has(self.spec.pod) ? 1 : 0) + (has(self.spec.vm) ? 1 : 0) == 1",message="exactly one of spec.pod or spec.vm must be set"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 22",message="DevPod name must be at most 22 characters (length budget for derived Pod/PVC/Service names)"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.startsWith(self.spec.owner + '-')",message="DevPod name must start with '<owner>-'"
type DevPod struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DevPodSpec   `json:"spec,omitempty"`
	Status DevPodStatus `json:"status,omitempty"`
}

// DevPodList is a list of DevPod.
//
// +kubebuilder:object:root=true
type DevPodList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevPod `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DevPod{}, &DevPodList{})
}
