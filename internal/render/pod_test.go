// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func minimalDevPod() *devpodv1alpha1.DevPod {
	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-dev", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "alice",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "dev",
						Image: "ghcr.io/example/devbox:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}},
				},
			},
		},
	}
}

func cfg() *devpodv1alpha1.GatewayConfig {
	return &devpodv1alpha1.GatewayConfig{
		Spec: devpodv1alpha1.GatewayConfigSpec{
			DevPodNamespace: "devpods",
			SupervisorImage: "ghcr.io/example/devpod-supervisor:v0.1.0",
			BackendPort:     2222,
		},
	}
}

func TestRenderPod_HasBootstrapInitContainer(t *testing.T) {
	pod, err := render.Pod(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if got := len(pod.Spec.InitContainers); got != 1 {
		t.Fatalf("init containers = %d, want 1", got)
	}
	ic := pod.Spec.InitContainers[0]
	if ic.Name != render.SupervisorBootstrapContainerName {
		t.Errorf("init container name = %q, want %q", ic.Name, render.SupervisorBootstrapContainerName)
	}
	if ic.Image != "ghcr.io/example/devpod-supervisor:v0.1.0" {
		t.Errorf("init container image = %q", ic.Image)
	}
	if len(ic.Command) != 2 || ic.Command[0] != "/opt/devpod/devpod-supervisor" || ic.Command[1] != "bootstrap" {
		t.Errorf("init container command = %v, want [/opt/devpod/devpod-supervisor bootstrap]", ic.Command)
	}
	var binMounted bool
	for _, m := range ic.VolumeMounts {
		if m.Name == render.VolumeSupervisorBin && m.MountPath == "/devpod-bin" {
			binMounted = true
		}
	}
	if !binMounted {
		t.Errorf("init container missing devpod-bin volumeMount at /devpod-bin: %v", ic.VolumeMounts)
	}
}

func TestRenderPod_TargetContainerWrappedWithSupervisor(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Pod.Spec.Containers[0].Command = []string{"sleep", "infinity"}
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	c := pod.Spec.Containers[0]
	if len(c.Command) != 1 || c.Command[0] != "/opt/devpod/devpod-supervisor" {
		t.Errorf("command = %v, want [/opt/devpod/devpod-supervisor]", c.Command)
	}
	if len(c.Args) != 2 || c.Args[0] != "sleep" || c.Args[1] != "infinity" {
		t.Errorf("args = %v, want [sleep infinity]", c.Args)
	}
}

func TestRenderPod_EmptyUserCommand_OnlySshd(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Pod.Spec.Containers[0].Command = nil
	dp.Spec.Pod.Spec.Containers[0].Args = nil
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if len(pod.Spec.Containers[0].Args) != 0 {
		t.Errorf("expected empty args, got %v", pod.Spec.Containers[0].Args)
	}
}

func TestRenderPod_NoShareProcessNamespace(t *testing.T) {
	pod, err := render.Pod(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
		t.Errorf("ShareProcessNamespace must not be true in v2; got %#v", pod.Spec.ShareProcessNamespace)
	}
}

func TestRenderPod_BinAndHostVolumes(t *testing.T) {
	pod, err := render.Pod(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	var bin, host bool
	for _, v := range pod.Spec.Volumes {
		switch v.Name {
		case render.VolumeSupervisorBin:
			bin = true
			if v.EmptyDir == nil {
				t.Errorf("devpod-bin volume not EmptyDir: %#v", v)
			}
		case render.VolumeSupervisorHost:
			host = true
			if v.Secret == nil || v.Secret.SecretName != "alice-frontend-dev-hostkey" {
				t.Errorf("devpod-host volume wrong: %#v", v)
			}
		}
	}
	if !bin {
		t.Errorf("devpod-bin volume missing")
	}
	if !host {
		t.Errorf("devpod-host volume missing")
	}
}

func TestRenderPod_TargetMounts(t *testing.T) {
	pod, err := render.Pod(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	target := pod.Spec.Containers[0]
	var bin, host bool
	for _, m := range target.VolumeMounts {
		if m.Name == render.VolumeSupervisorBin && m.MountPath == "/opt/devpod" && m.ReadOnly {
			bin = true
		}
		if m.Name == render.VolumeSupervisorHost && m.MountPath == "/etc/devpod" && m.ReadOnly {
			host = true
		}
	}
	if !bin || !host {
		t.Errorf("target container missing supervisor mounts: bin=%v host=%v mounts=%v", bin, host, target.VolumeMounts)
	}
}

func TestRenderPod_MultipleContainers_OnlyTargetWrapped(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Pod.Spec.Containers = append(dp.Spec.Pod.Spec.Containers, corev1.Container{
		Name:    "companion",
		Image:   "busybox",
		Command: []string{"sleep", "infinity"},
	})
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if got := pod.Spec.Containers[0].Command[0]; got != "/opt/devpod/devpod-supervisor" {
		t.Errorf("target container[0] not wrapped: %v", got)
	}
	if got := pod.Spec.Containers[1].Command; len(got) != 2 || got[0] != "sleep" {
		t.Errorf("companion should be untouched, got command=%v", got)
	}
}

func TestRenderPod_HomeVolume_OnlyWhenPersistence(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      resource.MustParse("10Gi"),
		MountPath: "/workspace",
	}
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	var found bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == render.VolumeHome {
			found = true
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "alice-frontend-dev-home" {
				t.Errorf("home volume PVC ref wrong: %#v", v)
			}
		}
	}
	if !found {
		t.Errorf("home volume missing when persistence enabled")
	}

	dpNo := minimalDevPod()
	pod2, err := render.Pod(dpNo, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	for _, v := range pod2.Spec.Volumes {
		if v.Name == render.VolumeHome {
			t.Errorf("home volume present without persistence")
		}
	}
}

func TestRenderPod_PersistenceMountsOnTargetContainer(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      resource.MustParse("1Gi"),
		MountPath: "/workspace",
	}
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	target := pod.Spec.Containers[0] // default target = first container
	var matches []corev1.VolumeMount
	for _, m := range target.VolumeMounts {
		if m.Name == render.VolumeHome {
			matches = append(matches, m)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one devpod-home mount on target container, got %d", len(matches))
	}
	if matches[0].MountPath != "/workspace" {
		t.Errorf("home mountPath = %q, want /workspace", matches[0].MountPath)
	}
}

func TestRenderPod_PersistenceTargetContainerExplicit(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Pod.Spec.Containers = append(dp.Spec.Pod.Spec.Containers, corev1.Container{
		Name:  "companion",
		Image: "busybox",
	})
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:            resource.MustParse("1Gi"),
		MountPath:       "/data",
		TargetContainer: "companion",
	}
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	var devMounted, companionMounted bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == render.VolumeHome {
			devMounted = true
		}
	}
	for _, m := range pod.Spec.Containers[1].VolumeMounts {
		if m.Name == render.VolumeHome && m.MountPath == "/data" {
			companionMounted = true
		}
	}
	if devMounted {
		t.Errorf("home mounted on dev container; should only be on companion")
	}
	if !companionMounted {
		t.Errorf("home not mounted on companion at /data")
	}
}

func TestRenderPod_PersistenceUnknownTargetContainerError(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:            resource.MustParse("1Gi"),
		MountPath:       "/data",
		TargetContainer: "does-not-exist",
	}
	if _, err := render.Pod(dp, cfg()); err == nil {
		t.Fatal("expected error on unknown targetContainer")
	}
}

func TestRenderPod_PreservesUserVolumes(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Pod.Spec.Volumes = []corev1.Volume{{
		Name:         "user-cache",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	var found bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == "user-cache" {
			found = true
		}
	}
	if !found {
		t.Errorf("user volume dropped")
	}
}

func TestRenderPod_Labels(t *testing.T) {
	pod, err := render.Pod(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if pod.Labels[render.LabelOwner] != "alice" {
		t.Errorf("missing owner label: %v", pod.Labels)
	}
	if pod.Labels[render.LabelDevPod] != "frontend-dev" {
		t.Errorf("missing devpod label: %v", pod.Labels)
	}
}

func TestRenderPod_InjectsDevpodShellEnvWhenSet(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Shell = "fish"
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	target := pod.Spec.Containers[0]
	var seen string
	for _, e := range target.Env {
		if e.Name == "DEVPOD_SHELL" {
			seen = e.Value
		}
	}
	if seen != "fish" {
		t.Errorf("DEVPOD_SHELL env = %q, want %q", seen, "fish")
	}
}

func TestRenderPod_OmitsDevpodShellEnvWhenUnset(t *testing.T) {
	dp := minimalDevPod() // Shell defaults to ""
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	target := pod.Spec.Containers[0]
	for _, e := range target.Env {
		if e.Name == "DEVPOD_SHELL" {
			t.Errorf("DEVPOD_SHELL env unexpectedly set to %q", e.Value)
		}
	}
}

func TestRenderPod_SupervisorBinVolumeHasSizeLimit(t *testing.T) {
	pod, err := render.Pod(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name != render.VolumeSupervisorBin {
			continue
		}
		if v.EmptyDir == nil {
			t.Fatalf("volume %q has no EmptyDir source", v.Name)
		}
		if v.EmptyDir.SizeLimit == nil {
			t.Errorf("emptyDir.sizeLimit unset; want 100Mi")
			return
		}
		want := resource.MustParse("100Mi")
		if v.EmptyDir.SizeLimit.Cmp(want) != 0 {
			t.Errorf("emptyDir.sizeLimit = %s, want %s",
				v.EmptyDir.SizeLimit.String(), want.String())
		}
		return
	}
	t.Fatalf("volume %q not found", render.VolumeSupervisorBin)
}
