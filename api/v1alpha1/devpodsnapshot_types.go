// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DevPodSnapshotPhase enumerates lifecycle states of a snapshot.
//
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type DevPodSnapshotPhase string

const (
	SnapshotPending   DevPodSnapshotPhase = "Pending"
	SnapshotRunning   DevPodSnapshotPhase = "Running"
	SnapshotSucceeded DevPodSnapshotPhase = "Succeeded"
	SnapshotFailed    DevPodSnapshotPhase = "Failed"
)

// DevPodSnapshotSpec defines the desired snapshot operation. Immutable
// after creation.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type DevPodSnapshotSpec struct {
	// DevPodName is the name of the DevPod to snapshot (same namespace).
	//
	// +kubebuilder:validation:MinLength=1
	DevPodName string `json:"devPodName"`

	// Image is the target OCI image reference including tag.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$`
	Image string `json:"image"`

	// Push controls whether the committed image is pushed to the registry.
	// Defaults to true. Set to false for local-only snapshots.
	//
	// +optional
	// +kubebuilder:default=true
	Push bool `json:"push"`

	// PushSecretRef names a Secret of type kubernetes.io/dockerconfigjson
	// used to authenticate the push. When nil, the Job relies on node-level
	// registry credentials. Ignored when push is false.
	//
	// +optional
	PushSecretRef *LocalObjectRef `json:"pushSecretRef,omitempty"`
}

// JobRef identifies the snapshot Job.
type JobRef struct {
	Name string `json:"name"`
}

// DevPodSnapshotStatus reports observed state.
type DevPodSnapshotStatus struct {
	// +optional
	Phase DevPodSnapshotPhase `json:"phase,omitempty"`

	// Digest is the OCI digest of the pushed image (set on success).
	// +optional
	Digest string `json:"digest,omitempty"`

	// Message is a human-readable explanation (set on failure).
	// +optional
	Message string `json:"message,omitempty"`

	// JobRef references the snapshot Job.
	// +optional
	JobRef *JobRef `json:"jobRef,omitempty"`

	// +optional
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// DevPodSnapshot captures a running DevPod container as an OCI image.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dps
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="DevPod",type=string,JSONPath=`.spec.devPodName`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DevPodSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DevPodSnapshotSpec   `json:"spec,omitempty"`
	Status DevPodSnapshotStatus `json:"status,omitempty"`
}

// DevPodSnapshotList is a list of DevPodSnapshot.
//
// +kubebuilder:object:root=true
type DevPodSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevPodSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DevPodSnapshot{}, &DevPodSnapshotList{})
}
