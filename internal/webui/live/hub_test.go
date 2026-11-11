// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHub_NoWorkWithoutSubscribers(t *testing.T) {
	h := NewHub()
	var builds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Tick(ctx, h, 20*time.Millisecond, func() Snapshot {
		builds.Add(1)
		return Snapshot{TS: time.Now()}
	})
	time.Sleep(150 * time.Millisecond)
	if n := builds.Load(); n != 0 {
		t.Fatalf("built %d snapshots with nobody listening", n)
	}
	ch, unsub := h.Subscribe()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber got nothing")
	}
	unsub()
	time.Sleep(60 * time.Millisecond)
	n := builds.Load()
	time.Sleep(150 * time.Millisecond)
	if builds.Load() != n {
		t.Fatal("kept building after the last subscriber left")
	}
}

func TestFollower_RelaysAndReconnects(t *testing.T) {
	const token = "tok"
	var served atomic.Bool
	dying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || served.Swap(true) {
			http.Error(w, "gone", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 1; i <= 3; i++ {
			b, _ := json.Marshal(Snapshot{TS: time.Unix(int64(i), 0).UTC()})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond) // a real leader sends one per second
		}
	}))
	defer dying.Close()

	leaderHub := NewHub()
	next := httptest.NewServer(LiveHandler(leaderHub, token, func() bool { return true }))
	defer next.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Tick(ctx, leaderHub, 50*time.Millisecond, func() Snapshot { return Snapshot{TS: time.Unix(100, 0).UTC()} })

	var addr atomic.Value
	addr.Store(strings.TrimPrefix(dying.URL, "http://"))
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()
	go Follow(ctx, func() (string, error) { return addr.Load().(string), nil }, token, h)

	for i := 1; i <= 3; i++ {
		select {
		case s := <-ch:
			if s.TS.Unix() != int64(i) {
				t.Fatalf("snapshot %d has ts %v", i, s.TS)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("snapshot %d never relayed", i)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.Connected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if h.Connected() {
		t.Fatal("follower still reports connected after the leader went away")
	}
	addr.Store(strings.TrimPrefix(next.URL, "http://"))
	select {
	case s := <-ch:
		if s.TS.Unix() != 100 || !h.Connected() {
			t.Fatalf("after reconnect: %v connected=%v", s.TS, h.Connected())
		}
	case <-time.After(8 * time.Second):
		t.Fatal("follower never reconnected to the new leader")
	}
}

func TestRemoteSource_DevPodFrame(t *testing.T) {
	now := time.Unix(1000, 0)
	st := NewState(clock(&now))
	st.Apply("u1", nil)
	srv := httptest.NewServer(DevPodHandler(st, "tok", func() bool { return true }))
	defer srv.Close()
	src := &RemoteSource{Addr: func() (string, error) { return strings.TrimPrefix(srv.URL, "http://"), nil }, Token: "tok"}
	if _, err := src.DevPod(context.Background(), "u1", true); err != nil {
		t.Fatal(err)
	}
	bad := &RemoteSource{Addr: src.Addr, Token: "wrong"}
	if _, err := bad.DevPod(context.Background(), "u1", true); err == nil {
		t.Fatal("wrong token accepted")
	}
}

// A follower that becomes the leader stops following while its own Tick
// has already started: the follower loop winding down must not leave the
// new leader looking disconnected (the "实时数据重连中…" bar while data
// flows).
func TestFollowerToLeader_StaysConnected(t *testing.T) {
	const token = "tok"
	old := NewHub()
	oldLeader := httptest.NewServer(LiveHandler(old, token, func() bool { return true }))
	defer oldLeader.Close()
	tctx, stopOld := context.WithCancel(context.Background())
	defer stopOld()
	go Tick(tctx, old, 20*time.Millisecond, func() Snapshot { return Snapshot{TS: time.Unix(1, 0).UTC()} })

	h := NewHub()
	h.SetConnected(false)
	ch, unsub := h.Subscribe()
	defer unsub()
	fctx, stopFollowing := context.WithCancel(context.Background())
	followed := make(chan struct{})
	go func() {
		Follow(fctx, func() (string, error) { return strings.TrimPrefix(oldLeader.URL, "http://"), nil }, token, h)
		close(followed)
	}()
	select {
	case <-ch: // following works
	case <-time.After(5 * time.Second):
		t.Fatal("the follower never got a snapshot")
	}

	// leadership arrives: the leader's Tick starts, then the follower loop is told to stop
	lctx, stopLeading := context.WithCancel(context.Background())
	defer stopLeading()
	go Tick(lctx, h, 20*time.Millisecond, func() Snapshot { return Snapshot{TS: time.Unix(2, 0).UTC()} })
	time.Sleep(50 * time.Millisecond)
	stopFollowing()
	<-followed
	time.Sleep(100 * time.Millisecond)
	if !h.Connected() {
		t.Fatal("the new leader reports disconnected after it stopped following")
	}
	// and a hub marked disconnected by anything recovers while it leads
	h.SetConnected(false)
	time.Sleep(100 * time.Millisecond)
	if !h.Connected() {
		t.Fatal("a leading hub stayed disconnected")
	}
}
