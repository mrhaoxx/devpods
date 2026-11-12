// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package tunnel

import "net"

// Listener hands streams to a server (an http.Server, say) as accepted
// connections: what an initiator's Handle offers, Accept returns.
type Listener struct {
	conns chan net.Conn
	done  chan struct{}
}

// NewListener returns a Listener.
func NewListener() *Listener {
	return &Listener{conns: make(chan net.Conn), done: make(chan struct{})}
}

// Offer gives the listener's server a stream; after Close it is closed.
func (l *Listener) Offer(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

// Accept returns the next offered stream.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close ends Accept.
func (l *Listener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

// Addr is a placeholder: streams have no address.
func (l *Listener) Addr() net.Addr { return streamAddr{} }

type streamAddr struct{}

func (streamAddr) Network() string { return "tunnel" }
func (streamAddr) String() string  { return "tunnel" }
