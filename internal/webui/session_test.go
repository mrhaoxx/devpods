// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestSessionRoundtrip(t *testing.T) {
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), 24*time.Hour)
	now := time.Unix(1_800_000_000, 0)

	tok := sm.Mint("gl-alice", true, now)
	sess, err := sm.Verify(tok, now.Add(23*time.Hour))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if sess.User != "gl-alice" || !sess.Admin {
		t.Fatalf("unexpected session: %+v", sess)
	}
}

func TestSessionExpiry(t *testing.T) {
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	now := time.Unix(1_800_000_000, 0)
	tok := sm.Mint("gl-alice", false, now)
	if _, err := sm.Verify(tok, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestSessionTamper(t *testing.T) {
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	now := time.Unix(1_800_000_000, 0)
	tok := sm.Mint("gl-alice", false, now)

	// Flip a payload byte.
	parts := strings.SplitN(tok, ".", 2)
	mutated := "A" + parts[0][1:] + "." + parts[1]
	if mutated != tok {
		if _, err := sm.Verify(mutated, now); err == nil {
			t.Fatal("expected signature error on tampered payload")
		}
	}

	// Different key must reject.
	other := webui.NewSessionManager([]byte("ffffffffffffffffffffffffffffffff"), time.Hour)
	if _, err := other.Verify(tok, now); err == nil {
		t.Fatal("expected signature error under different key")
	}

	// Garbage.
	if _, err := sm.Verify("not-a-token", now); err == nil {
		t.Fatal("expected parse error")
	}
}
