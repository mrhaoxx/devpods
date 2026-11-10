// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func ptyReq(cols, rows uint32) []byte {
	return rawlog.Encode(rawlog.Request{Type: "pty-req", OK: true, Payload: ssh.Marshal(struct {
		Term             string
		Cols, Rows, W, H uint32
		Modes            string
	}{"xterm", cols, rows, 0, 0, ""})})
}

// fakeGateway plays the gateway's /internal/live for session S1.
func fakeGateway(t *testing.T, lines []string, hold chan struct{}) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, l := range lines {
			fmt.Fprintln(w, l)
			w.(http.Flusher).Flush()
		}
		if hold != nil {
			<-hold
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func sseEvents(t *testing.T, body *bufio.Scanner, n int) []string {
	t.Helper()
	var out []string
	deadline := time.After(5 * time.Second)
	got := make(chan string)
	go func() {
		for body.Scan() {
			l := body.Text()
			if strings.HasPrefix(l, "data: ") || strings.HasPrefix(l, "event: ") {
				got <- l
			}
		}
		close(got)
	}()
	for len(out) < n {
		select {
		case l, ok := <-got:
			if !ok {
				return out
			}
			out = append(out, l)
		case <-deadline:
			t.Fatalf("timed out after %v", out)
		}
	}
	return out
}

func TestLiveSessionAPI(t *testing.T) {
	setupSuite(t)
	t0 := time.Unix(1794210460, 0).UTC()
	liveRig := func(t *testing.T, gwLines []string, hold chan struct{}, closed bool) (*webui.Server, *webui.SessionManager, *fakeAuditStore, *[]string, *[]string) {
		s, sm := newServer(t)
		seg1 := segment(t,
			rawlog.Record{TS: t0, Channel: 1, Kind: rawlog.KindChannelOpen, Payload: rawlog.Encode(rawlog.ChannelOpen{Type: "session"})},
			rawlog.Record{TS: t0, Channel: 1, Kind: rawlog.KindRequest, Payload: ptyReq(100, 30)},
			rawlog.Record{TS: t0.Add(time.Second), Channel: 1, Kind: rawlog.KindDataOut, Payload: []byte("old-1 ")},
			rawlog.Record{TS: t0.Add(time.Second), Channel: 2, Kind: rawlog.KindDataOut, Payload: []byte("not this channel ")},
		)
		row := store.SessionRow{SessionID: "S1", User: "gl-bob", OpenedAt: t0, GatewayPod: "gw-1"}
		if closed {
			end := t0.Add(time.Hour)
			row.ClosedAt = &end
		}
		st := &fakeAuditStore{sessions: map[string]*store.SessionDetail{"S1": {Session: row}}, presence: map[string][]string{}}
		s.Audit = &webui.AuditDeps{Store: st, Objects: fakeObjects{store.RawKey(t0, "S1", 1): seg1}}
		gw, seen := fakeGateway(t, gwLines, hold)
		var resolved []string
		s.LiveSessions = webui.NewLiveSessions(func(_ context.Context, pod string) (string, error) {
			resolved = append(resolved, pod)
			return strings.TrimPrefix(gw.URL, "http://"), nil
		}, "k3y", 1)
		return s, sm, st, seen, &resolved
	}
	open := func(t *testing.T, s *webui.Server, c *http.Cookie) *http.Response {
		t.Helper()
		srv := httptest.NewServer(s.Routes())
		t.Cleanup(srv.Close)
		req, _ := http.NewRequest("GET", srv.URL+"/api/audit/sessions/S1/channels/1/live", nil)
		req.AddCookie(c)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	t.Run("catch-up then live", func(t *testing.T) {
		s, sm, st, seen, resolved := liveRig(t, []string{
			`{"from":2,"cur":2}`, `{"t":1,"o":"YmFja2xvZyA="}`, `{"live":true}`,
			`{"t":2,"o":"bGl2ZSE="}`, `{"t":3,"c":120,"r":40}`, `{"closed":true}`,
		}, nil, false)
		resp := open(t, s, forge(sm, "gl-bob", false))
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		ev := sseEvents(t, bufio.NewScanner(resp.Body), 4)
		var reset struct {
			Cols, Rows int
			Init       string
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ev[0], "data: ")), &reset); err != nil {
			t.Fatalf("reset %q: %v", ev[0], err)
		}
		if reset.Cols != 100 || reset.Rows != 30 || reset.Init != "old-1 backlog " {
			t.Fatalf("reset %+v", reset)
		}
		var o, rz []any
		_ = json.Unmarshal([]byte(strings.TrimPrefix(ev[1], "data: ")), &o)
		_ = json.Unmarshal([]byte(strings.TrimPrefix(ev[2], "data: ")), &rz)
		// the eventsource driver hands resize data to the terminal as is:
		// it must be {cols, rows}, not the asciicast "120x40" string
		size, _ := rz[2].(map[string]any)
		if len(o) != 3 || o[1] != "o" || o[2] != "live!" || len(rz) != 3 || rz[1] != "r" || size["cols"] != 120.0 || size["rows"] != 40.0 {
			t.Fatalf("events %v %v", o, rz)
		}
		if ev[3] != "event: done" {
			t.Fatalf("end %q", ev[3])
		}
		if len(*resolved) != 1 || (*resolved)[0] != "gw-1" {
			t.Fatalf("resolved %v", *resolved)
		}
		if len(*seen) != 1 || !strings.Contains((*seen)[0], "session=S1") || !strings.Contains((*seen)[0], "channel=1") || !strings.HasSuffix((*seen)[0], "Bearer k3y") {
			t.Fatalf("gateway saw %v", *seen)
		}
		if acts := st.actions(); len(acts) != 1 || acts[0] != "watch_live" {
			t.Fatalf("access log %v", acts)
		}
	})
	t.Run("ended session is 409", func(t *testing.T) {
		s, sm, _, _, _ := liveRig(t, nil, nil, true)
		if resp := open(t, s, forge(sm, "gl-bob", false)); resp.StatusCode != http.StatusConflict {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("someone else's session is 404", func(t *testing.T) {
		s, sm, _, _, _ := liveRig(t, nil, nil, false)
		if resp := open(t, s, forge(sm, "gl-alice", false)); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("disabled is 404", func(t *testing.T) {
		s, sm, _, _, _ := liveRig(t, nil, nil, false)
		s.LiveSessions = nil
		if resp := open(t, s, forge(sm, "gl-bob", false)); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("viewer cap is 429", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		s, sm, _, _, _ := liveRig(t, []string{`{"from":1,"cur":1}`, `{"live":true}`}, hold, false)
		first := open(t, s, forge(sm, "gl-bob", false))
		sseEvents(t, bufio.NewScanner(first.Body), 1) // the first viewer is streaming
		if resp := open(t, s, forge(sm, "gl-bob", false)); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}
