// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package spool

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

type fakeObjects struct {
	mu    sync.Mutex
	puts  map[string][]byte
	fails int
	// refuse: keys refused this many more times (-1: always)
	refuse map[string]int
	delay  time.Duration
	now    int // puts going at once
	most   int
}

func (f *fakeObjects) Put(_ context.Context, key string, body io.Reader, size int64) error {
	f.mu.Lock()
	f.now++
	f.most = max(f.most, f.now)
	delay := f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now--
	if n := f.refuse[key]; n != 0 {
		if n > 0 {
			f.refuse[key] = n - 1
		}
		return errors.New("s3 refuses " + key)
	}
	if f.fails > 0 {
		f.fails--
		return errors.New("s3 down")
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size mismatch")
	}
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = b
	return nil
}

func (f *fakeObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

type fakeEvents struct {
	mu     sync.Mutex
	tokens []string
	alerts int
	kinds  []string
}

func (f *fakeEvents) Insert(_ context.Context, b store.Batch, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	f.alerts += len(b.Alerts)
	for _, a := range b.Alerts {
		f.kinds = append(f.kinds, a.Kind)
	}
	return nil
}

func TestUploader_SuccessRemoves(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20)
	writeObject(t, s, "files/k1", 5)
	if err := s.WriteBatch(store.Batch{Alerts: []store.AlertRow{{Kind: "gap"}}}); err != nil {
		t.Fatal(err)
	}
	items, _ := s.Ready()
	var batchToken string
	for _, it := range items {
		if it.Kind == ItemBatch {
			batchToken = it.Token
		}
	}
	objs, evs := &fakeObjects{}, &fakeEvents{}
	u := &Uploader{Spool: s, Objects: objs, Events: evs}
	if err := u.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(objs.puts["files/k1"]) != "xxxxx" {
		t.Fatalf("puts %v", objs.puts)
	}
	if len(evs.tokens) != 1 || evs.tokens[0] != batchToken || evs.alerts != 1 {
		t.Fatalf("inserts %v (want token %s)", evs.tokens, batchToken)
	}
	if left, _ := s.Ready(); len(left) != 0 {
		t.Fatalf("%d items left after flush", len(left))
	}
	if s.Usage() != 0 {
		t.Fatalf("usage %d after flush", s.Usage())
	}
}

func TestUploader_FailureRetainsAndRetries(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20)
	writeObject(t, s, "raw/k", 3)
	objs := &fakeObjects{fails: 2}
	var mu sync.Mutex
	var attempts []int
	u := &Uploader{
		Spool: s, Objects: objs, Events: &fakeEvents{}, Interval: 5 * time.Millisecond,
		Backoff: func(n int) time.Duration {
			mu.Lock()
			attempts = append(attempts, n)
			mu.Unlock()
			return time.Millisecond
		},
	}
	if err := u.Flush(context.Background()); err == nil {
		t.Fatal("Flush must report the failure")
	}
	// each item has its own backoff: the failure counted is this one's first
	if items, _ := s.Ready(); len(items) != 1 {
		t.Fatal("a failed item must stay in the spool")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go u.Run(ctx)
	for {
		if items, _ := s.Ready(); len(items) == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("item never uploaded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("backoff attempts %v, want [1 2]: the item's first failure (Flush), then its second (Run)", attempts)
	}
}

// An object S3 keeps refusing waits out its own backoff; the other
// objects go, and so do the batches that do not name it.
func TestUploader_OneStuckObjectDoesNotHoldTheRest(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20)
	writeObject(t, s, "raw/stuck", 3)
	writeObject(t, s, "raw/fine", 4)
	if err := s.WriteBatch(store.Batch{Alerts: []store.AlertRow{{Kind: "unrelated"}}}); err != nil {
		t.Fatal(err)
	}
	objs, evs := &fakeObjects{refuse: map[string]int{"raw/stuck": -1}}, &fakeEvents{}
	u := &Uploader{Spool: s, Objects: objs, Events: evs}
	if err := u.Flush(context.Background()); err == nil {
		t.Fatal("the refusal is reported")
	}
	if _, ok := objs.puts["raw/fine"]; !ok || len(evs.tokens) != 1 {
		t.Fatalf("held back by the stuck one: puts %v, inserts %v", objs.puts, evs.tokens)
	}
	if left, _ := s.Ready(); len(left) != 1 || left[0].ObjectKey != "raw/stuck" {
		t.Fatalf("left %+v", left)
	}
}

// A batch goes once the objects its rows name are up, and only then.
func TestUploader_ABatchWaitsForItsObjects(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20)
	writeObject(t, s, "raw/s1-1", 3)
	if err := s.WriteBatch(store.Batch{RawSegments: []store.RawSegmentRow{{SessionID: "s1", Seq: 1, S3Key: "raw/s1-1"}}}); err != nil {
		t.Fatal(err)
	}
	objs, evs := &fakeObjects{refuse: map[string]int{"raw/s1-1": 1}}, &fakeEvents{}
	u := &Uploader{Spool: s, Objects: objs, Events: evs}
	_ = u.Flush(context.Background())
	if len(evs.tokens) != 0 {
		t.Fatal("the batch went before the segment it names")
	}
	if err := u.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := objs.puts["raw/s1-1"]; !ok || len(evs.tokens) != 1 {
		t.Fatalf("after the segment went: puts %v, inserts %v", objs.puts, evs.tokens)
	}
}

// A batch that does not parse is set aside, said in an alert, and the
// queue goes on.
func TestUploader_AnUnreadableBatchIsSetAside(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 1<<20)
	bad := filepath.Join(dir, "ready/batch", ulid.Make().String()+".json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatch(store.Batch{Alerts: []store.AlertRow{{Kind: "fine"}}}); err != nil {
		t.Fatal(err)
	}
	evs := &fakeEvents{}
	u := &Uploader{Spool: s, Objects: &fakeObjects{}, Events: evs}
	if err := u.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = u.Flush(context.Background()) // the alert about it
	if _, err := os.Stat(filepath.Join(dir, "failed", filepath.Base(bad))); err != nil {
		t.Fatalf("not set aside: %v", err)
	}
	if !slices.Contains(evs.kinds, "fine") || !slices.Contains(evs.kinds, "audit_upload_failed") {
		t.Fatalf("alerts inserted %v", evs.kinds)
	}
	if left, _ := s.Ready(); len(left) != 0 {
		t.Fatalf("left %+v", left)
	}
}

// Objects go several at a time.
func TestUploader_ObjectsGoTogether(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20)
	for i := range 8 {
		writeObject(t, s, "raw/k"+strconv.Itoa(i), 2)
	}
	objs := &fakeObjects{delay: 30 * time.Millisecond}
	u := &Uploader{Spool: s, Objects: objs, Events: &fakeEvents{}, Workers: 4}
	if err := u.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(objs.puts) != 8 || objs.most < 2 || objs.most > 4 {
		t.Fatalf("puts %d, at most %d at once (want 2…4)", len(objs.puts), objs.most)
	}
}
