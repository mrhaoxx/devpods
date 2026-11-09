// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

// Limits (spec §5.10, plan Task 18).
const (
	MaxBody       = 4 << 20  // compressed request body
	MaxDecoded    = 32 << 20 // decompressed NDJSON
	RatePerSecond = 10       // per DevPod
	RateBurst     = 20
	EventEvery    = time.Minute // one k8s Event per alert kind per DevPod
)

// Handler serves POST /ingest/v1/batch on the leader.
type Handler struct {
	Key      []byte
	IsLeader func() bool
	Writer   *Writer
	State    *live.State
	// DevPods reports a live DevPod by UID.
	DevPods func(uid string) (owner, name string, ok bool)
	// Resolver maps peers to sessions; nil leaves them pending.
	Resolver *Resolver
	// Events records a k8s Event (reason TelemetryAlert) on the DevPod;
	// nil = none. Calls are already rate-limited.
	Events func(uid, kind, detail string)

	mu       sync.Mutex
	written  map[stream]uint64
	limiters map[string]*rate.Limiter
	evented  map[[2]string]time.Time
}

type stream struct{ uid, boot string }

// countingReader stops after max+1 bytes and remembers it went over.
type countingReader struct {
	r    io.Reader
	n    int64
	over bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n > MaxBody {
		c.over = true
		return 0, errTooLarge
	}
	if rem := MaxBody + 1 - c.n; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > MaxBody {
		c.over = true
	}
	return n, err
}

var errTooLarge = errors.New("request body too large")

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.IsLeader() {
		http.Error(w, ErrNotLeader.Error(), http.StatusServiceUnavailable)
		return
	}
	if r.ContentLength > MaxBody {
		http.Error(w, errTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	cr := &countingReader{r: r.Body}
	hd, recs, err := wire.Decode(cr, MaxDecoded)
	if err != nil {
		if cr.over || errors.Is(err, wire.ErrTooLarge) {
			http.Error(w, errTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	uid := hd.DevPodUID
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if uid == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(wire.Token(h.Key, uid))) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, _, ok := h.DevPods(uid); !ok {
		http.Error(w, "unknown DevPod", http.StatusUnauthorized)
		return
	}
	if hd.V != wire.Version {
		http.Error(w, fmt.Sprintf("unsupported version %d", hd.V), http.StatusBadRequest)
		return
	}
	if !h.limiter(uid).Allow() {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	st := stream{uid, hd.BootID}
	if hd.Seq <= h.writtenSeq(st) {
		writeAck(w, hd.Seq) // already durable: acknowledge, do not rewrite
		return
	}
	if h.State != nil {
		h.State.Apply(uid, recs)
	}
	b := h.rows(hd, recs)
	if !b.Empty() {
		done := make(chan error, 1)
		h.Writer.Submit(fmt.Sprintf("%s/%s/%d-%d", uid, hd.BootID, hd.FromSeq, hd.Seq), b, func(err error) { done <- err })
		select {
		case err := <-done:
			if err != nil {
				if !errors.Is(err, ErrNotLeader) {
					slog.Error("telemetry: insert", "err", err)
				}
				http.Error(w, "write failed: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		case <-r.Context().Done():
			return
		}
	}
	h.markWritten(st, hd.Seq)
	for _, a := range b.Alerts {
		h.event(uid, a.Kind, a.Detail)
	}
	writeAck(w, hd.Seq)
}

func writeAck(w http.ResponseWriter, seq uint64) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire.Ack{AckedSeq: seq})
}

func (h *Handler) limiter(uid string) *rate.Limiter {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.limiters == nil {
		h.limiters = map[string]*rate.Limiter{}
	}
	l := h.limiters[uid]
	if l == nil {
		l = rate.NewLimiter(RatePerSecond, RateBurst)
		h.limiters[uid] = l
	}
	return l
}

func (h *Handler) writtenSeq(s stream) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.written[s]
}

func (h *Handler) markWritten(s stream, seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.written == nil {
		h.written = map[stream]uint64{}
	}
	if seq > h.written[s] {
		h.written[s] = seq
	}
}

func (h *Handler) event(uid, kind, detail string) {
	if h.Events == nil {
		return
	}
	k := [2]string{uid, kind}
	now := time.Now()
	h.mu.Lock()
	if h.evented == nil {
		h.evented = map[[2]string]time.Time{}
	}
	if last, ok := h.evented[k]; ok && now.Sub(last) < EventEvery {
		h.mu.Unlock()
		return
	}
	h.evented[k] = now
	h.mu.Unlock()
	h.Events(uid, kind, detail)
}

// Forget drops a deleted DevPod's ingest state.
func (h *Handler) Forget(uid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.written {
		if s.uid == uid {
			delete(h.written, s)
		}
	}
	delete(h.limiters, uid)
	for k := range h.evented {
		if k[0] == uid {
			delete(h.evented, k)
		}
	}
}
