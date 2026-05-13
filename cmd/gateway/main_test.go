// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestBuildIdentitySources_NoLDAP_OnlyCRD(t *testing.T) {
	cfg := &devpodv1alpha1.GatewayConfig{}
	srcs, err := buildIdentitySources(context.Background(), nil /*client unused*/, cfg, "/var/empty/ldap")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("len srcs = %d, want 1 (crd only)", len(srcs))
	}
	if srcs[0].Name() != "crd" {
		t.Errorf("srcs[0].Name() = %q, want %q", srcs[0].Name(), "crd")
	}
}

func TestBuildIdentitySources_LDAPSpec_RequiresMountedSecrets(t *testing.T) {
	cfg := &devpodv1alpha1.GatewayConfig{
		Spec: devpodv1alpha1.GatewayConfigSpec{
			LDAP: &devpodv1alpha1.LDAPSpec{
				URL:    "ldaps://ldap.example.test:636",
				BindDN: "cn=svc,dc=example,dc=test",
				BaseDN: "dc=example,dc=test",
			},
		},
	}
	_, err := buildIdentitySources(context.Background(), nil, cfg, "/var/empty/ldap")
	if err == nil {
		t.Fatal("expected error: LDAP configured but no Secret files mounted")
	}
}
