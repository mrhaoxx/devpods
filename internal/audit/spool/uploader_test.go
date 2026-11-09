// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package spool

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

type fakeObjects struct {
	mu    sync.Mutex
	puts  map[string][]byte
	fails int
}

func (f *fakeObjects) Put(_ context.Context, key string, body io.Reader, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
}

func (f *fakeEvents) Insert(_ context.Context, b store.Batch, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	f.alerts += len(b.Alerts)
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
	if len(attempts) != 1 || attempts[0] != 1 {
		t.Fatalf("backoff attempts %v, want [1] (one Flush failure + one Run failure → second is attempt 1 in Run)", attempts)
	}
}
