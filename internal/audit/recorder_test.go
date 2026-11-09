// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/spool"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

func TestRecorder_FailOpenWhenStoresDown(t *testing.T) {
	g := newRig(t, 256<<10)
	g.objs.fail, g.evs.fail = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.r.Run(ctx)

	s := g.start(policy.Effective{})
	ch, err := s.OpenChannel(ClientToBackend, "session", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = req(t, ch, ClientToBackend, "shell", nil)
	chunk := make([]byte, 32<<10)
	for i := 0; i < 40; i++ { // 1.25 MiB of incompressible output into a 256 KiB spool
		_, _ = rand.Read(chunk)
		if res := ch.Data(BackendToClient, false, chunk); !bytes.Equal(res.Pass, chunk) {
			t.Fatal("fail-open session must pass data through untouched")
		}
	}
	select {
	case <-s.Done():
		t.Fatal("fail-open session must never be killed")
	default:
	}
	ch.Close()
	s.Close("client_disconnect", 0, 0)

	if g.r.spool.Usage() > 256<<10 {
		t.Fatalf("spool usage %d exceeds its limit", g.r.spool.Usage())
	}
	if s.gaps.Load() == 0 {
		t.Fatal("dropped raw data must be counted as gaps")
	}
	items, _ := g.r.spool.Ready()
	gapAlert := false
	for _, it := range items {
		if it.Kind != spool.ItemBatch {
			continue
		}
		b, _ := os.ReadFile(it.Path)
		var batch store.Batch
		_ = json.Unmarshal(b, &batch)
		for _, a := range batch.Alerts {
			gapAlert = gapAlert || a.Kind == "gap"
		}
	}
	if !gapAlert {
		t.Fatal("a gap alert must be queued in the event reserve")
	}
}

func fillSpool(t *testing.T, sp *spool.Spool, n int) {
	t.Helper()
	f, err := sp.Create("filler")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if err := f.Seal(); err != nil {
		t.Fatal(err)
	}
}

func TestRecorder_FailClosed(t *testing.T) {
	g := newRig(t, 100<<10)
	s := g.start(policy.Effective{FailClosed: true})
	fillSpool(t, g.r.spool, 91<<10) // High
	if _, err := s.OpenChannel(ClientToBackend, "session", nil); !isDeny(err, MsgAuditUnavailable) {
		t.Fatalf("at High: %v", err)
	}
	open := g.start(policy.Effective{})
	if _, err := open.OpenChannel(ClientToBackend, "session", nil); err != nil {
		t.Fatalf("fail-open sessions are unaffected: %v", err)
	}
	budget := int64(100<<10 - (100<<10)/20)
	fillSpool(t, g.r.spool, int(budget-g.r.spool.Usage()-int64(len("filler")))) // Full
	if g.r.spool.Level() != spool.LevelFull {
		t.Fatalf("level %v, want full", g.r.spool.Level())
	}
	g.r.tick(time.Now())
	select {
	case <-s.Done():
	default:
		t.Fatal("fail-closed session must be torn down at Full")
	}
	select {
	case <-open.Done():
		t.Fatal("fail-open session must survive")
	default:
	}
}

func TestRecorder_DrainUploadsEverything(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	ch, _ := s.OpenChannel(ClientToBackend, "session", nil)
	_ = req(t, ch, ClientToBackend, "shell", nil)
	ch.Data(BackendToClient, false, []byte("$ "))
	ch.Close()
	s.Close("client_disconnect", 0, 0)
	all := g.drain()
	if items, _ := g.r.spool.Ready(); len(items) != 0 {
		t.Fatalf("%d items left after Drain", len(items))
	}
	v2 := false
	for _, r := range all.Sessions {
		v2 = v2 || r.Version == 2
	}
	if !v2 || len(all.RawSegments) == 0 {
		t.Fatalf("sessions %+v segments %d", all.Sessions, len(all.RawSegments))
	}
}

func TestNewSessionID_SortsByTime(t *testing.T) {
	a := NewSessionID(time.Unix(100, 0))
	b := NewSessionID(time.Unix(200, 0))
	if len(a) != 26 || a >= b {
		t.Fatalf("%s %s", a, b)
	}
}

func TestIsOpaqueTransfer(t *testing.T) {
	yes := []string{"rsync --server -vlogDtpre.iLsfxCIvu . /dst", "tar czf - /data", "/usr/bin/base64 secret.key", "dd if=/dev/sda", "cat > /tmp/x"}
	no := []string{"rsync -av a b", "ls", "cat /etc/os-release", "make tar"}
	for _, c := range yes {
		if !IsOpaqueTransfer(c) {
			t.Errorf("%q should be a suspected transfer", c)
		}
	}
	for _, c := range no {
		if IsOpaqueTransfer(c) {
			t.Errorf("%q should not be a suspected transfer", c)
		}
	}
}
