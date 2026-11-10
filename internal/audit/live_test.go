// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
)

func out(s *Session, ch uint32, text string) { s.record(ch, rawlog.KindDataOut, []byte(text)) }

func backlog(t *testing.T, w *Watch) string {
	t.Helper()
	var b strings.Builder
	if err := w.Backlog(func(r rawlog.Record) error {
		if r.Kind == rawlog.KindDataOut {
			b.Write(r.Payload)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func next(t *testing.T, w *Watch) (rawlog.Record, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return w.Next(ctx)
}

func TestWatch_HandOverWithoutLossOrDuplication(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	out(s, 1, "a")
	out(s, 2, "other channel")
	s.sealAndFlush() // segment 1: sealed, still in the spool
	out(s, 1, "b")   // segment 2: open
	w, err := g.r.Watch(s.meta.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.From != 1 {
		t.Fatalf("From = %d, want 1 (segment 1 is still local)", w.From)
	}
	out(s, 1, "c")
	out(s, 2, "skip")
	out(s, 1, "d")
	got := backlog(t, w)
	for _, want := range []string{"c", "d"} {
		r, err := next(t, w)
		if err != nil {
			t.Fatal(err)
		}
		got += string(r.Payload)
		_ = want
	}
	if got != "abcd" {
		t.Fatalf("backlog + live = %q, want abcd", got)
	}
}

func TestWatch_UploadedSegmentsAreLeftToObjectStorage(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	out(s, 1, "old")
	s.sealAndFlush()
	if err := g.r.up.Flush(context.Background()); err != nil { // segment 1 uploaded
		t.Fatal(err)
	}
	out(s, 1, "new")
	w, err := g.r.Watch(s.meta.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.From != 2 || backlog(t, w) != "new" {
		t.Fatalf("From = %d backlog %q", w.From, backlog(t, w))
	}
}

func TestWatch_NoOpenSegmentStartsAtTheNextOne(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	out(s, 1, "x")
	s.sealAndFlush()
	if err := g.r.up.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := g.r.Watch(s.meta.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.From != 2 || backlog(t, w) != "" {
		t.Fatalf("From = %d backlog %q", w.From, backlog(t, w))
	}
	out(s, 1, "y")
	if r, err := next(t, w); err != nil || string(r.Payload) != "y" {
		t.Fatalf("live: %q %v", r.Payload, err)
	}
}

func TestWatch_OverflowDropsOnlyTheWatcher(t *testing.T) {
	old := watchQueueBytes
	watchQueueBytes = 8
	defer func() { watchQueueBytes = old }()
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	w, err := g.r.Watch(s.meta.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 5; i++ {
		out(s, 1, "12345") // nobody reads: the third write overflows
	}
	var last error
	for i := 0; i < 5 && last == nil; i++ {
		_, last = next(t, w)
	}
	if !errors.Is(last, ErrWatchOverflow) {
		t.Fatalf("got %v, want ErrWatchOverflow", last)
	}
	out(s, 1, "recording goes on") // must not block or fail
	s.sealAndFlush()
}

func TestWatch_EndsWithTheChannelAndTheSession(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	w1, err := g.r.Watch(s.meta.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w1.Close()
	w2, err := g.r.Watch(s.meta.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	s.record(1, rawlog.KindChannelClose, rawlog.Encode(rawlog.ChannelClose{}))
	if r, err := next(t, w1); err != nil || r.Kind != rawlog.KindChannelClose {
		t.Fatalf("close record: %v %v", r.Kind, err)
	}
	if _, err := next(t, w1); !errors.Is(err, io.EOF) {
		t.Fatalf("after the channel closed: %v", err)
	}
	s.Close("done", 0, 0)
	if _, err := next(t, w2); !errors.Is(err, io.EOF) {
		t.Fatalf("after the session closed: %v", err)
	}
	if _, err := g.r.Watch(s.meta.ID, 1); !errors.Is(err, ErrNoSession) {
		t.Fatalf("closed session: %v", err)
	}
	if _, err := g.r.Watch("nope", 1); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown session: %v", err)
	}
}
