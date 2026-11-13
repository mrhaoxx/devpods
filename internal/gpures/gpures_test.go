// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gpures

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCountAndVendor(t *testing.T) {
	q := resource.MustParse
	l := corev1.ResourceList{corev1.ResourceCPU: q("8"), "mthreads.com/gpu": q("8"), "metax-tech.com/gpu": q("2"), "nvidia.com/mig-1g.10gb": q("4")}
	if n := Count(l); n != 10 {
		t.Fatalf("count %d, want 10 (every vendor's whole cards, nothing else)", n)
	}
	if v := Vendor(l); v != "Moore Threads" {
		t.Fatalf("vendor %q", v)
	}
	if Count(corev1.ResourceList{}) != 0 || Vendor(corev1.ResourceList{corev1.ResourceCPU: q("1")}) != "" {
		t.Fatal("no cards: 0 and no vendor")
	}
}

func TestSet(t *testing.T) {
	old := Types()
	defer func() { types = old }()
	if err := Set("vendor.example/gpu=Example, nvidia.com/gpu"); err != nil {
		t.Fatal(err)
	}
	if ts := Types(); len(ts) != 2 || ts[0].Label != "Example" || ts[1].Label != "nvidia.com" {
		t.Fatalf("types %+v", ts)
	}
	q := resource.MustParse
	if n := Count(corev1.ResourceList{"vendor.example/gpu": q("3"), "mthreads.com/gpu": q("8")}); n != 3 {
		t.Fatalf("count %d: only the given types are GPUs", n)
	}
	if r, ok := Resolve("example"); !ok || r != "vendor.example/gpu" {
		t.Fatalf("resolve by label: %q %v", r, ok)
	}
	if r, _ := Resolve(""); r != "vendor.example/gpu" {
		t.Fatalf("the first is the default: %q", r)
	}
	if Set("gpu") == nil || Set("a/b,a/b") == nil {
		t.Fatal("a name without a domain, or given twice, is refused")
	}
}
