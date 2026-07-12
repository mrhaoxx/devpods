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

func TestKoreAnnotationKeys(t *testing.T) {
	got := webui.KoreAnnotationKeys(map[string]string{
		"kore.zjusct.io/pin":  "true",
		"example.com/other":   "x",
		"kore.zjusct.io/pool": "team",
	})
	if len(got) != 2 || got[0] != "kore.zjusct.io/pin" || got[1] != "kore.zjusct.io/pool" {
		t.Fatalf("got %v", got)
	}
	if got := webui.KoreAnnotationKeys(map[string]string{"a": "b"}); len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
	if got := webui.KoreAnnotationKeys(nil); len(got) != 0 {
		t.Fatalf("want empty for nil, got %v", got)
	}
}

func rr(cpuReq, cpuLim string) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if cpuReq != "" {
		out.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if cpuLim != "" {
		out.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
	}
	return out
}

func TestValidateBinding(t *testing.T) {
	pin := func(ann map[string]string, res corev1.ResourceRequirements) *devpodv1alpha1.BindingSpec {
		return &devpodv1alpha1.BindingSpec{Annotations: ann, Resources: res}
	}
	cases := []struct {
		name    string
		b       *devpodv1alpha1.BindingSpec
		wantErr bool
	}{
		{"pin integer ok", pin(map[string]string{"kore.zjusct.io/pin": "true"}, rr("8", "8")), false},
		{"pin fractional cpu", pin(map[string]string{"kore.zjusct.io/pin": "true"}, rr("1500m", "1500m")), true},
		{"pin requests != limits", pin(map[string]string{"kore.zjusct.io/pin": "true"}, rr("4", "8")), true},
		{"pin missing cpu limit", pin(map[string]string{"kore.zjusct.io/pin": "true"}, corev1.ResourceRequirements{}), true},
		{"pool ok", pin(map[string]string{"kore.zjusct.io/pool": "team-hpl", "kore.zjusct.io/pool-size": "64"}, rr("2", "32")), false},
		{"pool missing size", pin(map[string]string{"kore.zjusct.io/pool": "team-hpl"}, rr("2", "32")), true},
		{"pool bad size", pin(map[string]string{"kore.zjusct.io/pool": "t", "kore.zjusct.io/pool-size": "zero"}, rr("2", "4")), true},
		{"pin and pool exclusive", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/pool": "t", "kore.zjusct.io/pool-size": "4"}, rr("4", "4")), true},
		{"neither pin nor pool", pin(map[string]string{"kore.zjusct.io/numa-policy": "single"}, rr("4", "4")), true},
		{"cpuset forbidden in templates", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/cpuset": "8-15"}, rr("8", "8")), true},
		{"unknown kore key", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/bogus": "x"}, rr("8", "8")), true},
		{"non-kore key forbidden", pin(map[string]string{"kore.zjusct.io/pin": "true", "example.com/x": "y"}, rr("8", "8")), true},
		{"bad numa policy value", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/numa-policy": "both"}, rr("8", "8")), true},
		{"good policies", pin(map[string]string{
			"kore.zjusct.io/pin":           "true",
			"kore.zjusct.io/numa-policy":   "preferred",
			"kore.zjusct.io/memory-policy": "strict",
			"kore.zjusct.io/placement":     "scatter",
			"kore.zjusct.io/smt-policy":    "logical",
		}, rr("8", "8")), false},
		{"nil binding", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := webui.ValidateBinding(tc.b)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
