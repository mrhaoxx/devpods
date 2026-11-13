// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"context"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/mrhaoxx/devpod/internal/agent"
)

// telemetryStreams is how many telemetry streams are kept open to each
// agent: HTTP/2 on each, so one carries many batches at once.
const telemetryStreams = 2

// SetTelemetry has the console take in the telemetry of the clusters whose
// agents send it over their link (capability agent.CapTelemetryLink):
// while leading() — this replica is the receiver's leader — it keeps
// streams open to each such agent, and h answers the agent's batches on
// them. A replica not leading keeps none.
func (r *Registry) SetTelemetry(h http.Handler, leading func() bool) {
	r.sink.Store(&telemetrySink{h: h, leading: leading})
}

// telemetry keeps the telemetry streams to a connected cluster's agent
// while they are wanted; it ends with the cluster's connection (ctx).
func (r *Registry) telemetry(ctx context.Context, cluster string) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if s := r.sink.Load(); s != nil {
			s.h.ServeHTTP(w, req)
			return
		}
		http.Error(w, "this console takes no telemetry", http.StatusServiceUnavailable)
	})
	h2s := &http2.Server{}
	base := &http.Server{
		// a stream quiet for a while is pinged; one whose agent is gone
		// (and the link with it) is noticed, closed and replaced
		HTTP2: &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 10 * time.Second},
	}

	var mu sync.Mutex
	open := map[net.Conn]bool{}
	closeAll := func() {
		mu.Lock()
		all := make([]net.Conn, 0, len(open))
		for c := range open {
			all = append(all, c)
		}
		mu.Unlock()
		for _, c := range all { // its server returns and takes it out of open
			_ = c.Close()
		}
	}
	defer closeAll()
	tick := time.NewTicker(r.o.TelemetryInterval)
	defer tick.Stop()
	for {
		s := r.sink.Load()
		in, _ := r.Info(cluster)
		wanted := s != nil && s.leading() && slices.Contains(in.Capabilities, agent.CapTelemetryLink)
		if !wanted {
			closeAll()
		}
		mu.Lock()
		n := len(open)
		mu.Unlock()
		for ; wanted && n < telemetryStreams; n++ {
			conn, err := r.srv.Dial(ctx, cluster, agent.PurposeTelemetry)
			if err != nil {
				break // tried again at the next tick
			}
			mu.Lock()
			open[conn] = true
			mu.Unlock()
			go func() {
				h2s.ServeConn(conn, &http2.ServeConnOpts{Context: ctx, Handler: handler, BaseConfig: base})
				_ = conn.Close()
				mu.Lock()
				delete(open, conn)
				mu.Unlock()
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
