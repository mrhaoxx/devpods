// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

// A quota's GPU limit is over every kind of GPU together — also one
// written, before there were several kinds, under NVIDIA's resource.
func TestQuotaCountsEveryKindOfGPU(t *testing.T) {
	q := resource.MustParse
	dp := func(limits corev1.ResourceList) devpodv1alpha1.DevPod {
		return devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Running: true, Pod: &devpodv1alpha1.PodWorkloadSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Resources: corev1.ResourceRequirements{Limits: limits}}}}}}}
	}
	has := dp(corev1.ResourceList{"mthreads.com/gpu": q("3")})
	wants := dp(corev1.ResourceList{gpures.NVIDIA: q("1"), "metax-tech.com/gpu": q("1")})
	for name, compute := range map[string]corev1.ResourceList{
		"total":             {gpures.Total: q("4")},
		"written as nvidia": {gpures.NVIDIA: q("4")},
	} {
		err := CheckQuota(devpodv1alpha1.UserQuota{Compute: compute}, []devpodv1alpha1.DevPod{has}, &wants)
		if err == nil || len(err.Violations) != 1 || err.Violations[0].Resource != string(gpures.Total) || err.Violations[0].Used != "3" || err.Violations[0].Requested != "2" {
			t.Fatalf("%s: 3 Moore Threads + 1 NVIDIA + 1 MetaX against 4: %+v", name, err)
		}
	}
	if err := CheckQuota(devpodv1alpha1.UserQuota{Compute: corev1.ResourceList{gpures.Total: q("5")}}, []devpodv1alpha1.DevPod{has}, &wants); err != nil {
		t.Fatalf("5 allows 3+2: %+v", err)
	}
	// with a total beside it, NVIDIA's own resource limits NVIDIA's cards alone
	both := corev1.ResourceList{gpures.Total: q("8"), gpures.NVIDIA: q("0")}
	if err := CheckQuota(devpodv1alpha1.UserQuota{Compute: both}, nil, &wants); err == nil || err.Violations[0].Resource != string(gpures.NVIDIA) {
		t.Fatalf("no NVIDIA cards allowed: %+v", err)
	}
}
