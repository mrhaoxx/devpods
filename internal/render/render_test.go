// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func TestOwnerLabels(t *testing.T) {
	got := render.OwnerLabels("alice")
	want := map[string]string{
		"devpod.io/owner":              "alice",
		"app.kubernetes.io/managed-by": "devpod-controller",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("OwnerLabels[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestDevPodLabels_IncludesOwnerAndName(t *testing.T) {
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-dev"},
		Spec:       devpodv1alpha1.DevPodSpec{Owner: "alice"},
	}
	got := render.DevPodLabels(dp)
	if got["devpod.io/owner"] != "alice" {
		t.Errorf("missing owner label: %v", got)
	}
	if got["devpod.io/devpod"] != "frontend-dev" {
		t.Errorf("missing devpod label: %v", got)
	}
}

func TestObjectNames(t *testing.T) {
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-dev"},
		Spec:       devpodv1alpha1.DevPodSpec{Owner: "alice"},
	}
	if got, want := render.PodName(dp), "alice-frontend-dev"; got != want {
		t.Errorf("PodName = %q, want %q", got, want)
	}
	if got, want := render.ServiceName(dp), "alice-frontend-dev"; got != want {
		t.Errorf("ServiceName = %q, want %q", got, want)
	}
	if got, want := render.HostKeySecretName(dp), "alice-frontend-dev-hostkey"; got != want {
		t.Errorf("HostKeySecretName = %q, want %q", got, want)
	}
	if got, want := render.HomePVCName(dp), "alice-frontend-dev-home"; got != want {
		t.Errorf("HomePVCName = %q, want %q", got, want)
	}
	if got, want := render.OwnerNetPolName("alice"), "devpod-allow-alice"; got != want {
		t.Errorf("OwnerNetPolName = %q, want %q", got, want)
	}
}
