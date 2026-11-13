// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TelemetryPath is where DevPods post their telemetry batches.
const TelemetryPath = "/ingest/v1/batch"

// MaxTelemetryBatch bounds one forwarded batch.
const MaxTelemetryBatch = 16 << 20

// TelemetryForward passes telemetry batches posted in the cluster on to
// the console's receiver at target: the DevPods of a managed cluster have
// no way out of it of their own, the agent has. The batch, who it says it
// is from (the receiver checks that) and the receiver's answer pass; no
// other path, method or header does.
func TelemetryForward(target string, hc *http.Client) http.Handler {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TelemetryPath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxTelemetryBatch))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "cannot read the batch", http.StatusBadRequest)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "bad receiver address", http.StatusInternalServerError)
			return
		}
		for name, vals := range r.Header { // the batch's own: its form, and who it says it is from
			switch {
			case name == "Authorization", name == "Content-Type", name == "Content-Encoding", strings.HasPrefix(name, "X-Devpod-"):
				out.Header[name] = vals
			}
		}
		resp, err := hc.Do(out)
		if err != nil {
			http.Error(w, "the telemetry receiver cannot be reached", http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for _, h := range []string{"Content-Type", "Retry-After"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
	})
}

// CapTelemetryLink is the capability of an agent that takes its cluster's
// telemetry to the console over its link: the console then opens telemetry
// streams to it.
const CapTelemetryLink = "telemetry-link"

// PurposeTelemetry: a stream the console opens to an agent for its
// telemetry; on it the agent is the HTTP client, the console's receiver
// the server.
var PurposeTelemetry = mustPurpose("telemetry")

// TelemetryLink takes the cluster's telemetry to the console over the
// agent's link: the console offers streams (Offer), and each batch the
// DevPods post (Forward) goes on one as a request to the console's
// receiver, whose answer passes back. The agent needs no way to the
// console of its own beyond its link.
type TelemetryLink struct {
	// Wait bounds how long a batch waits for a stream (default 10 s);
	// then the DevPod is told the receiver cannot be reached.
	Wait  time.Duration
	conns chan net.Conn
	once  sync.Once
	hc    *http.Client
}

// NewTelemetryLink returns a TelemetryLink with no streams yet.
func NewTelemetryLink() *TelemetryLink {
	return &TelemetryLink{conns: make(chan net.Conn, 16)}
}

// Offer keeps a stream the console opened, for a batch to go on.
func (l *TelemetryLink) Offer(c net.Conn) {
	select {
	case l.conns <- c:
	default:
		_ = c.Close() // enough are kept
	}
}

func (l *TelemetryLink) take(ctx context.Context) (net.Conn, error) {
	wait := l.Wait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case c := <-l.conns:
		return c, nil
	case <-t.C:
		return nil, errors.New("no telemetry stream from the console")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Forward is the handler DevPods post their batches to.
func (l *TelemetryLink) Forward() http.Handler {
	l.once.Do(func() {
		l.hc = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			// every connection is a stream the console offered; a stream
			// kept idle is used again
			DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return l.take(ctx) },
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     5 * time.Minute,
			DisableCompression:  true,
		}}
	})
	return TelemetryForward("http://console"+TelemetryPath, l.hc)
}
