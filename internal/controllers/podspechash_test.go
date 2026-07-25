// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/mrhaoxx/devpod/internal/render"
)

// podWith builds a rendered-shaped Pod: the controller-injected bootstrap
// initContainer at index 0, then whatever the template author wrote.
func podWith(bootstrapImage, userInitImage, appImage string) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: render.SupervisorBootstrapContainerName, Image: bootstrapImage},
				{Name: "plat101-hpc", Image: userInitImage},
			},
			Containers: []corev1.Container{{Name: "dev", Image: appImage}},
		},
	}
}

// A supervisor bump must NOT read as drift: that initContainer already ran, so
// recreating the Pod delivers nothing and only drops the user's SSH session.
func TestPodSpecHashIgnoresBootstrapImage(t *testing.T) {
	before := podSpecHash(podWith("supervisor:v1", "hpc:1", "devbox:4"))
	after := podSpecHash(podWith("supervisor:v2", "hpc:1", "devbox:4"))
	if before != after {
		t.Fatalf("bumping the bootstrap image must not count as drift (%s vs %s)", before, after)
	}
}

// Everything the user or template author wrote still counts — editing a DevPod
// must keep applying, with the restart the UI already warns about.
func TestPodSpecHashSeesUserChanges(t *testing.T) {
	base := podSpecHash(podWith("supervisor:v1", "hpc:1", "devbox:4"))
	if h := podSpecHash(podWith("supervisor:v1", "hpc:2", "devbox:4")); h == base {
		t.Error("a template author's own initContainer image must count as drift")
	}
	if h := podSpecHash(podWith("supervisor:v1", "hpc:1", "devbox:5")); h == base {
		t.Error("the app container image must count as drift")
	}
}

// Only the container the controller injects is skipped, and only when it is
// actually there — a Pod whose first initContainer is the user's keeps it
// fully hashed.
func TestPodSpecHashSkipsOnlyTheInjectedOne(t *testing.T) {
	userOnly := func(img string) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "plat101-hpc", Image: img}},
			Containers:     []corev1.Container{{Name: "dev", Image: "devbox:4"}},
		}}
	}
	if podSpecHash(userOnly("hpc:1")) == podSpecHash(userOnly("hpc:2")) {
		t.Error("with no injected bootstrap, the first initContainer is the user's and must count")
	}
}
