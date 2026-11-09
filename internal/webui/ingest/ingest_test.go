// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

type fakeStore struct {
	mu     sync.Mutex
	calls  []store.TelemetryBatch
	tokens []string
	block  time.Duration
	err    error
}

func (f *fakeStore) InsertTelemetry(_ context.Context, b store.TelemetryBatch, token string) error {
	time.Sleep(f.block)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, b)
	f.tokens = append(f.tokens, token)
	return nil
}

func (f *fakeStore) snapshot() []store.TelemetryBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.TelemetryBatch(nil), f.calls...)
}

func newHandler(st Store, leader *atomic.Bool, interval time.Duration) *Handler {
	w := NewWriter(st)
	w.Interval = interval
	w.IsLeader = leader.Load
	return &Handler{
		Key: testKey, IsLeader: leader.Load, Writer: w, State: live.NewState(nil),
		DevPods: func(uid string) (string, string, bool) {
			if strings.HasPrefix(uid, "u") {
				return "alice", "dp-" + uid, true
			}
			return "", "", false
		},
	}
}

func leaderTrue() *atomic.Bool {
	var b atomic.Bool
	b.Store(true)
	return &b
}

func body(t *testing.T, h wire.Header, recs []wire.Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := wire.Encode(&buf, h, recs); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func containerRec(seq uint64) wire.Record {
	return wire.Record{K: wire.KContainer, C: &wire.Container{TS: time.Unix(int64(1000+seq), 0).UTC(), CPU: 1, Pids: seq}}
}

func post(h http.Handler, uid, token string, b []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/ingest/v1/batch", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("X-DevPod-UID", uid)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postSeq(t *testing.T, h http.Handler, uid string, from, to uint64) *httptest.ResponseRecorder {
	t.Helper()
	var recs []wire.Record
	for s := from; s <= to; s++ {
		recs = append(recs, containerRec(s))
	}
	hdr := wire.Header{V: wire.Version, DevPodUID: uid, BootID: "boot1", FromSeq: from, Seq: to, TS: time.Now().UTC()}
	return post(h, uid, wire.Token(testKey, uid), body(t, hdr, recs))
}

func acked(t *testing.T, rec *httptest.ResponseRecorder) uint64 {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var a wire.Ack
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a.AckedSeq
}

func TestIngest_NotLeader503(t *testing.T) {
	var leader atomic.Bool
	h := newHandler(&fakeStore{}, &leader, 10*time.Millisecond)
	if rec := postSeq(t, h, "u1", 1, 1); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("follower must answer 503, got %d", rec.Code)
	}
}

func TestIngest_BadToken401AndOversize413(t *testing.T) {
	st := &fakeStore{}
	h := newHandler(st, leaderTrue(), 10*time.Millisecond)
	hdr := wire.Header{V: wire.Version, DevPodUID: "u1", BootID: "b", Seq: 1}
	b := body(t, hdr, []wire.Record{containerRec(1)})
	if rec := post(h, "u1", wire.Token(testKey, "u2"), b); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another DevPod's token: %d", rec.Code)
	}
	unknown := wire.Header{V: wire.Version, DevPodUID: "x9", BootID: "b", Seq: 1}
	if rec := post(h, "x9", wire.Token(testKey, "x9"), body(t, unknown, nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown DevPod: %d", rec.Code)
	}
	big := make([]byte, 5<<20)
	_, _ = rand.Read(big)
	if rec := post(h, "u1", wire.Token(testKey, "u1"), big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("5 MiB body: %d", rec.Code)
	}
	bomb := []wire.Record{{K: wire.KAlert, Alert: &wire.Alert{Kind: "x", Detail: strings.Repeat("a", 40<<20)}}}
	if rec := post(h, "u1", wire.Token(testKey, "u1"), body(t, hdr, bomb)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("40 MiB decompressed: %d", rec.Code)
	}
	if len(st.snapshot()) != 0 {
		t.Fatal("rejected batches must not be written")
	}
}

func TestIngest_AckAfterWrite(t *testing.T) {
	st := &fakeStore{block: 500 * time.Millisecond}
	h := newHandler(st, leaderTrue(), 10*time.Millisecond)
	start := time.Now()
	if a := acked(t, postSeq(t, h, "u1", 1, 1)); a != 1 {
		t.Fatalf("acked %d", a)
	}
	if time.Since(start) < 500*time.Millisecond || len(st.snapshot()) != 1 {
		t.Fatal("the ack must wait for the insert")
	}
	st.mu.Lock()
	st.err, st.block = errors.New("clickhouse down"), 0
	st.mu.Unlock()
	if rec := postSeq(t, h, "u1", 2, 2); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("insert error must be 503, got %d", rec.Code)
	}
}

func TestIngest_MergesOneInsertPerSecond(t *testing.T) {
	st := &fakeStore{}
	h := newHandler(st, leaderTrue(), time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if rec := postSeq(t, h, fmt.Sprintf("u%02d", i), 1, 1); rec.Code != http.StatusOK {
				t.Errorf("u%02d: %d", i, rec.Code)
			}
		}(i)
	}
	wg.Wait()
	calls := st.snapshot()
	if len(calls) != 1 || len(calls[0].Containers) != 50 {
		t.Fatalf("want 1 insert with 50 rows, got %d inserts", len(calls))
	}
	if len(st.tokens[0]) != 64 {
		t.Fatalf("token %q", st.tokens[0])
	}
}

// seqCount counts container rows by their seq tag (Pids).
func seqCount(st *fakeStore) map[uint32]int {
	n := map[uint32]int{}
	for _, c := range st.snapshot() {
		for _, r := range c.Containers {
			n[r.Pids]++
		}
	}
	return n
}

func TestIngest_FailoverResendNoLossNoDup(t *testing.T) {
	st := &fakeStore{}
	leaderA := leaderTrue()
	a := newHandler(st, leaderA, 200*time.Millisecond)
	for s := uint64(1); s <= 5; s++ {
		if got := acked(t, postSeq(t, a, "u1", s, s)); got != s {
			t.Fatalf("seq %d acked %d", s, got)
		}
	}
	res := make(chan int, 1)
	go func() { res <- postSeq(t, a, "u1", 6, 6).Code }()
	time.Sleep(50 * time.Millisecond)
	leaderA.Store(false) // lease lost while seq 6 waits for the merge window
	if code := <-res; code != http.StatusServiceUnavailable {
		t.Fatalf("seq 6 on the old leader: %d", code)
	}
	b := newHandler(st, leaderTrue(), 10*time.Millisecond)
	if got := acked(t, postSeq(t, b, "u1", 6, 8)); got != 8 {
		t.Fatalf("new leader acked %d", got)
	}
	n := seqCount(st)
	for s := uint32(1); s <= 8; s++ {
		if n[s] != 1 {
			t.Fatalf("seq %d written %d times: %v", s, n[s], n)
		}
	}
}

func TestIngest_ReplayAcknowledgedWithoutRewrite(t *testing.T) {
	st := &fakeStore{}
	h := newHandler(st, leaderTrue(), 10*time.Millisecond)
	for i := 0; i < 2; i++ {
		if got := acked(t, postSeq(t, h, "u1", 1, 1)); got != 1 {
			t.Fatalf("acked %d", got)
		}
	}
	if n := len(st.snapshot()); n != 1 {
		t.Fatalf("replay rewrote: %d inserts", n)
	}
}

func TestIngest_RowsAlertsAndEvents(t *testing.T) {
	st := &fakeStore{}
	h := newHandler(st, leaderTrue(), 10*time.Millisecond)
	var mu sync.Mutex
	var events []string
	h.Events = func(uid, kind, detail string) {
		mu.Lock()
		events = append(events, uid+"/"+kind)
		mu.Unlock()
	}
	t0 := time.Unix(5000, 0).UTC()
	recs := []wire.Record{
		{K: wire.KProcEvent, PE: &wire.ProcEvent{TS: t0.Add(time.Minute), FirstSeen: t0, Event: "process_exit", Pid: 7, Start: 9, PPid: 3,
			Exe: "/usr/bin/python3", Cmdline: "python x.py", Attr: &wire.Attr{Peer: "10.0.0.9:1", How: "tree"}}},
		{K: wire.KProcEvent, PE: &wire.ProcEvent{TS: t0, FirstSeen: t0, Event: "untracked_entry", Pid: 40, Start: 50, Exe: "/bin/sh"}},
		{K: wire.KProcEvent, PE: &wire.ProcEvent{TS: t0, FirstSeen: t0, Event: "process_start", Pid: 41, Start: 51, Exe: "/bin/x", Attr: &wire.Attr{How: "unattributed"}}},
		{K: wire.KProcEvent, PE: &wire.ProcEvent{TS: t0, FirstSeen: t0, Event: "process_start", Pid: 42, Start: 52, Exe: "/usr/bin/nvidia-smi", Attr: &wire.Attr{How: "system"}}},
		{K: wire.KSSHConn, SC: &wire.SSHConn{TS: t0.Add(time.Minute), FirstSeen: t0, Pid: 50, Start: 100, Peer: "10.0.0.9:1", Closed: true}},
		{K: wire.KCommand, Cmd: &wire.Command{TS: t0, End: t0.Add(2 * time.Second), EventID: "e1", ShellPid: 60, Cmd: "make", Exit: 2, Attr: &wire.Attr{Peer: "10.0.0.9:1", How: "tree"}}},
		{K: wire.KAlert, Alert: &wire.Alert{TS: t0, Kind: "gap", Detail: "dropped 3"}},
	}
	hdr := wire.Header{V: wire.Version, DevPodUID: "u1", BootID: "b", Seq: 1}
	acked(t, post(h, "u1", wire.Token(testKey, "u1"), body(t, hdr, recs)))
	hdr.Seq = 2
	acked(t, post(h, "u1", wire.Token(testKey, "u1"), body(t, hdr, recs[1:2]))) // same alert kind again within a minute

	calls := st.snapshot()
	b := calls[0]
	if len(b.ProcRows) != 3 || b.ProcRows[2].Attribution != "system" {
		t.Fatalf("process rows (system processes are stored, not alerted) %+v", b.ProcRows)
	}
	exit := b.ProcRows[0]
	if exit.ExitSeen == nil || !exit.FirstSeen.Equal(t0) || exit.Exe != "/usr/bin/python3" || exit.SessionID != "pending" || exit.Attribution != "tree" {
		t.Fatalf("exit row %+v", exit)
	}
	if len(b.SSHConns) != 1 || b.SSHConns[0].ClosedAt == nil || !b.SSHConns[0].FirstSeen.Equal(t0) {
		t.Fatalf("ssh conns %+v", b.SSHConns)
	}
	if len(b.Commands) != 1 || b.Commands[0].Source != "shell_hook" || b.Commands[0].DurationMS != 2000 || *b.Commands[0].ExitCode != 2 || b.Commands[0].Attribution != "pending" {
		t.Fatalf("commands %+v", b.Commands)
	}
	kinds := map[string]bool{}
	for _, a := range b.Alerts {
		if a.Source != "supervisor" || a.DevPodUID != "u1" {
			t.Fatalf("alert %+v", a)
		}
		kinds[a.Kind] = true
	}
	if len(b.Alerts) != 3 || !kinds["untracked_entry"] || !kinds["unattributed"] || !kinds["gap"] {
		t.Fatalf("alerts %+v (system processes must not raise unattributed)", b.Alerts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("one Event per kind per DevPod per minute: %v", events)
	}
}

type fakeSessions struct {
	mu sync.Mutex
	m  map[string]map[string]store.SessionRef
}

func (f *fakeSessions) OpenSessionsByBackend(context.Context, time.Time) (map[string]map[string]store.SessionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m, nil
}

func TestResolver_PendingThenResolved(t *testing.T) {
	src := &fakeSessions{m: map[string]map[string]store.SessionRef{}}
	r := &Resolver{Source: src}
	ctx := context.Background()
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if sid, user := r.Resolve("u1", "10.0.0.9:41000"); sid != "pending" || user != "" {
		t.Fatalf("unknown peer: %q %q", sid, user)
	}
	src.mu.Lock()
	src.m = map[string]map[string]store.SessionRef{"u1": {"10.0.0.9:41000": {SessionID: "S1", User: "alice"}}}
	src.mu.Unlock()
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if sid, user := r.Resolve("u1", "10.0.0.9:41000"); sid != "S1" || user != "alice" {
		t.Fatalf("resolved: %q %q", sid, user)
	}
	if sid, _ := r.Resolve("u1", ""); sid != "" {
		t.Fatalf("no peer → no session, got %q", sid)
	}
}

func TestWriter_Stats(t *testing.T) {
	st := &fakeStore{block: 30 * time.Millisecond}
	w := NewWriter(st)
	w.Interval = 5 * time.Millisecond
	for i := 0; i < 3; i++ {
		done := make(chan error, 1)
		w.Submit("k", store.TelemetryBatch{Containers: make([]store.ContainerMetricRow, 2)}, func(err error) { done <- err })
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	s := w.TakeStats()
	if s.Flushes != 3 || s.Rows != 6 || s.Errors != 0 || s.P50 < 30*time.Millisecond || s.Max < s.P99 || s.P99 < s.P50 {
		t.Fatalf("stats %+v", s)
	}
	if again := w.TakeStats(); again.Flushes != 0 {
		t.Fatalf("TakeStats must reset: %+v", again)
	}
}

// Every batch is authenticated by DevPod UID: a known UID is looked up
// once per authCacheTTL, not per batch.
func TestIngest_AuthLookupCached(t *testing.T) {
	h := newHandler(&fakeStore{}, leaderTrue(), 5*time.Millisecond)
	lookups := 0
	h.DevPods = func(uid string) (string, string, bool) { lookups++; return "alice", "dp", uid == "u1" }
	for s := uint64(1); s <= 5; s++ {
		acked(t, postSeq(t, h, "u1", s, s))
	}
	if lookups != 1 {
		t.Fatalf("%d lookups for 5 batches", lookups)
	}
	for i := 0; i < 2; i++ { // unknown UIDs are not cached as valid
		if rec := postSeq(t, h, "u9", 1, 1); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown uid: %d", rec.Code)
		}
	}
}

// Nothing is decompressed before the caller is authenticated: a
// compressed frame can declare a huge window (spec review #1).
func TestIngest_UnauthenticatedNotDecompressed(t *testing.T) {
	h := newHandler(&fakeStore{}, leaderTrue(), 5*time.Millisecond)
	garbage := []byte("not a zstd frame at all")
	if rec := post(h, "u1", "wrong-token", garbage); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d (the body must not be read first)", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/ingest/v1/batch", bytes.NewReader(garbage))
	req.Header.Set("Authorization", "Bearer "+wire.Token(testKey, "u1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no X-DevPod-UID: %d", rec.Code)
	}
	// the body must belong to the authenticated DevPod
	hdr := wire.Header{V: wire.Version, DevPodUID: "u2", BootID: "b", Seq: 1}
	if rec := post(h, "u1", wire.Token(testKey, "u1"), body(t, hdr, []wire.Record{containerRec(1)})); rec.Code != http.StatusBadRequest {
		t.Fatalf("body for another DevPod: %d", rec.Code)
	}
}

// Decoded batches waiting for their write are bounded: over the budget
// the leader answers 503 with Retry-After instead of growing.
func TestIngest_InFlightBudget(t *testing.T) {
	st := &fakeStore{block: 300 * time.Millisecond}
	h := newHandler(st, leaderTrue(), 5*time.Millisecond)
	h.MaxInFlight = 1 // bytes: any second batch exceeds it
	first := make(chan int, 1)
	go func() { first <- postSeq(t, h, "u1", 1, 1).Code }()
	time.Sleep(100 * time.Millisecond)
	rec := postSeq(t, h, "u2", 1, 1)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over budget: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first batch: %d", code)
	}
	if code := postSeq(t, h, "u2", 1, 1).Code; code != http.StatusOK {
		t.Fatalf("after the first is written the budget frees: %d", code)
	}
}
