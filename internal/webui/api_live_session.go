// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/mrhaoxx/devpod/internal/audit"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Live session viewing (audit spec §16): the screen is rebuilt from the
// last liveTailBytes of the channel's output — older segments from object
// storage, newer ones from the gateway's disk — and then the gateway's
// live records are relayed as asciinema-player eventsource messages.
const (
	liveTailBytes = 512 << 10
	// segments read forward from the start for the terminal size when
	// the tail has none
	liveSizeScanSegments = 2
)

// LiveSessions wires the relay; nil on Server disables live viewing.
type LiveSessions struct {
	resolve func(ctx context.Context, gatewayPod string) (string, error)
	key     string
	client  *http.Client
	sem     chan struct{}
}

// NewLiveSessions: resolve maps a gateway Pod name to host:port of its
// live endpoint; key is the shared gateway key; at most maxViewers views
// run at once on this replica.
func NewLiveSessions(resolve func(context.Context, string) (string, error), key string, maxViewers int) *LiveSessions {
	return &LiveSessions{resolve: resolve, key: key, client: &http.Client{}, sem: make(chan struct{}, maxViewers)}
}

// liveTail is the bounded screen state: the last max bytes of output and
// the latest terminal size.
type liveTail struct {
	max        int
	buf        []byte
	cols, rows uint32
}

func (t *liveTail) out(p []byte) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
}

func (t *liveTail) bytes() []byte {
	b := t.buf
	if len(b) > t.max {
		b = b[len(b)-t.max:]
	}
	for len(b) > 0 && !utf8.RuneStart(b[0]) { // a cut through a character
		b = b[1:]
	}
	return b
}

// segmentChannel reads one stored segment: the last keep bytes of ch's
// output and the last terminal size it set.
func segmentChannel(ctx context.Context, objs store.ObjectStore, key string, ch uint32, keep int) ([]byte, uint32, uint32, error) {
	rc, err := objs.Get(ctx, key)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rc.Close()
	rd, err := rawlog.OpenSegment(rc)
	if err != nil {
		return nil, 0, 0, err
	}
	t := liveTail{max: keep}
	for {
		rec, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, 0, 0, err
		}
		if rec.Channel != ch {
			continue
		}
		if l, ok := audit.LiveRecordLine(rec); ok {
			if len(l.Out) > 0 {
				t.out(l.Out)
			} else if l.Cols > 0 {
				t.cols, t.rows = l.Cols, l.Rows
			}
		}
	}
	b := t.buf
	if len(b) > keep {
		b = b[len(b)-keep:]
	}
	return b, t.cols, t.rows, nil
}

// tailFromStore fills t from the segments before from, newest first,
// until the tail is full; the size comes from the newest segment that
// set one, else from the first segments.
func (s *Server) tailFromStore(ctx context.Context, d *store.SessionDetail, ch, from uint32, t *liveTail) {
	key := func(seq int) string { return store.RawKey(d.Session.OpenedAt, d.Session.SessionID, uint32(seq)) }
	var chunks [][]byte
	need := t.max
	for seq := int(from) - 1; seq >= 1 && need > 0; seq-- {
		b, c, r, err := segmentChannel(ctx, s.Audit.Objects, key(seq), ch, need)
		if err != nil {
			continue // missing or unreadable: the screen just starts later
		}
		if c > 0 && t.cols == 0 {
			t.cols, t.rows = c, r
		}
		chunks = append(chunks, b)
		need -= len(b)
	}
	for i := len(chunks) - 1; i >= 0; i-- {
		t.out(chunks[i])
	}
	for seq := 1; t.cols == 0 && seq < int(from) && seq <= liveSizeScanSegments; seq++ {
		if _, c, r, err := segmentChannel(ctx, s.Audit.Objects, key(seq), ch, 1); err == nil && c > 0 {
			t.cols, t.rows = c, r
		}
	}
}

// utf8Prefix splits p into its longest prefix of whole characters and
// the rest (an incomplete trailing character, at most 3 bytes).
func utf8Prefix(p []byte) ([]byte, []byte) {
	for i := len(p) - 1; i >= 0 && i >= len(p)-3; i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				return p[:i], p[i:]
			}
			break
		}
	}
	return p, nil
}

// handleAuditLive serves GET /api/audit/sessions/{id}/channels/{ch}/live
// as asciinema-player eventsource messages.
func (s *Server) handleAuditLive(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	ls := s.LiveSessions
	if ls == nil {
		s.writeErr(w, http.StatusNotFound, "LIVE_DISABLED", "live session viewing is not enabled", nil)
		return
	}
	d, ok := s.visibleSession(w, r, sess)
	if !ok {
		return
	}
	ch, err := channelParam(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "invalid channel", nil)
		return
	}
	if d.Session.ClosedAt != nil {
		s.writeErr(w, http.StatusConflict, "SESSION_CLOSED", "the session has ended; open the replay instead", nil)
		return
	}
	select {
	case ls.sem <- struct{}{}:
		defer func() { <-ls.sem }()
	default:
		s.writeErr(w, http.StatusTooManyRequests, "TOO_MANY_VIEWERS", "too many live viewers right now; try again shortly", nil)
		return
	}
	if !s.logAccess(w, r, sess.User, "watch_live", d.Session.SessionID, fmt.Sprintf("channel/%d", ch)) {
		return
	}
	addr, err := ls.resolve(r.Context(), d.Session.GatewayPod)
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "LIVE_UNAVAILABLE", "the session's gateway cannot be reached: "+err.Error(), nil)
		return
	}
	q := url.Values{"session": {d.Session.SessionID}, "channel": {fmt.Sprint(ch)}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+addr+audit.LivePath+"?"+q.Encode(), nil)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	req.Header.Set("Authorization", "Bearer "+ls.key)
	resp, err := ls.client.Do(req)
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "LIVE_UNAVAILABLE", "the session's gateway cannot be reached", nil)
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		s.writeErr(w, http.StatusConflict, "SESSION_CLOSED", "the session has ended; open the replay instead", nil)
		return
	case http.StatusTooManyRequests:
		s.writeErr(w, http.StatusTooManyRequests, "TOO_MANY_VIEWERS", "too many live viewers on the gateway; try again shortly", nil)
		return
	default:
		s.writeErr(w, http.StatusBadGateway, "LIVE_UNAVAILABLE", "gateway: "+resp.Status, nil)
		return
	}
	dec := json.NewDecoder(bufio.NewReaderSize(resp.Body, 64<<10))
	var head audit.LiveLine
	if err := dec.Decode(&head); err != nil || head.From == 0 {
		s.writeErr(w, http.StatusBadGateway, "LIVE_UNAVAILABLE", "bad gateway stream", nil)
		return
	}
	tail := &liveTail{max: liveTailBytes}
	s.tailFromStore(r.Context(), d, ch, head.From, tail)
	closed := false
	for { // the gateway's local backlog, up to the hand-over
		var l audit.LiveLine
		if err := dec.Decode(&l); err != nil {
			s.writeErr(w, http.StatusBadGateway, "LIVE_UNAVAILABLE", "the gateway stream ended early", nil)
			return
		}
		if l.Live {
			break
		}
		switch {
		case len(l.Out) > 0:
			tail.out(l.Out)
		case l.Cols > 0:
			tail.cols, tail.rows = l.Cols, l.Rows
		case l.Closed:
			closed = true
		}
	}

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	emit := func(raw string) bool {
		if _, err := io.WriteString(w, raw); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
	data := func(v any) bool {
		b, err := json.Marshal(v)
		return err == nil && emit("data: "+string(b)+"\n\n")
	}
	cols, rows := tail.cols, tail.rows
	if cols == 0 {
		cols, rows = 80, 24
	}
	if !data(map[string]any{"cols": cols, "rows": rows, "time": 0, "init": string(tail.bytes())}) {
		return
	}
	tail.buf = nil // the relay keeps no history
	if closed {
		emit("event: done\ndata: {}\n\n")
		return
	}
	start := time.Now()
	var carry []byte
	for {
		var l audit.LiveLine
		if err := dec.Decode(&l); err != nil {
			if r.Context().Err() == nil {
				data(map[string]string{"state": "offline"})
			}
			return
		}
		t := time.Since(start).Seconds()
		switch {
		case len(l.Out) > 0:
			text, rest := utf8Prefix(append(carry, l.Out...))
			carry = append(carry[:0], rest...)
			if len(text) > 0 && !data([]any{t, "o", string(text)}) {
				return
			}
		case l.Cols > 0:
			// the eventsource driver passes resize data to the terminal
			// untouched, so it must already be {cols, rows}
			if !data([]any{t, "r", map[string]uint32{"cols": l.Cols, "rows": l.Rows}}) {
				return
			}
		case l.HB:
			if !emit(": hb\n\n") {
				return
			}
		case l.Closed:
			emit("event: done\ndata: {}\n\n")
			return
		case l.Error != "":
			data(map[string]string{"state": "offline"})
			return
		}
	}
}
