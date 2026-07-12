// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestHashAndVerifyPassword(t *testing.T) {
	if _, err := webui.HashPassword("short", 8); err == nil {
		t.Fatal("expected min-length rejection")
	}
	hash, err := webui.HashPassword("correct horse", 8)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct horse" {
		t.Fatal("hash must not be the plaintext")
	}
	if !webui.VerifyPassword(hash, "correct horse") {
		t.Fatal("verify should accept the right password")
	}
	if webui.VerifyPassword(hash, "wrong") {
		t.Fatal("verify should reject the wrong password")
	}
	if webui.VerifyPassword("not-a-hash", "anything") {
		t.Fatal("verify should reject a malformed hash")
	}
}

func TestAdminFor(t *testing.T) {
	s := &webui.Server{Admins: map[string]bool{"root": true}}
	if !s.AdminForTest("root", nil) {
		t.Fatal("allowlist admin")
	}
	if s.AdminForTest("alice", nil) {
		t.Fatal("non-admin, no user")
	}
	u := &devpodv1alpha1.User{Spec: devpodv1alpha1.UserSpec{Admin: true}}
	if !s.AdminForTest("alice", u) {
		t.Fatal("spec.admin should grant")
	}
	u2 := &devpodv1alpha1.User{}
	if s.AdminForTest("alice", u2) {
		t.Fatal("spec.admin false, not in allowlist")
	}
}
