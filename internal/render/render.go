// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package render builds Kubernetes API objects from DevPod CRDs.
//
// All functions in this package are pure: same input → same output, no I/O,
// no clock reads, no client calls. This makes them table-testable and lets
// the controllers be thin orchestrators on top.
package render

import (
	"hash/fnv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Label keys used across rendered objects.
const (
	LabelOwner   = "devpod.io/owner"
	LabelDevPod  = "devpod.io/devpod"
	LabelManaged = "app.kubernetes.io/managed-by"
)

// Names reserved by the v2 render injection. User PodSpecs may not
// use these container or volume names — the webhook enforces the
// "devpod-" prefix rule on volumes/mounts; the bootstrap container
// name is implicitly reserved by virtue of being prepended to
// initContainers.
const (
	SupervisorBootstrapContainerName = "devpod-bootstrap"
	VolumeSupervisorBin              = "devpod-bin"
	VolumeSupervisorHost             = "devpod-host"
	VolumeHome                       = "devpod-home"
)

// OwnerLabels returns labels every DevPod-owned object carries.
func OwnerLabels(owner string) map[string]string {
	return map[string]string{
		LabelOwner:   owner,
		LabelManaged: "devpod-controller",
	}
}

// DevPodLabels returns labels every object created for a specific DevPod carries.
func DevPodLabels(dp *devpodv1alpha1.DevPod) map[string]string {
	out := OwnerLabels(dp.Spec.Owner)
	out[LabelDevPod] = dp.Name
	return out
}

// PodName returns the deterministic name for a DevPod's rendered Pod.
//
// We prefix with owner so kubectl get pods is grouped by user even though
// every DevPod shares the devpods namespace.
//
func PodName(dp *devpodv1alpha1.DevPod) string {
	return dp.Name
}

// ServiceName mirrors PodName for the headless Service.
func ServiceName(dp *devpodv1alpha1.DevPod) string { return PodName(dp) }

// HostKeySecretName names the per-DevPod sshd host key Secret.
func HostKeySecretName(dp *devpodv1alpha1.DevPod) string {
	return PodName(dp) + "-hostkey"
}

// HomePVCName names the PVC backing the home directory when persistence
// is enabled.
func HomePVCName(dp *devpodv1alpha1.DevPod) string {
	return PodName(dp) + "-home"
}

// OwnerNetPolName names the per-owner allow NetworkPolicy.
func OwnerNetPolName(owner string) string {
	return "devpod-allow-" + owner
}

// BackendPort returns the TCP port the in-container sshd should
// listen on for this DevPod.
//
// When the user's pod uses hostNetwork, multiple DevPod pods on the
// same node would collide on a shared port, so the port is derived
// deterministically from the DevPod's UID (stable across container
// restarts) and falls in [65000, 65500). On UID-less objects
// (rendered before kube-apiserver assigned a UID, e.g. unit tests)
// the configured default applies.
//
// Otherwise returns cfg.Spec.BackendPort (default 2222).
func BackendPort(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) int32 {
	def := cfg.Spec.BackendPort
	if def == 0 {
		def = 2222
	}
	if dp.Spec.Pod == nil || !dp.Spec.Pod.Spec.HostNetwork {
		return def
	}
	if dp.UID == "" {
		return def
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(dp.UID))
	return 65000 + int32(h.Sum32()%500)
}

// ObjectMeta produces the standard metav1.ObjectMeta the controller
// stamps onto every rendered object. OwnerReferences are deliberately
// not set here; callers must invoke
// controllerutil.SetControllerReference(dp, obj, scheme) after
// construction so the runtime.Scheme can supply the correct GVK.
func ObjectMeta(name, namespace string, dp *devpodv1alpha1.DevPod) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Labels:    DevPodLabels(dp),
	}
}
