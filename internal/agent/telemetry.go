// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
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
// telemetry; on it the agent is the HTTP/2 client, the console's receiver
// the server.
var PurposeTelemetry = mustPurpose("telemetry")

// TelemetryLink takes the cluster's telemetry to the console over the
// agent's link: the console offers streams (Offer), each becomes an HTTP/2
// connection at once, and each batch the DevPods post (Forward) goes on
// one as a request to the console's receiver, whose answer passes back.
// The agent needs no way to the console of its own beyond its link.
type TelemetryLink struct {
	// Wait bounds how long a batch waits for a stream (default 10 s);
	// then the DevPod is told the receiver cannot be reached.
	Wait time.Duration
	t2   *http2.Transport
	hc   *http.Client

	mu    sync.Mutex
	conns []*http2.ClientConn
	more  chan struct{} // closed when a stream is offered
}

// NewTelemetryLink returns a TelemetryLink with no streams yet.
func NewTelemetryLink() *TelemetryLink {
	t1 := &http.Transport{
		DisableCompression: true, // the batch and the answer pass as they are
		// a stream quiet for a while is pinged: what is in between (a
		// reverse proxy) does not take it for idle, and one gone is noticed
		HTTP2: &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 10 * time.Second},
	}
	t2, err := http2.ConfigureTransports(t1)
	if err != nil {
		panic(err) // only if t1 already speaks HTTP/2: it does not
	}
	t2.AllowHTTP = true
	l := &TelemetryLink{t2: t2, more: make(chan struct{})}
	l.hc = &http.Client{Timeout: 30 * time.Second, Transport: roundTripFunc(l.roundTrip)}
	return l
}

// Offer takes a stream the console opened: from now on an HTTP/2
// connection, kept (and pinged) until a batch goes on it.
func (l *TelemetryLink) Offer(c net.Conn) {
	cc, err := l.t2.NewClientConn(c)
	if err != nil {
		_ = c.Close()
		return
	}
	l.mu.Lock()
	live := l.conns[:0]
	for _, x := range l.conns {
		if st := x.State(); !st.Closed && !st.Closing {
			live = append(live, x)
		}
	}
	l.conns = append(live, cc)
	close(l.more)
	l.more = make(chan struct{})
	l.mu.Unlock()
}

func (l *TelemetryLink) roundTrip(req *http.Request) (*http.Response, error) {
	wait := l.Wait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		l.mu.Lock()
		more := l.more
		var cc *http2.ClientConn
		for _, x := range l.conns {
			if x.CanTakeNewRequest() {
				cc = x
				break
			}
		}
		l.mu.Unlock()
		if cc != nil {
			return cc.RoundTrip(req)
		}
		select {
		case <-more:
		case <-t.C:
			return nil, errors.New("no telemetry stream from the console")
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Forward is the handler DevPods post their batches to.
func (l *TelemetryLink) Forward() http.Handler {
	return TelemetryForward("http://console"+TelemetryPath, l.hc)
}
