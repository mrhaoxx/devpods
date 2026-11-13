// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/spool"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Config parameterises a Recorder.
type Config struct {
	SpoolDir      string
	SpoolMaxBytes int64
	Objects       store.ObjectStore
	Events        store.EventStore
	GatewayPod    string

	SegmentMaxBytes int64         // default 8 MiB
	SegmentMaxAge   time.Duration // default 60s
	FlushInterval   time.Duration // default 5s
	Now             func() time.Time
}

// SessionMeta identifies a session.
type SessionMeta struct {
	ID, User, DevPod, DevPodUID, Owner, ClientIP, PubkeyFP, AuthKind, ProxyAlias string
	OpenedAt                                                                     time.Time
	// Cluster the DevPod is in and Gateway (by registered name) the
	// session came in at: set by a gateway that serves several clusters.
	Cluster, Gateway string
}

// NewSessionID returns a ULID for a session opened at t.
func NewSessionID(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}

// Recorder owns the spool and uploader and hands out Sessions.
type Recorder struct {
	cfg   Config
	spool *spool.Spool
	up    *spool.Uploader

	mu       sync.Mutex
	sessions map[*Session]struct{}
	// ended: the cluster of sessions that ended lately, by id — what
	// they left in the spool may still be uploading when a gap is told
	ended map[string]endedSession
	// http: requests the HTTP proxy served, written as one batch per
	// FlushInterval (httpFlushed: when last)
	http        []store.HTTPRequestRow
	httpFlushed time.Time
}

type endedSession struct {
	cluster string
	at      time.Time
}

// endedKeep: how long an ended session's cluster is kept.
const endedKeep = time.Hour

// NewRecorder opens the spool (recovering what a previous process left).
func NewRecorder(cfg Config) (*Recorder, error) {
	if cfg.Objects == nil || cfg.Events == nil {
		return nil, errors.New("audit: Objects and Events stores are required")
	}
	if cfg.SegmentMaxBytes <= 0 {
		cfg.SegmentMaxBytes = 8 << 20
	}
	if cfg.SegmentMaxAge <= 0 {
		cfg.SegmentMaxAge = 60 * time.Second
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	sp, err := spool.Open(cfg.SpoolDir, cfg.SpoolMaxBytes)
	if err != nil {
		return nil, err
	}
	r := &Recorder{cfg: cfg, spool: sp, sessions: map[*Session]struct{}{}, ended: map[string]endedSession{}}
	r.up = &spool.Uploader{Spool: sp, Objects: cfg.Objects, Events: cfg.Events, OnLag: UploadLag.Set}
	return r, nil
}

// Run uploads in the background and, once a second, rotates aged
// segments, flushes row batches and tears down fail-closed sessions when
// the spool is full. Returns when ctx is done.
func (r *Recorder) Run(ctx context.Context) {
	go r.up.Run(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.tick(now)
		}
	}
}

func (r *Recorder) snapshot() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.sessions))
	for s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// HTTP keeps one request the HTTP proxy served; it is written to the
// spool with the next batch.
func (r *Recorder) HTTP(row store.HTTPRequestRow) {
	row.GatewayPod = r.cfg.GatewayPod
	r.mu.Lock()
	r.http = append(r.http, row)
	r.mu.Unlock()
}

// Full says the spool has no room left: what is recorded now is lost.
func (r *Recorder) Full() bool { return r.spool.Level() == spool.LevelFull }

// flushHTTP writes the kept requests as one batch, once FlushInterval has
// passed since the last (always when force).
func (r *Recorder) flushHTTP(now time.Time, force bool) {
	r.mu.Lock()
	if len(r.http) == 0 || (!force && now.Sub(r.httpFlushed) < r.cfg.FlushInterval) {
		r.mu.Unlock()
		return
	}
	rows := r.http
	r.http, r.httpFlushed = nil, now
	r.mu.Unlock()
	if err := r.spool.WriteBatch(store.Batch{HTTPRequests: rows}); err != nil {
		HTTPDroppedTotal.Add(float64(len(rows))) // the spool is completely full
	}
}

func (r *Recorder) tick(now time.Time) {
	r.flushHTTP(now, false)
	SpoolBytes.Set(float64(r.spool.Usage()))
	r.mu.Lock()
	for id, e := range r.ended {
		if now.Sub(e.at) > endedKeep {
			delete(r.ended, id)
		}
	}
	r.mu.Unlock()
	full := r.spool.Level() == spool.LevelFull
	for _, s := range r.snapshot() {
		s.tick(now)
		if full && s.pol.FailClosed {
			s.kill("audit_fail_closed")
		}
	}
}

// Start begins recording a session under policy pol.
func (r *Recorder) Start(meta SessionMeta, pol policy.Effective) *Session {
	s := newSession(r, meta, pol)
	r.mu.Lock()
	r.sessions[s] = struct{}{}
	r.mu.Unlock()
	return s
}

func (r *Recorder) remove(s *Session) {
	r.mu.Lock()
	delete(r.sessions, s)
	r.ended[s.meta.ID] = endedSession{cluster: s.meta.Cluster, at: r.cfg.Now()}
	r.mu.Unlock()
}

// clusterOf: the cluster of a session this recorder recorded.
func (r *Recorder) clusterOf(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for s := range r.sessions {
		if s.meta.ID == id {
			return s.meta.Cluster
		}
	}
	return r.ended[id].cluster
}

// Drain seals and flushes every session (and the HTTP requests kept), then uploads until the spool is
// empty or ctx ends. On a deadline it records best-effort gap alerts for
// sessions whose data could not be uploaded.
func (r *Recorder) Drain(ctx context.Context) error {
	for _, s := range r.snapshot() {
		s.sealAndFlush()
	}
	r.flushHTTP(r.cfg.Now(), true)
	for {
		if err := r.up.Flush(ctx); err == nil {
			items, err := r.spool.Ready()
			if err == nil && len(items) == 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			r.alertUnuploaded()
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (r *Recorder) alertUnuploaded() {
	items, err := r.spool.Ready()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	var b store.Batch
	now := r.cfg.Now()
	for _, it := range items {
		// raw/<y>/<m>/<d>/<session>/... and files/<y>/<m>/<d>/<session>/...
		parts := strings.Split(it.ObjectKey, "/")
		if len(parts) < 5 || seen[parts[4]] {
			continue
		}
		seen[parts[4]] = true
		b.Alerts = append(b.Alerts, store.AlertRow{TS: now, SessionID: parts[4], Cluster: r.clusterOf(parts[4]), Kind: "gap", Source: "gateway", Detail: "gateway stopped before audit data was uploaded"})
	}
	if len(b.Alerts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.cfg.Events.Insert(ctx, b, ulid.Make().String())
}
