// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gpures

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A model as people write it matches the models it names — word by word,
// a word one of the model's or several adjacent ones joined — and no
// model a word of it is only the start of.
func TestModelMatches(t *testing.T) {
	for _, c := range []struct {
		want, model string
		match       bool
	}{
		{"a100", "NVIDIA-A100-80GB-PCIe", true},
		{"a100", "NVIDIA-A100-PCIE-40GB", true},
		{"a100-80gb", "NVIDIA-A100-80GB-PCIe", true},
		{"a100-80gb", "NVIDIA-A100-PCIE-40GB", false},
		{"a100-40gb", "NVIDIA-A100-PCIE-40GB", true},
		{"4090", "NVIDIA-GeForce-RTX-4090", true},
		{"rtx-4090", "NVIDIA-GeForce-RTX-4090", true},
		{"rtx4090", "NVIDIA-GeForce-RTX-4090", true},
		{"s5000", "MTT-S5000", true},
		{"mtt-s5000", "MTT-S5000", true},
		{"MTTS5000", "MTT-S5000", true},
		{"b300", "NVIDIA B300 SXM6 AC", true},
		{"a10", "NVIDIA-A100-80GB-PCIe", false},
		{"40", "NVIDIA-A100-PCIE-40GB", false},
		{"nvidia", "NVIDIA", true},
		{"h800", "NVIDIA-H800-PCIe", true},
		{"h800", "NVIDIA-GeForce-RTX-4090", false},
	} {
		if got := ModelMatches(c.want, []string{c.model}); got != c.match {
			t.Errorf("%q against %q: %v", c.want, c.model, got)
		}
	}
	// a machine of mixed cards: only a model every card is
	if ModelMatches("s5000", []string{"MTT-S5000", "MTT-S4000"}) {
		t.Error("a mixed machine matched a model one of its cards is not")
	}
	if !ModelMatches("mtt", []string{"MTT-S5000", "MTT-S4000"}) {
		t.Error("a mixed machine did not match what all its cards are")
	}
	if ModelMatches("a100", nil) || ModelMatches("", []string{"NVIDIA-A100-80GB-PCIe"}) {
		t.Error("nothing to match matched")
	}
}

func node(labels map[string]string, cards map[corev1.ResourceName]int64) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "m", Labels: labels}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{}, Capacity: corev1.ResourceList{}}}
	for r, v := range cards {
		n.Status.Allocatable[r] = *resource.NewQuantity(v, resource.DecimalSI)
		n.Status.Capacity[r] = *resource.NewQuantity(v, resource.DecimalSI)
	}
	return n
}

// A machine's models, from the first source that says: an admin's label,
// the whole machine's product label, each card's, the vendor.
func TestModels(t *testing.T) {
	mtt := map[string]string{}
	for i := range 8 {
		mtt["mthreads.com/gpu"+string(rune('0'+i))+".product"] = "MTT-S5000"
	}
	for _, c := range []struct {
		name string
		n    *corev1.Node
		want []string
	}{
		{"an admin's label first", node(map[string]string{"devpod.io/gpu-model": "NVIDIA-B300-SXM6-AC", "nvidia.com/gpu.product": "Y"}, map[corev1.ResourceName]int64{NVIDIA: 8}), []string{"NVIDIA-B300-SXM6-AC"}},
		{"NVIDIA's feature discovery", node(map[string]string{"nvidia.com/gpu.product": "NVIDIA-GeForce-RTX-4090"}, map[corev1.ResourceName]int64{NVIDIA: 4}), []string{"NVIDIA-GeForce-RTX-4090"}},
		{"Moore Threads' per card", node(mtt, map[corev1.ResourceName]int64{"mthreads.com/gpu": 8}), []string{"MTT-S5000"}},
		{"mixed cards", node(map[string]string{"mthreads.com/gpu0.product": "MTT-S5000", "mthreads.com/gpu1.product": "MTT-S4000"}, map[corev1.ResourceName]int64{"mthreads.com/gpu": 2}), []string{"MTT-S4000", "MTT-S5000"}},
		{"no label: the vendor", node(map[string]string{"nvidia.com/gpu": "true"}, map[corev1.ResourceName]int64{NVIDIA: 8}), []string{"NVIDIA"}},
		{"no cards", node(map[string]string{"nvidia.com/gpu.product": "X"}, nil), nil},
	} {
		if got := Models(c.n); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
