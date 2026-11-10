// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/decode"
	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/spool"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Decoder factories; tests swap them.
var (
	newSFTP   = decode.NewSFTP
	newSCP    = decode.NewSCP
	newZmodem = decode.NewZmodem
)

// Session records one SSH connection. It implements SessionHooks.
type Session struct {
	r        *Recorder
	meta     SessionMeta
	pol      policy.Effective
	done     chan struct{}
	killOnce sync.Once

	nextCh atomic.Uint32
	gaps   atomic.Uint32

	mu        sync.Mutex
	backend   string
	seg       *rawlog.SegmentWriter
	cur       *segFile
	segSeq    uint32
	buf       store.Batch
	lastFlush time.Time
	gapAlert  bool
	closed    bool
	watchers  []*watcher // live viewers (live.go)
}

var _ SessionHooks = (*Session)(nil)

// segFile is the spool file behind the current raw segment.
type segFile struct {
	f          *spool.File
	seq        uint32
	key        string
	compressed int64
}

func (g *segFile) Write(p []byte) (int, error) { return g.f.Write(p) }
func (g *segFile) Close() error {
	g.compressed = g.f.Size()
	return g.f.Seal()
}

func newSession(r *Recorder, meta SessionMeta, pol policy.Effective) *Session {
	s := &Session{r: r, meta: meta, pol: pol, done: make(chan struct{}), lastFlush: r.cfg.Now()}
	s.buf.Sessions = append(s.buf.Sessions, s.row(1, nil, "", 0, 0))
	return s
}

func (s *Session) row(version uint64, closedAt *time.Time, reason string, in, out int64) store.SessionRow {
	snap, _ := json.Marshal(s.pol)
	return store.SessionRow{
		SessionID: s.meta.ID, User: s.meta.User, DevPod: s.meta.DevPod, DevPodUID: s.meta.DevPodUID,
		Owner: s.meta.Owner, GatewayPod: s.r.cfg.GatewayPod, ClientIP: s.meta.ClientIP, PubkeyFP: s.meta.PubkeyFP,
		AuthKind: s.meta.AuthKind, ProxyAlias: s.meta.ProxyAlias, BackendLocalAddr: s.backend,
		OpenedAt: s.meta.OpenedAt, ClosedAt: closedAt, CloseReason: reason, BytesIn: in, BytesOut: out,
		PolicySnapshot: string(snap), Gaps: s.gaps.Load(), Version: version,
	}
}

// SetBackendAddr records the gateway-side address of the backend TCP
// connection (the key container-side data is correlated on).
//
// The open row was written before the backend was dialled, so it is
// patched while still buffered, or written again (same version: the
// later insert replaces the earlier). Without this the address would
// only land with the close row, and telemetry could not find the
// session while it is open.
func (s *Session) SetBackendAddr(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backend = addr
	if s.closed {
		return
	}
	for i := range s.buf.Sessions {
		if r := &s.buf.Sessions[i]; r.SessionID == s.meta.ID && r.ClosedAt == nil {
			r.BackendLocalAddr = addr
			return
		}
	}
	s.buf.Sessions = append(s.buf.Sessions, s.row(1, nil, "", 0, 0))
}

// Done is closed when a fail-closed session must be torn down.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) kill() { s.killOnce.Do(func() { close(s.done) }) }

// --- raw stream --------------------------------------------------------------

func (s *Session) newSegWriter() *rawlog.SegmentWriter {
	cfg := s.r.cfg
	return rawlog.NewSegmentWriter(rawlog.SegmentConfig{
		MaxBytes: cfg.SegmentMaxBytes,
		MaxAge:   cfg.SegmentMaxAge,
		Now:      cfg.Now,
		Create: func(uint32) (io.WriteCloser, error) {
			s.segSeq++
			key := store.RawKey(s.meta.OpenedAt, s.meta.ID, s.segSeq)
			f, err := s.r.spool.Create(key)
			if err != nil {
				s.segSeq--
				return nil, err
			}
			s.cur = &segFile{f: f, seq: s.segSeq, key: key}
			return s.cur, nil
		},
		OnSeal: func(_ uint32, first, last time.Time, raw int64) error {
			if s.cur != nil {
				s.buf.RawSegments = append(s.buf.RawSegments, store.RawSegmentRow{
					SessionID: s.meta.ID, Seq: s.cur.seq, S3Key: s.cur.key,
					FirstTS: first, LastTS: last, Bytes: raw, CompressedBytes: s.cur.compressed,
				})
				s.cur = nil
			}
			return nil
		},
	})
}

// record appends one raw record. Caller must not hold s.mu.
func (s *Session) record(ch uint32, kind rawlog.Kind, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(rawlog.Record{TS: s.r.cfg.Now(), Channel: ch, Kind: kind, Payload: payload})
}

func (s *Session) recordLocked(rec rawlog.Record) {
	if s.closed {
		return
	}
	if s.seg == nil {
		s.seg = s.newSegWriter()
	}
	if err := s.seg.Write(rec); err != nil {
		s.abandonSegmentLocked(err)
	}
	s.deliverLocked(rec)
}

// abandonSegmentLocked drops the current segment after a spool error:
// its data is lost and counted as a gap.
func (s *Session) abandonSegmentLocked(err error) {
	if s.cur != nil {
		_ = s.cur.f.Discard()
		s.cur = nil
	}
	s.seg = nil
	s.gapLocked(err)
}

func (s *Session) gapLocked(err error) {
	s.gaps.Add(1)
	GapsTotal.Inc()
	if !s.gapAlert {
		s.gapAlert = true
		s.buf.Alerts = append(s.buf.Alerts, store.AlertRow{
			TS: s.r.cfg.Now(), DevPodUID: s.meta.DevPodUID, SessionID: s.meta.ID,
			Kind: "gap", Source: "gateway", Detail: "audit data dropped: " + err.Error(),
		})
		s.flushLocked(s.r.cfg.Now())
	}
}

func (s *Session) flushLocked(now time.Time) {
	s.lastFlush = now
	if s.buf.Empty() {
		return
	}
	if err := s.r.spool.WriteBatch(s.buf); err != nil {
		GapsTotal.Inc() // rows lost: the spool is completely full
	}
	s.buf = store.Batch{}
}

func (s *Session) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.seg != nil {
		if err := s.seg.Tick(); err != nil {
			s.abandonSegmentLocked(err)
		}
	}
	if now.Sub(s.lastFlush) >= s.r.cfg.FlushInterval {
		s.flushLocked(now)
	}
}

// sealAndFlush closes the current segment and writes pending rows.
func (s *Session) sealAndFlush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seg != nil {
		if err := s.seg.Close(); err != nil {
			s.abandonSegmentLocked(err)
		}
		s.seg = nil
	}
	s.flushLocked(s.r.cfg.Now())
}

// --- rows --------------------------------------------------------------------

func (s *Session) addRows(fn func(b *store.Batch)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.buf)
}

func (s *Session) policyEvent(rule, action, detail string) {
	PolicyActionsTotal.WithLabelValues(rule, action).Inc()
	s.addRows(func(b *store.Batch) {
		b.PolicyEvents = append(b.PolicyEvents, store.PolicyEventRow{
			TS: s.r.cfg.Now(), SessionID: s.meta.ID, User: s.meta.User, DevPodUID: s.meta.DevPodUID,
			Rule: rule, Action: action, Detail: detail,
		})
	})
}

func (s *Session) deny(rule, msg string) error {
	s.policyEvent(rule, "deny", msg)
	return &DenyError{Msg: msg}
}

func (s *Session) alert(kind, detail string) {
	s.addRows(func(b *store.Batch) {
		b.Alerts = append(b.Alerts, store.AlertRow{
			TS: s.r.cfg.Now(), DevPodUID: s.meta.DevPodUID, SessionID: s.meta.ID,
			Kind: kind, Source: "gateway", Detail: detail,
		})
	})
}

func (s *Session) fileEvent(row store.FileEventRow) {
	row.TS = s.r.cfg.Now()
	row.SessionID, row.User, row.DevPodUID = s.meta.ID, s.meta.User, s.meta.DevPodUID
	if row.EventID == "" {
		row.EventID = NewSessionID(row.TS)
	}
	s.addRows(func(b *store.Batch) { b.FileEvents = append(b.FileEvents, row) })
}

// --- SessionHooks --------------------------------------------------------------

// OpenChannel applies the forwarding and fail-closed rules, then starts
// recording the channel.
func (s *Session) OpenChannel(dir Dir, chType string, extra []byte) (ChannelHooks, error) {
	if s.pol.FailClosed && s.r.spool.Level() >= spool.LevelHigh {
		return nil, s.deny("failMode", MsgAuditUnavailable)
	}
	switch chType {
	case "direct-tcpip", "direct-streamlocal@openssh.com":
		if dir == ClientToBackend && s.pol.DenyLocalForward {
			return nil, s.deny("forwarding.local", MsgForwardDenied)
		}
	case "auth-agent@openssh.com":
		if s.pol.DenyAgentForward {
			return nil, s.deny("agentForwarding", MsgAgentDenied)
		}
	case "x11":
		if s.pol.DenyX11 {
			return nil, s.deny("x11Forwarding", MsgX11Denied)
		}
	}
	c := newChannel(s, s.nextCh.Add(1), dir, chType, extra)
	s.mu.Lock()
	s.recordLocked(rawlog.Record{TS: c.opened, Channel: c.id, Kind: rawlog.KindChannelOpen,
		Payload: rawlog.Encode(rawlog.ChannelOpen{Dir: uint8(dir), Type: chType, Extra: extra})})
	s.buf.Channels = append(s.buf.Channels, c.row)
	s.mu.Unlock()
	return c, nil
}

// GlobalRequest enforces forwarding.remote (TCP and Unix-socket).
func (s *Session) GlobalRequest(dir Dir, typ string, payload []byte) error {
	var listen bool
	switch typ {
	case "tcpip-forward", "streamlocal-forward@openssh.com":
		listen = true
	case "cancel-tcpip-forward", "cancel-streamlocal-forward@openssh.com":
	default:
		return nil
	}
	denied := listen && dir == ClientToBackend && s.pol.DenyRemoteForward
	s.record(0, rawlog.KindRequest, rawlog.Encode(rawlog.Request{Dir: uint8(dir), Type: typ, WantReply: true, OK: !denied, Denied: denied, Payload: payload}))
	if denied {
		return s.deny("forwarding.remote", MsgForwardDenied)
	}
	return nil
}

// Close ends the session: seals the segment and writes the final row.
func (s *Session) Close(reason string, bytesIn, bytesOut int64) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.seg != nil {
		if err := s.seg.Close(); err != nil {
			s.abandonSegmentLocked(err)
		}
		s.seg = nil
	}
	now := s.r.cfg.Now()
	s.buf.Sessions = append(s.buf.Sessions, s.row(2, &now, reason, bytesIn, bytesOut))
	s.flushLocked(now)
	s.closed = true
	s.endWatchersLocked()
	s.mu.Unlock()
	s.r.remove(s)
}
