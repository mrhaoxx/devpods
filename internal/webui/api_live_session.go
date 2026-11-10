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
	"sort"
	"time"
	"unicode/utf8"

	"github.com/mrhaoxx/devpod/internal/audit"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Live session viewing (audit spec §16): one NDJSON stream per channel,
// shaped like asciicast v2 so the browser's player reads it like a
// recording — a header, the channel's history (object storage, then the
// gateway's local backlog), {"live":true} at the hand-over, the live
// records, {"done":true} when the channel ends. Every time is seconds
// since the channel opened, so the history, the live edge and the
// session timeline share one clock. Nothing is held beyond one record.
var liveHistoryBytes int64 = 64 << 20 // newest raw segments streamed, by uncompressed size

// segments read from the start for the terminal size when the budget cut
// off the history that set it
const liveSizeScanSegments = 2

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

// castStream writes the NDJSON stream; output is cut at whole characters
// (a split one waits for the next record).
type castStream struct {
	w     *bufio.Writer
	fl    http.Flusher
	zero  int64 // channel open, unix ms
	carry []byte
	err   error
}

func (c *castStream) line(v any) bool {
	if c.err != nil {
		return false
	}
	b, err := json.Marshal(v)
	if err == nil {
		_, err = c.w.Write(append(b, '\n'))
	}
	c.err = err
	return err == nil
}

func (c *castStream) flush() bool {
	if c.err == nil {
		c.err = c.w.Flush()
		if c.err == nil && c.fl != nil {
			c.fl.Flush()
		}
	}
	return c.err == nil
}

func (c *castStream) at(ms int64) float64 {
	if ms < c.zero {
		return 0
	}
	return float64(ms-c.zero) / 1000
}

func (c *castStream) size(ms int64, cols, rows uint32) bool {
	return c.line([]any{c.at(ms), "r", fmt.Sprintf("%dx%d", cols, rows)})
}

// add writes one gateway-shaped line (output or size); others are ignored.
func (c *castStream) add(l audit.LiveLine) bool {
	switch {
	case len(l.Out) > 0:
		text, rest := utf8Prefix(append(c.carry, l.Out...))
		c.carry = append(c.carry[:0], rest...)
		if len(text) > 0 {
			return c.line([]any{c.at(l.T), "o", string(text)})
		}
	case l.Cols > 0:
		return c.size(l.T, l.Cols, l.Rows)
	}
	return c.err == nil
}

// eachChannelLine reads one stored segment and hands fn ch's records.
func eachChannelLine(ctx context.Context, objs store.ObjectStore, key string, ch uint32, fn func(audit.LiveLine) bool) error {
	rc, err := objs.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	rd, err := rawlog.OpenSegment(rc)
	if err != nil {
		return err
	}
	for {
		rec, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if rec.Channel != ch {
			continue
		}
		if l, ok := audit.LiveRecordLine(rec); ok && !fn(l) {
			return nil
		}
	}
}

// liveHistory plans the stored history before the gateway's first local
// segment: the newest segments within liveHistoryBytes, their object
// keys, and where on the channel's clock they start (zero when nothing
// was cut).
func liveHistory(d *store.SessionDetail, from uint32) (first uint32, key func(uint32) string, start time.Time) {
	rows := map[uint32]store.RawSegmentRow{}
	var seqs []uint32
	for _, r := range d.Segments {
		if r.Seq >= 1 && r.Seq < from {
			rows[r.Seq] = r
			seqs = append(seqs, r.Seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] })
	first = 1
	var sum int64
	for _, q := range seqs {
		if sum += rows[q].Bytes; sum > liveHistoryBytes {
			first = q + 1
			break
		}
	}
	if first > 1 {
		if r, ok := rows[first]; ok {
			start = r.FirstTS
		}
	}
	key = func(seq uint32) string {
		if r, ok := rows[seq]; ok && r.S3Key != "" {
			return r.S3Key
		}
		return store.RawKey(d.Session.OpenedAt, d.Session.SessionID, seq)
	}
	return first, key, start
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
	zero := d.Session.OpenedAt
	for _, c := range d.Channels {
		if c.ChannelID == ch {
			zero = c.OpenedAt
		}
	}
	first, key, start := liveHistory(d, head.From)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	cs := &castStream{w: bufio.NewWriterSize(w, 32<<10), fl: fl, zero: zero.UnixMilli()}
	startAt := 0.0
	if !start.IsZero() {
		startAt = cs.at(start.UnixMilli())
	}
	if !cs.line(struct {
		Version   int     `json:"version"`
		Width     uint32  `json:"width"`
		Height    uint32  `json:"height"`
		Timestamp int64   `json:"timestamp"`
		Start     float64 `json:"start"` // earliest moment the stream holds
		Now       float64 `json:"now"`   // the server's now, on the channel clock
	}{2, 80, 24, zero.Unix(), startAt, cs.at(time.Now().UnixMilli())}) {
		return
	}
	offline := func(why string) {
		if r.Context().Err() == nil {
			cs.line(map[string]string{"offline": why})
			cs.flush()
		}
	}
	if first > 1 { // the cut-off history set the terminal size
		var cols, rows uint32
		for seq := uint32(1); seq < first && seq <= liveSizeScanSegments; seq++ {
			_ = eachChannelLine(r.Context(), s.Audit.Objects, key(seq), ch, func(l audit.LiveLine) bool {
				if l.Cols > 0 {
					cols, rows = l.Cols, l.Rows
				}
				return true
			})
		}
		if cols > 0 && !cs.size(start.UnixMilli(), cols, rows) {
			return
		}
	}
	for seq := first; seq < head.From; seq++ {
		// a missing or unreadable segment is a gap in the replay, not an end
		_ = eachChannelLine(r.Context(), s.Audit.Objects, key(seq), ch, cs.add)
		if cs.err != nil {
			return
		}
	}
	closed := false
	for { // the gateway's local backlog, up to the hand-over
		var l audit.LiveLine
		if err := dec.Decode(&l); err != nil {
			offline("the gateway stream ended early")
			return
		}
		if l.Live {
			break
		}
		if l.Closed {
			closed = true
		} else if !cs.add(l) {
			return
		}
	}
	if !cs.line(map[string]bool{"live": true}) || !cs.flush() {
		return
	}
	for !closed {
		var l audit.LiveLine
		if err := dec.Decode(&l); err != nil {
			offline("the gateway went away")
			return
		}
		switch {
		case l.Closed:
			closed = true
		case l.Error != "":
			offline(l.Error)
			return
		case l.HB:
			cs.line(map[string]bool{"hb": true})
		default:
			cs.add(l)
		}
		if !cs.flush() {
			return
		}
	}
	cs.line(map[string]bool{"done": true})
	cs.flush()
}
