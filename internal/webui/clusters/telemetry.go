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

	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// telemetryStreams is how many telemetry streams are kept open to each
// agent: its batches go on them one after another, a few at once.
const telemetryStreams = 4

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
	ln := tunnel.NewListener()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if s := r.sink.Load(); s != nil {
				s.h.ServeHTTP(w, req)
				return
			}
			http.Error(w, "this console takes no telemetry", http.StatusServiceUnavailable)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       10 * time.Minute,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		_ = ln.Close()
		_ = srv.Close() // and every stream it holds
	}()

	var mu sync.Mutex
	open := map[net.Conn]bool{}
	tick := time.NewTicker(r.o.TelemetryInterval)
	defer tick.Stop()
	for {
		s := r.sink.Load()
		in, _ := r.Info(cluster)
		wanted := s != nil && s.leading() && slices.Contains(in.Capabilities, agent.CapTelemetryLink)
		mu.Lock()
		var drop []net.Conn
		if !wanted {
			for c := range open {
				drop = append(drop, c)
			}
		}
		n := len(open) - len(drop)
		mu.Unlock()
		for _, c := range drop { // closing takes it out of open (under mu)
			_ = c.Close()
		}
		for ; wanted && n < telemetryStreams; n++ {
			conn, err := r.srv.Dial(ctx, cluster, agent.PurposeTelemetry)
			if err != nil {
				break // tried again at the next tick
			}
			c := &trackedConn{Conn: conn}
			c.onClose = func() {
				mu.Lock()
				delete(open, c)
				mu.Unlock()
			}
			mu.Lock()
			open[c] = true
			mu.Unlock()
			ln.Offer(c)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// trackedConn says when it is closed (by either end: the server closes it
// once it is read to its end).
type trackedConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *trackedConn) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}
