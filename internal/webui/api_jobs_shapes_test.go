// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// dropJob deletes a job a test submitted (so the next is not held to the
// owner's count of jobs).
func dropJob(t *testing.T, id string) {
	t.Helper()
	j := jobNow(t, id)
	_ = k8sClient.Delete(context.Background(), &j)
}

// A job's rank shape is stored as one of its forms: machines alone (one
// rank on each), machines × ranks on each, or ranks packed; what does not
// add up is refused, saying why.
func TestSubmitShapes(t *testing.T) {
	p := jobRig(t, "js", "js-alice")
	alice := p.as("js-alice")
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"from": map[string]any{"devpod": "js-alice-dev1"}, "gpus": 1, "cpus": 4, "command": "true"}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	for name, c := range map[string]struct {
		extra               map[string]any
		nodes, tasks, per   int32
		refused, refusedFor string
	}{
		"machines alone":         {extra: map[string]any{"nodes": 1}, nodes: 1},
		"ranks on each":          {extra: map[string]any{"nodes": 1, "tasksPerNode": 2}, nodes: 1, tasks: 2, per: 2},
		"ranks over machines":    {extra: map[string]any{"nodes": 1, "tasks": 4}, nodes: 1, tasks: 4, per: 4},
		"packed":                 {extra: map[string]any{"nodes": 0, "tasks": 5}, tasks: 5},
		"packed, at most 2":      {extra: map[string]any{"nodes": 0, "tasks": 2, "tasksPerNode": 2}, tasks: 2, per: 2},
		"not a multiple":         {extra: map[string]any{"nodes": 2, "tasks": 5}, refused: "multiple"},
		"too many ranks":         {extra: map[string]any{"nodes": 0, "tasks": 300}, refused: "256"},
		"neither":                {extra: map[string]any{"nodes": 0}, refused: "machines"},
		"each and all disagree":  {extra: map[string]any{"nodes": 1, "tasks": 4, "tasksPerNode": 2}, refused: "each"},
		"one rank each, by name": {extra: map[string]any{"nodes": 1, "tasksPerNode": 1}, nodes: 1},
	} {
		code, v := submitJob(t, p.s, alice, body(c.extra))
		if c.refused != "" {
			if code != http.StatusBadRequest || !strings.Contains(v.Message, c.refused) {
				t.Errorf("%s: %d %q, want 400 saying %q", name, code, v.Message, c.refused)
			}
			continue
		}
		if code != http.StatusCreated {
			t.Errorf("%s: %d %+v", name, code, v)
			continue
		}
		j := jobNow(t, v.ID)
		if j.Spec.Nodes != c.nodes || j.Spec.Tasks != c.tasks || j.Spec.TasksPerNode != c.per {
			t.Errorf("%s: stored nodes %d tasks %d per %d", name, j.Spec.Nodes, j.Spec.Tasks, j.Spec.TasksPerNode)
		}
		dropJob(t, v.ID)
	}
}

// Ranks packed past what the machines could ever hold are refused,
// saying how many they hold.
func TestSubmitPackedNoMachine(t *testing.T) {
	p := jobRig(t, "jn", "jn-alice")
	code, v := submitJob(t, p.s, p.as("jn-alice"), map[string]any{"from": map[string]any{"devpod": "jn-alice-dev1"}, "nodes": 0, "tasks": 10, "gpus": 1, "cpus": 4, "command": "true"})
	if code != http.StatusConflict || v.Code != "NO_MACHINE" || !strings.Contains(v.Message, "8 ranks") {
		t.Fatalf("10 ranks of a card on one 8-card machine: %d %+v", code, v)
	}
}

// labelMachine sets labels on a machine (and, with cards, its cards of
// kind), and waits for the cache to have them.
func labelMachine(t *testing.T, name string, labels map[string]string, kind corev1.ResourceName, cards int64) {
	t.Helper()
	ctx := context.Background()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var n corev1.Node
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			return err
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		for k, v := range labels {
			n.Labels[k] = v
		}
		return k8sClient.Update(ctx, &n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind != "" {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var n corev1.Node
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
				return err
			}
			n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("192"), kind: *resource.NewQuantity(cards, resource.DecimalSI)}
			n.Status.Capacity = n.Status.Allocatable
			return k8sClient.Status().Update(ctx, &n)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		var n corev1.Node
		if k8sClient.Get(ctx, types.NamespacedName{Name: name}, &n) != nil {
			return false
		}
		for k, v := range labels {
			if n.Labels[k] != v {
				return false
			}
		}
		q := n.Status.Allocatable[kind]
		return kind == "" || q.Value() == cards
	})
}

// A GPU model picks the machines whose cards it names; one no machine
// has is refused with the models there and their cards; one only another
// vendor's machines have is refused naming both vendors.
func TestSubmitGPUModel(t *testing.T) {
	p := jobRig(t, "jg", "jg-alice")
	gpuMachine(t, p.n2)
	labelMachine(t, p.n1, map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-80GB-PCIe"}, "", 0)
	labelMachine(t, p.n2, map[string]string{"nvidia.com/gpu.product": "NVIDIA-GeForce-RTX-4090"}, "", 0)
	alice := p.as("jg-alice")
	body := func(model string) map[string]any {
		return map[string]any{"from": map[string]any{"devpod": "jg-alice-dev1"}, "nodes": 1, "gpus": 1, "cpus": 4, "command": "true", "gpuModel": model}
	}
	code, v := submitJob(t, p.s, alice, body("a100"))
	if code != http.StatusCreated {
		t.Fatalf("a100: %d %+v", code, v)
	}
	if j := jobNow(t, v.ID); j.Spec.GPUModel != "a100" {
		t.Fatalf("stored model %q", j.Spec.GPUModel)
	}
	dropJob(t, v.ID)
	code, v = submitJob(t, p.s, alice, body("h800"))
	if code != http.StatusConflict || v.Code != "NO_GPU_MODEL" || !strings.Contains(v.Message, "NVIDIA-A100-80GB-PCIe") || !strings.Contains(v.Message, "NVIDIA-GeForce-RTX-4090") {
		t.Fatalf("h800: %d %+v", code, v)
	}
	// n2 becomes a Moore Threads machine: its model is no NVIDIA source's
	labelMachine(t, p.n2, map[string]string{"nvidia.com/gpu.product": "", "mthreads.com/gpu0.product": "MTT-S5000"}, "mthreads.com/gpu", 8)
	code, v = submitJob(t, p.s, alice, body("s5000"))
	if code != http.StatusConflict || v.Code != "NO_GPU_MODEL" || !strings.Contains(v.Message, "Moore Threads") || !strings.Contains(v.Message, "NVIDIA") {
		t.Fatalf("s5000 from an NVIDIA source: %d %+v", code, v)
	}
	if code, v := submitJob(t, p.s, alice, map[string]any{"from": map[string]any{"devpod": "jg-alice-dev1"}, "nodes": 1, "cpus": 4, "command": "true", "gpuModel": "a100"}); code != http.StatusBadRequest {
		t.Fatalf("a model with no cards asked: %d %+v", code, v)
	}
	_ = devpodv1alpha1.ReservableNodeLabel
}
