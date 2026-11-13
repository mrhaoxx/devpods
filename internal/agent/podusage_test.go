// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mrhaoxx/devpod/internal/agent"
)

// What other workloads hold on a node: the requests of every pod placed
// there and not finished, except the DevPods' own pods (the console counts
// those from the DevPods).
func TestTallyNodes(t *testing.T) {
	rl := func(kv ...string) corev1.ResourceList {
		l := corev1.ResourceList{}
		for i := 0; i < len(kv); i += 2 {
			l[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
		}
		return l
	}
	pod := func(ns, name, node string, phase corev1.PodPhase, labels map[string]string, init []corev1.ResourceList, cs ...corev1.ResourceList) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
			Spec: corev1.PodSpec{NodeName: node}, Status: corev1.PodStatus{Phase: phase}}
		for _, r := range init {
			p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Resources: corev1.ResourceRequirements{Requests: r}})
		}
		for _, r := range cs {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Resources: corev1.ResourceRequirements{Requests: r}})
		}
		return p
	}
	pods := []*corev1.Pod{
		pod("serving", "sglang-0", "n1", corev1.PodRunning, nil, nil, rl("cpu", "4", "memory", "8Gi", "nvidia.com/gpu", "2")),
		// the containers add up; a larger init container is the floor, per resource
		pod("serving", "x", "n1", corev1.PodPending, nil, []corev1.ResourceList{rl("cpu", "10")}, rl("cpu", "1", "memory", "1Gi"), rl("cpu", "2", "nvidia.com/gpu", "1")),
		// a DevPod's own pod
		pod("devpods", "alice-train", "n1", corev1.PodRunning, map[string]string{"devpod.io/devpod": "alice-train"}, nil, rl("nvidia.com/gpu", "4")),
		// in the DevPod namespace but no DevPod's
		pod("devpods", "probe", "n2", corev1.PodRunning, nil, nil, rl("nvidia.com/gpu", "1")),
		// finished, or not placed: holds nothing
		pod("serving", "done", "n2", corev1.PodSucceeded, nil, nil, rl("nvidia.com/gpu", "8")),
		pod("serving", "waiting", "", corev1.PodPending, nil, nil, rl("nvidia.com/gpu", "8")),
	}
	got := agent.TallyNodes(pods, "devpods")
	want := map[string]agent.NodeUsage{
		"n1": {CPU: 14, Mem: 9 << 30, GPU: 3},
		"n2": {GPU: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
