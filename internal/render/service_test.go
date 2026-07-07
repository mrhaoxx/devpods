// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/mrhaoxx/devpod/internal/render"
)

func TestService_HeadlessOnSSHPort(t *testing.T) {
	svc, err := render.Service(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Service: %v", err)
	}

	if got, want := svc.Name, "frontend-dev"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := svc.Spec.ClusterIP, corev1.ClusterIPNone; got != want {
		t.Errorf("ClusterIP = %q, want %q (headless)", got, want)
	}
	if got, want := len(svc.Spec.Ports), 1; got != want {
		t.Fatalf("port count = %d, want %d", got, want)
	}
	p := svc.Spec.Ports[0]
	if p.Port != 2222 || p.TargetPort.IntValue() != 2222 {
		t.Errorf("port mapping wrong: %+v", p)
	}
	if svc.Spec.Selector[render.LabelDevPod] != "frontend-dev" {
		t.Errorf("selector missing devpod label: %v", svc.Spec.Selector)
	}
	if svc.Spec.Selector[render.LabelOwner] != "alice" {
		t.Errorf("selector missing owner label: %v", svc.Spec.Selector)
	}
}
