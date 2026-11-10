// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
)

func liveGet(t *testing.T, srv *httptest.Server, key, q string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/internal/live?"+q, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLiveHandler(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	srv := httptest.NewServer(LiveHandler(g.r, "secret", 1))
	defer srv.Close()
	q := "session=" + s.meta.ID + "&channel=1"

	if resp := liveGet(t, srv, "wrong", q); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}
	if resp := liveGet(t, srv, "secret", "session=nope&channel=1"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d", resp.StatusCode)
	}

	out(s, 1, "a")
	resp := liveGet(t, srv, "secret", q)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if busy := liveGet(t, srv, "secret", q); busy.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second viewer over the cap: %d", busy.StatusCode)
	}
	lines := make(chan LiveLine, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var l LiveLine
			if json.Unmarshal(sc.Bytes(), &l) == nil {
				lines <- l
			}
		}
		close(lines)
	}()
	recv := func() LiveLine {
		t.Helper()
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended")
			}
			return l
		case <-time.After(3 * time.Second):
			t.Fatal("timed out")
		}
		return LiveLine{}
	}
	if h := recv(); h.From != 1 || h.Cur != 1 {
		t.Fatalf("header %+v", h)
	}
	if l := recv(); string(l.Out) != "a" {
		t.Fatalf("backlog %+v", l)
	}
	if l := recv(); !l.Live {
		t.Fatalf("hand-over marker %+v", l)
	}
	out(s, 1, "b")
	s.record(1, rawlog.KindRequest, rawlog.Encode(rawlog.Request{Type: "window-change", OK: true, Payload: ssh.Marshal(struct{ C, R, W, H uint32 }{132, 43, 0, 0})}))
	s.record(1, rawlog.KindChannelClose, rawlog.Encode(rawlog.ChannelClose{}))
	if l := recv(); string(l.Out) != "b" || l.T == 0 {
		t.Fatalf("live output %+v", l)
	}
	if l := recv(); l.Cols != 132 || l.Rows != 43 {
		t.Fatalf("resize %+v", l)
	}
	if l := recv(); !l.Closed {
		t.Fatalf("close %+v", l)
	}
}

func TestKillHandler(t *testing.T) {
	g := newRig(t, 1<<20)
	s := g.start(policy.Effective{})
	h := KillHandler(g.r, "k3y")
	do := func(method, id, key string) int {
		req := httptest.NewRequest(method, KillPath+"?session="+id, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if c := do(http.MethodPost, s.meta.ID, ""); c != http.StatusUnauthorized {
		t.Fatalf("no key: %d", c)
	}
	if c := do(http.MethodGet, s.meta.ID, "k3y"); c != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", c)
	}
	if c := do(http.MethodPost, "nope", "k3y"); c != http.StatusNotFound {
		t.Fatalf("unknown session: %d", c)
	}
	select {
	case <-s.Done():
		t.Fatal("killed too early")
	default:
	}
	if c := do(http.MethodPost, s.meta.ID, "k3y"); c != http.StatusNoContent {
		t.Fatalf("kill: %d", c)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("session not torn down")
	}
	if s.KillReason() != "admin_kill" {
		t.Fatalf("reason %q", s.KillReason())
	}
}
