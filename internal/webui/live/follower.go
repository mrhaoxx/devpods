// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Internal endpoints on the leader's :8081 listener (spec §5.10).
const (
	LivePath   = "/internal/live"
	DevPodPath = "/internal/live/devpod"
)

// idleTimeout: the leader sends a snapshot every second; a link silent
// this long is dead.
const idleTimeout = 5 * time.Second

func authorized(r *http.Request, token string) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// LiveHandler serves GET /internal/live: an SSE stream of the leader's
// snapshots for followers.
func LiveHandler(h *Hub, token string, isLeader func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !isLeader() {
			http.Error(w, "not the leader", http.StatusServiceUnavailable)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		ch, cancel := h.Subscribe()
		defer cancel()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		check := time.NewTicker(time.Second)
		defer check.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-check.C:
				if !isLeader() {
					return // followers re-resolve the leader
				}
			case s := <-ch:
				b, err := json.Marshal(s)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
					return
				}
				fl.Flush()
			}
		}
	})
}

// Follow relays the leader's snapshots into h until ctx ends,
// re-resolving the leader and reconnecting with backoff 1 s → 5 s.
// While disconnected it publishes nothing and h reports !Connected.
func Follow(ctx context.Context, addr func() (string, error), token string, h *Hub) {
	backoff := time.Second
	for ctx.Err() == nil {
		got := followOnce(ctx, addr, token, h)
		h.SetConnected(false)
		if got {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
}

// followOnce runs one connection; it reports whether any snapshot came.
func followOnce(ctx context.Context, addr func() (string, error), token string, h *Hub) bool {
	a, err := addr()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a+LivePath, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	// Watchdog: cancel the request when the leader goes quiet.
	alive := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(idleTimeout)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-alive:
				t.Reset(idleTimeout)
			case <-t.C:
				cancel()
				return
			}
		}
	}()
	got := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var s Snapshot
		if err := json.Unmarshal(line[len("data: "):], &s); err != nil {
			continue
		}
		select {
		case alive <- struct{}{}:
		default:
		}
		got = true
		h.SetConnected(true)
		h.Publish(s)
	}
	return got
}

// DevPodFrame is one DevPod's per-second detail: the full latest sample
// and its processes.
type DevPodFrame struct {
	Metrics   *DevPodSample `json:"metrics,omitempty"`
	Processes []ProcLive    `json:"processes"`
}

// Source yields DevPod frames: the leader's own state, or the leader
// over HTTP on followers.
type Source interface {
	DevPod(ctx context.Context, uid string, allProcs bool) (DevPodFrame, error)
}

// LocalSource reads the leader's state.
type LocalSource struct{ State *State }

func (l LocalSource) DevPod(_ context.Context, uid string, all bool) (DevPodFrame, error) {
	f := DevPodFrame{Processes: l.State.Processes(uid, all)}
	if s, ok := l.State.Sample(uid); ok {
		f.Metrics = &s
	}
	return f, nil
}

// DevPodHandler serves GET /internal/live/devpod?uid=&procs=all.
func DevPodHandler(st *State, token string, isLeader func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !isLeader() {
			http.Error(w, "not the leader", http.StatusServiceUnavailable)
			return
		}
		f, _ := LocalSource{State: st}.DevPod(r.Context(), r.URL.Query().Get("uid"), r.URL.Query().Get("procs") == "all")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f)
	})
}

// RemoteSource asks the leader.
type RemoteSource struct {
	Addr   func() (string, error)
	Token  string
	Client *http.Client
}

func (rs *RemoteSource) DevPod(ctx context.Context, uid string, all bool) (DevPodFrame, error) {
	a, err := rs.Addr()
	if err != nil {
		return DevPodFrame{}, err
	}
	q := url.Values{"uid": {uid}}
	if all {
		q.Set("procs", "all")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a+DevPodPath+"?"+q.Encode(), nil)
	if err != nil {
		return DevPodFrame{}, err
	}
	req.Header.Set("Authorization", "Bearer "+rs.Token)
	c := rs.Client
	if c == nil {
		c = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return DevPodFrame{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return DevPodFrame{}, errors.New("leader: " + resp.Status)
	}
	var f DevPodFrame
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&f)
	return f, err
}

// SwitchSource reads locally while leading, remotely otherwise.
type SwitchSource struct {
	IsLeader func() bool
	Local    Source
	Remote   Source
}

func (s SwitchSource) DevPod(ctx context.Context, uid string, all bool) (DevPodFrame, error) {
	if s.IsLeader() {
		return s.Local.DevPod(ctx, uid, all)
	}
	return s.Remote.DevPod(ctx, uid, all)
}
