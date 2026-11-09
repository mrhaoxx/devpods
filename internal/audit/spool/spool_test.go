// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package spool

import (
	"bytes"
	"errors"
	"testing"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

func writeObject(t *testing.T, s *Spool, key string, n int) {
	t.Helper()
	f, err := s.Create(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("x"), n)); err != nil {
		t.Fatal(err)
	}
	if err := f.Seal(); err != nil {
		t.Fatal(err)
	}
}

func TestSpool_UsageAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		writeObject(t, s, "raw/k", 1000)
	}
	if err := s.WriteBatch(store.Batch{Alerts: []store.AlertRow{{Kind: "gap"}}}); err != nil {
		t.Fatal(err)
	}
	// An unsealed file from a crashed writer is discarded on reopen.
	f, _ := s.Create("raw/partial")
	_, _ = f.Write([]byte("partial"))

	before := s.Usage() - int64(len("partial"))
	s2, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Usage() != before || before < 3000 {
		t.Fatalf("usage after reopen %d, want %d", s2.Usage(), before)
	}
	items, err := s2.Ready()
	if err != nil || len(items) != 4 {
		t.Fatalf("ready items %d (%v)", len(items), err)
	}
}

func TestSpool_Levels(t *testing.T) {
	s, err := Open(t.TempDir(), 100<<10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Level() != LevelOK {
		t.Fatal("empty spool must be OK")
	}
	writeObject(t, s, "a", 81<<10)
	if s.Level() != LevelWarn {
		t.Fatalf("81%% → %v, want Warn", s.Level())
	}
	writeObject(t, s, "b", 10<<10)
	if s.Level() != LevelHigh {
		t.Fatalf("91%% → %v, want High", s.Level())
	}
}

func TestSpool_ContentFullBatchStillAccepted(t *testing.T) {
	s, err := Open(t.TempDir(), 100<<10)
	if err != nil {
		t.Fatal(err)
	}
	writeObject(t, s, "a", 94<<10)
	f, err := s.Create("b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 2<<10)); !errors.Is(err, ErrFull) {
		t.Fatalf("write into the event reserve: %v, want ErrFull", err)
	}
	_ = f.Discard()
	// Fill the content budget (max − max/20) exactly; the 1-byte key
	// sidecar of "c" counts too.
	budget := int64(100<<10 - (100<<10)/20)
	writeObject(t, s, "c", int(budget-s.Usage()-1))
	if s.Level() != LevelFull {
		t.Fatalf("level %v at usage %d/%d, want Full", s.Level(), s.Usage(), budget)
	}
	if _, err := s.Create("d"); !errors.Is(err, ErrFull) {
		t.Fatalf("Create at Full: %v, want ErrFull", err)
	}
	if err := s.WriteBatch(store.Batch{Alerts: []store.AlertRow{{Kind: "gap"}}}); err != nil {
		t.Fatalf("batch must use the reserve: %v", err)
	}
}
