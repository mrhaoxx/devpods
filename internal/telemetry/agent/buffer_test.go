// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

func metricRec(i int) wire.Record {
	return wire.Record{K: wire.KContainer, C: &wire.Container{CPU: float64(i), Cpuset: strings.Repeat("x", 200)}}
}

func auditRec(i int) wire.Record {
	return wire.Record{K: wire.KCommand, Cmd: &wire.Command{EventID: strings.Repeat("e", 200), ShellPid: i}}
}

func TestBuffer_MetricsDroppedBeforeAudit(t *testing.T) {
	b := NewBuffer(40 << 10) // audit 10 KiB, metrics 30 KiB
	for i := 1; i <= 20; i++ {
		b.Add(uint64(i), []wire.Record{auditRec(i)})
	}
	for i := 21; i <= 400; i++ {
		b.Add(uint64(i), []wire.Record{metricRec(i)})
	}
	_, _, recs := b.Pending(1 << 30)
	var audit, metrics int
	minMetric := 1 << 30
	for _, r := range recs {
		if r.K.Audit() {
			audit++
		} else {
			metrics++
			minMetric = min(minMetric, int(r.C.CPU))
		}
	}
	if audit != 20 {
		t.Fatalf("all audit records must survive metrics pressure, got %d", audit)
	}
	if metrics == 0 || minMetric <= 21 || b.Size() > 40<<10 {
		t.Fatalf("oldest metrics must be dropped: metrics=%d oldest=%d size=%d", metrics, minMetric, b.Size())
	}
}

func TestBuffer_AuditOverflowEmitsGap(t *testing.T) {
	b := NewBuffer(4 << 10) // audit 1 KiB
	for i := 1; i <= 30; i++ {
		b.Add(uint64(i), []wire.Record{auditRec(i)})
	}
	_, _, recs := b.Pending(1 << 30)
	var gaps []string
	for _, r := range recs {
		if r.K == wire.KAlert && r.Alert.Kind == "gap" {
			gaps = append(gaps, r.Alert.Detail)
		}
	}
	if len(gaps) != 1 || !strings.Contains(gaps[0], "audit records dropped") {
		t.Fatalf("want exactly one cumulative gap alert, got %q", gaps)
	}
}

func TestBuffer_PendingAndAck(t *testing.T) {
	b := NewBuffer(1 << 20)
	for i := 1; i <= 5; i++ {
		b.Add(uint64(i), []wire.Record{metricRec(i), auditRec(i)})
	}
	from, to, recs := b.Pending(1 << 30)
	if from != 1 || to != 5 || len(recs) != 10 {
		t.Fatalf("%d..%d %d", from, to, len(recs))
	}
	b.Ack(3)
	from, to, recs = b.Pending(1 << 30)
	if from != 4 || to != 5 || len(recs) != 4 {
		t.Fatalf("after ack: %d..%d %d", from, to, len(recs))
	}
	// maxBytes bounds the batch by whole seconds
	from, to, _ = b.Pending(1)
	if from != 4 || to != 4 {
		t.Fatalf("one second at least: %d..%d", from, to)
	}
}

type fakeSleep struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (f *fakeSleep) sleep(ctx context.Context, d time.Duration) bool {
	f.mu.Lock()
	f.delays = append(f.delays, d)
	f.mu.Unlock()
	return ctx.Err() == nil
}

func TestUploader_AckAndRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		h, recs, err := wire.Decode(r.Body, 8<<20)
		if err != nil || h.FromSeq != 1 || h.Seq != 3 || len(recs) != 3 {
			t.Errorf("batch %+v %d %v", h, len(recs), err)
		}
		_ = json.NewEncoder(w).Encode(wire.Ack{AckedSeq: h.Seq})
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("t1\n"), 0o600)
	b := NewBuffer(1 << 20)
	for i := 1; i <= 3; i++ {
		b.Add(uint64(i), []wire.Record{metricRec(i)})
	}
	fs := &fakeSleep{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := &Uploader{URL: func() string { return srv.URL }, TokenPath: tok, Sleep: fs.sleep}
	go u.Run(ctx, func() wire.Header { return wire.Header{V: wire.Version, DevPodUID: "u", BootID: "b"} }, b)
	for ctx.Err() == nil && b.Len() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Len() != 0 {
		t.Fatal("buffer not drained after the server recovered")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.delays) < 2 || fs.delays[0] < 800*time.Millisecond || fs.delays[1] < fs.delays[0] {
		t.Fatalf("retries must back off from ~1s: %v", fs.delays)
	}
}

func TestUploader_TokenReread(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		h, _, _ := wire.Decode(r.Body, 8<<20)
		_ = json.NewEncoder(w).Encode(wire.Ack{AckedSeq: h.Seq})
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("old"), 0o600)
	b := NewBuffer(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := &Uploader{URL: func() string { return srv.URL }, TokenPath: tok, Sleep: (&fakeSleep{}).sleep}
	go u.Run(ctx, func() wire.Header { return wire.Header{V: wire.Version, DevPodUID: "u"} }, b)
	b.Add(1, []wire.Record{metricRec(1)})
	waitLen(t, b)
	_ = os.WriteFile(tok, []byte("new"), 0o600)
	b.Add(2, []wire.Record{metricRec(2)})
	waitLen(t, b)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "Bearer old" || seen[1] != "Bearer new" {
		t.Fatalf("authorization headers %v", seen)
	}
}

func waitLen(t *testing.T, b *Buffer) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for b.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Len() > 0 {
		t.Fatal("not uploaded")
	}
}
