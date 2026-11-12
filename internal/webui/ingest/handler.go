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
	MaxBody       = 4 << 20 // compressed request body
	MaxDecoded    = 8 << 20 // decompressed NDJSON (agents send ≤ 1 MiB)
	RatePerSecond = 10      // per DevPod
	RateBurst     = 20
	EventEvery    = time.Minute // one k8s Event per alert kind per DevPod
	// DefaultMaxInFlight bounds the decompressed batches held until their
	// merged write (catch-up after a ClickHouse outage); over it → 503.
	DefaultMaxInFlight = 128 << 20
	// decodeSlots bounds concurrent decompression (≤ MaxDecoded each).
	decodeSlots = 16
	// authCacheTTL: a known DevPod UID is looked up again after this (a
	// deleted DevPod's agent may post this much longer).
	authCacheTTL = 30 * time.Second
)

// Handler serves POST /ingest/v1/batch on the leader.
type Handler struct {
	Key      []byte
	IsLeader func() bool
	Writer   *Writer
	State    *live.State
	// DevPods reports a live DevPod by UID, and the cluster it is in ("" in
	// a console of one): every row written for it says that cluster.
	DevPods func(uid string) (owner, name, cluster string, ok bool)
	// Resolver maps peers to sessions; nil leaves them pending.
	Resolver *Resolver
	// Events records a k8s Event (reason TelemetryAlert) on the DevPod;
	// nil = none. Calls are already rate-limited.
	Events func(uid, kind, detail string)
	// MaxInFlight overrides DefaultMaxInFlight (bytes).
	MaxInFlight int64

	mu       sync.Mutex
	inflight int64
	decoding chan struct{}
	written  map[stream]uint64
	known    map[string]knownDevPod // DevPod UID → its cluster, valid until
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
	// Authenticate before reading the body: decompression is the cost.
	uid := r.Header.Get("X-DevPod-UID")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if uid == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(wire.Token(h.Key, uid))) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cluster, ok := h.knownDevPod(uid)
	if !ok {
		http.Error(w, "unknown DevPod", http.StatusUnauthorized)
		return
	}
	if !h.limiter(uid).Allow() {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	hd, recs, size, err := h.decode(r)
	if err != nil {
		var tl *tooLargeError
		if errors.As(err, &tl) {
			http.Error(w, errTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if hd.DevPodUID != uid {
		http.Error(w, "batch is for another DevPod", http.StatusBadRequest)
		return
	}
	if !h.admit(size) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "ingest busy", http.StatusServiceUnavailable)
		return
	}
	defer h.release(size)
	if hd.V != wire.Version {
		http.Error(w, fmt.Sprintf("unsupported version %d", hd.V), http.StatusBadRequest)
		return
	}
	st := stream{uid, hd.BootID}
	if hd.Seq <= h.writtenSeq(st) {
		writeAck(w, hd.Seq) // already durable: acknowledge, do not rewrite
		return
	}
	if h.State != nil {
		h.State.SetBoot(uid, hd.BootID)
		h.State.Apply(uid, recs)
	}
	b := h.rows(hd, recs)
	b.SetCluster(cluster)
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

type knownDevPod struct {
	cluster string
	until   time.Time
}

// knownDevPod checks the UID against the DevPods, caching a hit (and the
// cluster the DevPod is in) for authCacheTTL.
func (h *Handler) knownDevPod(uid string) (string, bool) {
	h.mu.Lock()
	k, ok := h.known[uid]
	h.mu.Unlock()
	if ok && time.Now().Before(k.until) {
		return k.cluster, true
	}
	_, _, cluster, ok := h.DevPods(uid)
	if !ok {
		return "", false
	}
	h.mu.Lock()
	if h.known == nil {
		h.known = map[string]knownDevPod{}
	}
	h.known[uid] = knownDevPod{cluster: cluster, until: time.Now().Add(authCacheTTL)}
	h.mu.Unlock()
	return cluster, true
}

type tooLargeError struct{}

func (*tooLargeError) Error() string { return errTooLarge.Error() }

// decode decompresses the body, at most decodeSlots at a time.
func (h *Handler) decode(r *http.Request) (wire.Header, []wire.Record, int64, error) {
	h.mu.Lock()
	if h.decoding == nil {
		h.decoding = make(chan struct{}, decodeSlots)
	}
	slots := h.decoding
	h.mu.Unlock()
	select {
	case slots <- struct{}{}:
	case <-r.Context().Done():
		return wire.Header{}, nil, 0, r.Context().Err()
	}
	defer func() { <-slots }()
	cr := &countingReader{r: r.Body}
	hd, recs, n, err := wire.DecodeN(cr, MaxDecoded)
	if err != nil && (cr.over || errors.Is(err, wire.ErrTooLarge)) {
		return hd, nil, 0, &tooLargeError{}
	}
	return hd, recs, n, err
}

// admit reserves size bytes of the in-flight budget; one batch is always
// admitted when nothing else is held.
func (h *Handler) admit(size int64) bool {
	limit := h.MaxInFlight
	if limit <= 0 {
		limit = DefaultMaxInFlight
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight > 0 && h.inflight+size > limit {
		return false
	}
	h.inflight += size
	return true
}

func (h *Handler) release(size int64) {
	h.mu.Lock()
	h.inflight -= size
	h.mu.Unlock()
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
	delete(h.known, uid)
	for k := range h.evented {
		if k[0] == uid {
			delete(h.evented, k)
		}
	}
}
