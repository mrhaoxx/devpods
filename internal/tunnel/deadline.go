// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"io"
	"net"
	"sync"
)

// DeadlineSafe returns c with the deadlines of a net.Conn: one that passes
// fails the read or write it bounds, and later ones until it is moved, and
// leaves the connection as it was. A WebSocket NetConn (coder/websocket)
// ends the connection instead when a deadline passes during a read — and an
// HTTP server moves the read deadline into the past after every request, to
// stop its read in the background: a stream it served died after its first
// response, and a request sent on it at that moment failed (EOF).
func DeadlineSafe(c net.Conn) net.Conn {
	inner, outer := net.Pipe()
	go func() {
		_, _ = io.Copy(inner, c)
		_ = inner.Close()
	}()
	go func() {
		_, _ = io.Copy(c, inner)
		_ = c.Close()
	}()
	return &deadlineSafe{Conn: outer, under: c}
}

type deadlineSafe struct {
	net.Conn // the pipe's end: its deadlines are a net.Conn's
	under    net.Conn
	once     sync.Once
}

func (d *deadlineSafe) Close() error {
	d.once.Do(func() { _ = d.under.Close() })
	return d.Conn.Close()
}

func (d *deadlineSafe) LocalAddr() net.Addr  { return d.under.LocalAddr() }
func (d *deadlineSafe) RemoteAddr() net.Addr { return d.under.RemoteAddr() }
