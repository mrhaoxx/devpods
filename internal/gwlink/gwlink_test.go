// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gwlink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/gwlink"
)

func identity(t *testing.T) *agent.Identity {
	t.Helper()
	id, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSignedRequests(t *testing.T) {
	gw, other := identity(t), identity(t)
	keys := func(name string) (string, error) {
		switch name {
		case "b300":
			return gw.PublicKey(), nil
		case "mulan":
			return other.PublicKey(), nil
		}
		return "", errors.New("no such gateway")
	}
	now := time.Unix(1_800_000_000, 0)
	mk := func(method, target string, body []byte) *http.Request {
		r := httptest.NewRequest(method, target, bytes.NewReader(body))
		gwlink.Sign(r, body, "b300", gw, now)
		return r
	}
	body := []byte(`{"user":"alice","pod":"x"}`)

	r := mk("POST", "/gateway/v1/lookup", body)
	if name, err := gwlink.Verify(r, body, keys, now.Add(20*time.Second)); err != nil || name != "b300" {
		t.Fatalf("a good request: %q %v", name, err)
	}
	// anything changed after signing
	for what, change := range map[string]func(*http.Request) []byte{
		"the path":     func(r *http.Request) []byte { r.URL.Path = "/gateway/v1/keys"; return body },
		"the query":    func(r *http.Request) []byte { r.URL.RawQuery = "user=root"; return body },
		"the method":   func(r *http.Request) []byte { r.Method = "GET"; return body },
		"the body":     func(*http.Request) []byte { return []byte(`{"user":"root","pod":"x"}`) },
		"the name":     func(r *http.Request) []byte { r.Header.Set(gwlink.HeaderGateway, "mulan"); return body },
		"the date":     func(r *http.Request) []byte { r.Header.Set(gwlink.HeaderDate, "1800000030"); return body },
		"no signature": func(r *http.Request) []byte { r.Header.Del(gwlink.HeaderSignature); return body },
	} {
		r := mk("POST", "/gateway/v1/lookup", body)
		b := change(r)
		if _, err := gwlink.Verify(r, b, keys, now); err == nil {
			t.Errorf("accepted with %s changed", what)
		}
	}
	// a request kept and sent again later
	if _, err := gwlink.Verify(mk("POST", "/gateway/v1/lookup", body), body, keys, now.Add(2*time.Minute)); err == nil {
		t.Error("a two-minute-old request accepted")
	}
	// a gateway nobody registered
	r = httptest.NewRequest("GET", "/gateway/v1/clusters", nil)
	gwlink.Sign(r, nil, "ghost", gw, now)
	if _, err := gwlink.Verify(r, nil, keys, now); err == nil {
		t.Error("an unregistered gateway accepted")
	}
}

// console is a console that answers lookups and counts them.
type console struct {
	t     *testing.T
	gw    *agent.Identity
	calls atomic.Int64
	down  atomic.Bool
	keys  atomic.Value // []string
	clock func() time.Time
}

func (c *console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if _, err := gwlink.Verify(r, body, func(string) (string, error) { return c.gw.PublicKey(), nil }, c.clock()); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	c.calls.Add(1)
	if c.down.Load() {
		http.Error(w, "the console is restarting", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/gateway/v1/keys":
		if r.URL.Query().Get("user") == "nobody" {
			http.Error(w, `{"code":"NOT_FOUND"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(gwlink.Keys{Keys: c.keys.Load().([]string)})
	default:
		http.NotFound(w, r)
	}
}

func TestClient_CachesBrieflyAndOutlastsAShortOutage(t *testing.T) {
	gw := identity(t)
	con := &console{t: t, gw: gw}
	con.keys.Store([]string{"ssh-ed25519 AAAA one"})
	srv := httptest.NewServer(con)
	defer srv.Close()
	now := time.Now()
	clock := func() time.Time { return now }
	con.clock = clock // the two ends agree on the time
	c := gwlink.NewClient(gwlink.Options{Console: srv.URL, Gateway: "b300", Identity: gw, Now: clock, TTL: 5 * time.Second, Grace: 60 * time.Second})
	ctx := context.Background()

	keys, stale, err := c.Keys(ctx, "alice")
	if err != nil || stale || len(keys) != 1 {
		t.Fatalf("first: %v %v %v", keys, stale, err)
	}
	// within the cache time the console is not asked again
	con.keys.Store([]string{"ssh-ed25519 AAAA two"})
	if keys, _, _ := c.Keys(ctx, "alice"); keys[0] != "ssh-ed25519 AAAA one" || con.calls.Load() != 1 {
		t.Fatalf("within 5 s: %v after %d calls", keys, con.calls.Load())
	}
	// after it, it is: a withdrawn key stops working
	now = now.Add(6 * time.Second)
	if keys, stale, err := c.Keys(ctx, "alice"); err != nil || stale || keys[0] != "ssh-ed25519 AAAA two" || con.calls.Load() != 2 {
		t.Fatalf("after 5 s: %v %v %v after %d calls", keys, stale, err, con.calls.Load())
	}
	// "no such user" is an answer, cached like one
	if _, _, err := c.Keys(ctx, "nobody"); !errors.Is(err, gwlink.ErrNotFound) {
		t.Fatalf("an unknown user: %v", err)
	}
	// the console goes away: the last answer serves for a while, and says so
	con.down.Store(true)
	now = now.Add(30 * time.Second)
	if keys, stale, err := c.Keys(ctx, "alice"); err != nil || !stale || keys[0] != "ssh-ed25519 AAAA two" {
		t.Fatalf("console away for 30 s: %v %v %v", keys, stale, err)
	}
	// someone never asked about has no last answer
	if _, _, err := c.Keys(ctx, "carol"); !errors.Is(err, gwlink.ErrConsoleAway) {
		t.Fatalf("a new question while the console is away: %v", err)
	}
	// and not forever
	now = now.Add(60 * time.Second)
	if _, _, err := c.Keys(ctx, "alice"); !errors.Is(err, gwlink.ErrConsoleAway) {
		t.Fatalf("console away past the grace: %v", err)
	}
	// back again
	con.down.Store(false)
	if keys, stale, err := c.Keys(ctx, "alice"); err != nil || stale || len(keys) != 1 {
		t.Fatalf("console back: %v %v %v", keys, stale, err)
	}
}

// A console that does not know of gateways yet (an earlier version)
// answers its page, 200, to any path: that is not a join.
func TestJoin_OnlyAJoinIsAJoin(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "<!doctype html><title>console</title>")
	}))
	defer srv.Close()
	id := identity(t)
	if err := gwlink.Join(context.Background(), nil, srv.URL, "b300", "dpj_x", id); err == nil {
		t.Fatal("a page answered with 200 was taken for a join")
	}
	status = http.StatusForbidden
	if err := gwlink.Join(context.Background(), nil, srv.URL, "b300", "dpj_x", id); err == nil {
		t.Fatal("a refusal was taken for a join")
	}
	status = http.StatusNoContent
	if err := gwlink.Join(context.Background(), nil, srv.URL, "b300", "dpj_x", id); err != nil {
		t.Fatalf("a join: %v", err)
	}
}
