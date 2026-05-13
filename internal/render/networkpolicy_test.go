// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/mrhaoxx/devpod/internal/render"
)

func TestDefaultDenyNetworkPolicy(t *testing.T) {
	np := render.DefaultDenyNetworkPolicy("devpods")
	if got, want := np.Name, "devpod-default-deny"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := np.Namespace, "devpods"; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}
	if len(np.Spec.PodSelector.MatchLabels) != 0 {
		t.Errorf("podSelector should select ALL pods (empty); got %v", np.Spec.PodSelector.MatchLabels)
	}

	wantTypes := map[networkingv1.PolicyType]bool{
		networkingv1.PolicyTypeIngress: false,
		networkingv1.PolicyTypeEgress:  false,
	}
	for _, pt := range np.Spec.PolicyTypes {
		wantTypes[pt] = true
	}
	for k, v := range wantTypes {
		if !v {
			t.Errorf("missing PolicyType %s", k)
		}
	}
	// Default deny = empty Ingress / Egress lists.
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("default-deny must have no Ingress rules; got %d", len(np.Spec.Ingress))
	}
	if len(np.Spec.Egress) != 0 {
		t.Errorf("default-deny must have no Egress rules; got %d", len(np.Spec.Egress))
	}
}

func TestOwnerAllowNetworkPolicy(t *testing.T) {
	np := render.OwnerAllowNetworkPolicy("devpods", "alice", "devpod-system")
	if got, want := np.Name, "devpod-allow-alice"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := np.Spec.PodSelector.MatchLabels[render.LabelOwner], "alice"; got != want {
		t.Errorf("podSelector owner = %q, want %q", got, want)
	}

	if len(np.Spec.Ingress) < 2 {
		t.Errorf("expected at least 2 ingress rules (same-owner + gateway); got %d", len(np.Spec.Ingress))
	}
	// One rule must allow port 22 from the gateway namespace.
	gatewayRule := false
	for _, rule := range np.Spec.Ingress {
		for _, from := range rule.From {
			if from.NamespaceSelector != nil &&
				from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "devpod-system" {
				gatewayRule = true
			}
		}
	}
	if !gatewayRule {
		t.Errorf("no ingress rule from devpod-system namespace")
	}

	if len(np.Spec.Egress) == 0 {
		t.Errorf("expected egress rules (DNS, internet)")
	}
}
