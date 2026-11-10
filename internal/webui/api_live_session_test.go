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

// streamLines reads n NDJSON lines (or fewer if the stream ends).
func streamLines(t *testing.T, body *bufio.Scanner, n int) []string {
	t.Helper()
	var out []string
	deadline := time.After(5 * time.Second)
	got := make(chan string)
	go func() {
		for body.Scan() {
			got <- body.Text()
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
	ms := func(d time.Duration) int64 { return t0.Add(d).UnixMilli() }
	open1 := rawlog.Record{TS: t0, Channel: 1, Kind: rawlog.KindChannelOpen, Payload: rawlog.Encode(rawlog.ChannelOpen{Type: "session"})}
	liveRig := func(t *testing.T, gwLines []string, hold chan struct{}, closed bool) (*webui.Server, *webui.SessionManager, *fakeAuditStore, *[]string, *[]string) {
		s, sm := newServer(t)
		seg1 := segment(t,
			open1,
			rawlog.Record{TS: t0, Channel: 1, Kind: rawlog.KindRequest, Payload: ptyReq(100, 30)},
			rawlog.Record{TS: t0.Add(time.Second), Channel: 1, Kind: rawlog.KindDataOut, Payload: []byte("old-1 ")},
			rawlog.Record{TS: t0.Add(time.Second), Channel: 2, Kind: rawlog.KindDataOut, Payload: []byte("not this channel ")},
		)
		row := store.SessionRow{SessionID: "S1", User: "gl-bob", OpenedAt: t0, GatewayPod: "gw-1"}
		if closed {
			end := t0.Add(time.Hour)
			row.ClosedAt = &end
		}
		st := &fakeAuditStore{sessions: map[string]*store.SessionDetail{"S1": {
			Session:  row,
			Channels: []store.ChannelRow{{SessionID: "S1", ChannelID: 1, Type: "session", Pty: true, OpenedAt: t0}},
			Segments: []store.RawSegmentRow{{SessionID: "S1", Seq: 1, S3Key: store.RawKey(t0, "S1", 1), FirstTS: t0, LastTS: t0.Add(time.Second), Bytes: 100}},
		}}, presence: map[string][]string{}}
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

	t.Run("history then live on one clock", func(t *testing.T) {
		s, sm, st, seen, resolved := liveRig(t, []string{
			`{"from":2,"cur":2}`, fmt.Sprintf(`{"t":%d,"o":"YmFja2xvZyA="}`, ms(2*time.Second)), `{"live":true}`,
			fmt.Sprintf(`{"t":%d,"o":"bGl2ZSE="}`, ms(3*time.Second)), fmt.Sprintf(`{"t":%d,"c":120,"r":40}`, ms(4*time.Second)), `{"closed":true}`,
		}, nil, false)
		resp := open(t, s, forge(sm, "gl-bob", false))
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		lines := streamLines(t, bufio.NewScanner(resp.Body), 9)
		var head struct {
			Version       int
			Width, Height uint32
			Timestamp     int64
			Start         float64
			Now           float64 // seconds since the channel opened
		}
		if err := json.Unmarshal([]byte(lines[0]), &head); err != nil || head.Version != 2 || head.Timestamp != t0.Unix() || head.Start != 0 || head.Now < 4 {
			t.Fatalf("header %q %+v %v", lines[0], head, err)
		}
		// times are seconds since the channel opened, for the stored
		// history, the gateway's backlog and live records alike
		want := []string{
			`[0,"r","100x30"]`, `[1,"o","old-1 "]`, `[2,"o","backlog "]`, `{"live":true}`,
			`[3,"o","live!"]`, `[4,"r","120x40"]`, `{"done":true}`,
		}
		if got := lines[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("stream\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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
	t.Run("history past the budget starts later", func(t *testing.T) {
		s, sm, st, _, _ := liveRig(t, []string{`{"from":3,"cur":3}`, `{"live":true}`, `{"closed":true}`}, nil, false)
		seg2 := segment(t, rawlog.Record{TS: t0.Add(10 * time.Second), Channel: 1, Kind: rawlog.KindDataOut, Payload: []byte("newer")})
		s.Audit.Objects.(fakeObjects)[store.RawKey(t0, "S1", 2)] = seg2
		d := st.sessions["S1"]
		d.Segments = append(d.Segments, store.RawSegmentRow{SessionID: "S1", Seq: 2, S3Key: store.RawKey(t0, "S1", 2), FirstTS: t0.Add(10 * time.Second), Bytes: 100})
		defer webui.SetLiveHistoryBytesForTest(150)()
		lines := streamLines(t, bufio.NewScanner(open(t, s, forge(sm, "gl-bob", false)).Body), 5)
		var head struct{ Start float64 }
		_ = json.Unmarshal([]byte(lines[0]), &head)
		// segment 1 is past the budget: the stream starts at segment 2,
		// with the size segment 1 set
		want := []string{`[10,"r","100x30"]`, `[10,"o","newer"]`, `{"live":true}`, `{"done":true}`}
		if head.Start != 10 || strings.Join(lines[1:], "\n") != strings.Join(want, "\n") {
			t.Fatalf("header %s stream\n%s", lines[0], strings.Join(lines[1:], "\n"))
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
		streamLines(t, bufio.NewScanner(first.Body), 2) // the first viewer is streaming
		if resp := open(t, s, forge(sm, "gl-bob", false)); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}
