// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func mkDevPod(running bool, cpuLim, memLim, storage string) devpodv1alpha1.DevPod {
	dp := devpodv1alpha1.DevPod{
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "gl-alice",
			Running: running,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "dev",
						Image: "ubuntu:24.04",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(cpuLim),
								corev1.ResourceMemory: resource.MustParse(memLim),
							},
						},
					}},
				},
			},
		},
	}
	if storage != "" {
		dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
			Size:      resource.MustParse(storage),
			MountPath: "/home/dev",
		}
	}
	return dp
}

func quota(maxPods int32, cpu, mem, storage string) devpodv1alpha1.UserQuota {
	q := devpodv1alpha1.UserQuota{MaxDevPods: ptr.To(maxPods), Compute: corev1.ResourceList{}}
	if cpu != "" {
		q.Compute[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		q.Compute[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	if storage != "" {
		s := resource.MustParse(storage)
		q.Storage = &s
	}
	return q
}

func TestCheckQuotaWithinLimits(t *testing.T) {
	existing := []devpodv1alpha1.DevPod{mkDevPod(true, "4", "8Gi", "20Gi")}
	proposed := mkDevPod(true, "4", "8Gi", "20Gi")
	if err := webui.CheckQuota(quota(3, "8", "16Gi", "50Gi"), existing, &proposed); err != nil {
		t.Fatalf("unexpected violation: %v", err)
	}
}

func TestCheckQuotaComputeExceeded(t *testing.T) {
	existing := []devpodv1alpha1.DevPod{mkDevPod(true, "6", "8Gi", "")}
	proposed := mkDevPod(true, "4", "4Gi", "")
	err := webui.CheckQuota(quota(10, "8", "16Gi", ""), existing, &proposed)
	if err == nil {
		t.Fatal("expected cpu violation")
	}
	if len(err.Violations) != 1 || err.Violations[0].Resource != "cpu" {
		t.Fatalf("violations = %+v", err.Violations)
	}
	v := err.Violations[0]
	if v.Requested != "4" || v.Used != "6" || v.Limit != "8" {
		t.Fatalf("triple = %+v", v)
	}
}

func TestCheckQuotaHibernatedComputeFree(t *testing.T) {
	// A hibernated 6-cpu pod does not count toward compute.
	existing := []devpodv1alpha1.DevPod{mkDevPod(false, "6", "8Gi", "")}
	proposed := mkDevPod(true, "4", "4Gi", "")
	if err := webui.CheckQuota(quota(10, "8", "16Gi", ""), existing, &proposed); err != nil {
		t.Fatalf("unexpected violation: %v", err)
	}
}

func TestCheckQuotaStorageCountsHibernated(t *testing.T) {
	// Storage counts even for hibernated DevPods.
	existing := []devpodv1alpha1.DevPod{mkDevPod(false, "1", "1Gi", "40Gi")}
	proposed := mkDevPod(true, "1", "1Gi", "20Gi")
	err := webui.CheckQuota(quota(10, "", "", "50Gi"), existing, &proposed)
	if err == nil || err.Violations[0].Resource != "storage" {
		t.Fatalf("expected storage violation, got %v", err)
	}
}

func TestCheckQuotaMaxDevPods(t *testing.T) {
	existing := []devpodv1alpha1.DevPod{mkDevPod(false, "1", "1Gi", ""), mkDevPod(false, "1", "1Gi", "")}
	proposed := mkDevPod(false, "1", "1Gi", "")
	err := webui.CheckQuota(quota(2, "", "", ""), existing, &proposed)
	if err == nil || err.Violations[0].Resource != "devpods" {
		t.Fatalf("expected devpods violation, got %v", err)
	}
}

func TestCheckQuotaInitContainersCount(t *testing.T) {
	proposed := mkDevPod(true, "4", "4Gi", "")
	proposed.Spec.Pod.Spec.InitContainers = []corev1.Container{{
		Name: "init", Image: "busybox",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("5"),
		}},
	}}
	err := webui.CheckQuota(quota(10, "8", "", ""), nil, &proposed)
	if err == nil || err.Violations[0].Resource != "cpu" {
		t.Fatalf("expected cpu violation incl. initContainers, got %v", err)
	}
}

func TestRequireLimits(t *testing.T) {
	ok := mkDevPod(true, "1", "1Gi", "")
	if err := webui.RequireLimits(&ok.Spec.Pod.Spec); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	missing := mkDevPod(true, "1", "1Gi", "")
	missing.Spec.Pod.Spec.Containers[0].Resources.Limits = nil
	if err := webui.RequireLimits(&missing.Spec.Pod.Spec); err == nil {
		t.Fatal("expected error for missing limits")
	}
}

func TestEffectiveQuota(t *testing.T) {
	def := quota(3, "8", "16Gi", "50Gi")
	if got := webui.EffectiveQuota(&devpodv1alpha1.User{}, def); got.Compute.Cpu().String() != "8" {
		t.Fatalf("nil quota should fall back to defaults, got %+v", got)
	}
	u := &devpodv1alpha1.User{Spec: devpodv1alpha1.UserSpec{Quota: &devpodv1alpha1.UserQuota{MaxDevPods: ptr.To(int32(1))}}}
	got := webui.EffectiveQuota(u, def)
	if *got.MaxDevPods != 1 {
		t.Fatalf("explicit maxDevPods should win, got %d", *got.MaxDevPods)
	}
	if got.Compute.Cpu().String() != "8" {
		t.Fatalf("unset compute should fall back, got %s", got.Compute.Cpu())
	}
}
