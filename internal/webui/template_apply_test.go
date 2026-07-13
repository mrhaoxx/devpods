// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func pinTemplate() *devpodv1alpha1.DevPodTemplate {
	return &devpodv1alpha1.DevPodTemplate{
		Spec: devpodv1alpha1.DevPodTemplateSpec{
			DisplayName: "Pinned 8C",
			Binding: &devpodv1alpha1.BindingSpec{
				Annotations: map[string]string{
					"kore.zjusct.io/pin":         "true",
					"kore.zjusct.io/numa-policy": "single",
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("8"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
				},
			},
		},
	}
}

func TestApplyBindingOverlay(t *testing.T) {
	dp := mkDevPod(true, "2", "4Gi", "")
	if err := webui.ApplyTemplate(&dp, pinTemplate()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ann := dp.Spec.Pod.Metadata.Annotations
	if ann["kore.zjusct.io/pin"] != "true" || ann["kore.zjusct.io/numa-policy"] != "single" {
		t.Fatalf("annotations not stamped: %v", ann)
	}
	res := dp.Spec.Pod.Spec.Containers[0].Resources
	if res.Limits.Cpu().String() != "8" || res.Requests.Cpu().String() != "8" {
		t.Fatalf("resources not overridden: %+v", res)
	}
	// Memory limit overridden by the binding too.
	if res.Limits.Memory().String() != "16Gi" {
		t.Fatalf("memory = %s", res.Limits.Memory())
	}
}

func TestApplyBindingNeedsPod(t *testing.T) {
	dp := devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Owner: "gl-alice"}}
	if err := webui.ApplyTemplate(&dp, pinTemplate()); err == nil {
		t.Fatal("expected error: binding overlay onto DevPod without pod spec")
	}
}

func TestApplyPreset(t *testing.T) {
	tpl := pinTemplate()
	tpl.Spec.PodPreset = &devpodv1alpha1.PodPresetSpec{
		Image: "ghcr.io/example/cuda-dev:12",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
		Persistence: &devpodv1alpha1.PersistenceSpec{Size: resource.MustParse("20Gi"), MountPath: "/home/dev"},
		Shell:       "zsh",
	}
	dp := devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Owner: "gl-alice", Running: true}}
	if err := webui.ApplyTemplate(&dp, tpl); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if dp.Spec.Pod == nil || dp.Spec.Pod.Spec.Containers[0].Image != "ghcr.io/example/cuda-dev:12" {
		t.Fatalf("preset pod not built: %+v", dp.Spec.Pod)
	}
	if dp.Spec.Shell != "zsh" || dp.Spec.Persistence == nil {
		t.Fatalf("preset defaults not applied: shell=%q persistence=%v", dp.Spec.Shell, dp.Spec.Persistence)
	}
	// Binding on the same template overrides the preset's cpu/memory.
	if dp.Spec.Pod.Spec.Containers[0].Resources.Limits.Cpu().String() != "8" {
		t.Fatalf("binding should override preset cpu")
	}
}

func TestApplyPresetKeepsUserFields(t *testing.T) {
	tpl := &devpodv1alpha1.DevPodTemplate{Spec: devpodv1alpha1.DevPodTemplateSpec{
		DisplayName: "plain",
		PodPreset:   &devpodv1alpha1.PodPresetSpec{Image: "ubuntu:24.04", Shell: "bash"},
	}}
	dp := devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Owner: "gl-alice", Shell: "fish"}}
	if err := webui.ApplyTemplate(&dp, tpl); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if dp.Spec.Shell != "fish" {
		t.Fatalf("user-set shell must win over preset default, got %q", dp.Spec.Shell)
	}
}

func TestApplyInvalidBindingRejected(t *testing.T) {
	tpl := pinTemplate()
	tpl.Spec.Binding.Annotations["kore.zjusct.io/cpuset"] = "0-7"
	dp := mkDevPod(true, "2", "4Gi", "")
	if err := webui.ApplyTemplate(&dp, tpl); err == nil {
		t.Fatal("expected invalid binding to be rejected at apply time")
	}
}
