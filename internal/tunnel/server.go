// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// ErrPeerOffline: this instance has no control connection from the peer.
var ErrPeerOffline = errors.New("tunnel: peer has no connection to this instance")

const (
	defaultHeartbeat = 15 * time.Second
	defaultDead      = 45 * time.Second
	handshakeTimeout = 10 * time.Second
)

// ServerOptions configures one instance of a server.
type ServerOptions struct {
	// Instance is this instance's name; Instances lists every instance,
	// this one included.
	Instance  string
	Instances func() []string
	// Authorize decides who an initiator is from what it says and its
	// proof over the nonce it was sent. The returned name is the peer.
	Authorize func(ctx context.Context, hello Hello, nonce, proof []byte) (peer string, err error)
	// Sign, when set, proves this server to an initiator that asks, over
	// the nonce the initiator sent.
	Sign func(nonce []byte) []byte
	// OnPeer is told when a peer's control connection appears and goes.
	OnPeer func(peer string, connected bool)
	// Heartbeat and Dead: a control connection silent for Dead is dropped.
	Heartbeat, Dead time.Duration
}

// PeerStatus is what an instance knows of a connected peer.
type PeerStatus struct {
	Idle  int
	Since time.Time
	// RTT is the round trip of the latest ping this instance sent and the
	// peer answered (0: none answered yet — a peer of before pings both
	// ways never answers).
	RTT time.Duration
	// Rx and Tx: bytes read from and written to the peer's streams, since
	// it connected. Streams: those open now.
	Rx, Tx  int64
	Streams int
}

// Server is one instance accepting initiators.
type Server struct {
	o      ServerOptions
	notify sync.Mutex // held while a peer's presence changes and OnPeer is told
	mu     sync.Mutex
	peers  map[string]*peer
	conns  map[net.Conn]struct{}
	closed bool
	meter  meter // what Stats last read
}

type peer struct {
	control  net.Conn
	idle     []*conn
	arrive   chan struct{} // an idle connection came in
	gone     chan struct{} // closed when the control connection ends
	goneOnce sync.Once
	since    time.Time
	io       *peerIO
}

// peerIO counts what a peer's streams carry, and its round trip.
type peerIO struct {
	rx, tx, streams atomic.Int64
	rtt             atomic.Int64 // nanoseconds
}

// conn is a data connection; done closes when it is closed.
type conn struct {
	net.Conn
	once sync.Once
	done chan struct{}
	// wmu orders the idle words and the one use frame; after taken, the
	// connection is the stream's and nothing else is written to it.
	wmu   sync.Mutex
	taken bool
	// io counts the stream's bytes; set (under wmu) when it is taken
	io *peerIO
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.io != nil {
		c.io.rx.Add(int64(n))
	}
	return n, err
}

func (c *conn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if c.io != nil {
		c.io.tx.Add(int64(n))
	}
	return n, err
}

func (c *conn) Close() error {
	c.once.Do(func() {
		close(c.done)
		if c.io != nil {
			c.io.streams.Add(-1)
		}
	})
	return c.Conn.Close()
}

// NewServer returns an instance.
func NewServer(o ServerOptions) *Server {
	if o.Heartbeat <= 0 {
		o.Heartbeat = defaultHeartbeat
	}
	if o.Dead <= 0 {
		o.Dead = defaultDead
	}
	return &Server{o: o, peers: map[string]*peer{}, conns: map[net.Conn]struct{}{}}
}

func newNonce() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Close drops every connection; later ones are refused.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// Disconnect drops a peer's connections to this instance (it may dial
// again; refusing it then is Authorize's business).
func (s *Server) Disconnect(name string) {
	s.mu.Lock()
	p := s.peers[name]
	s.mu.Unlock()
	if p != nil {
		s.drop(name, p)
	}
}

// Peers lists the peers connected to this instance.
func (s *Server) Peers() map[string]PeerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]PeerStatus, len(s.peers))
	for name, p := range s.peers {
		out[name] = PeerStatus{Idle: len(p.idle), Since: p.since, RTT: time.Duration(p.io.rtt.Load()),
			Rx: p.io.rx.Load(), Tx: p.io.tx.Load(), Streams: int(p.io.streams.Load())}
	}
	return out
}

// Accept runs one accepted connection, control or data, and returns when
// it is over (an HTTP handler serving a WebSocket can simply call it).
func (s *Server) Accept(ctx context.Context, nc net.Conn) {
	if !s.track(nc) {
		_ = nc.Close()
		return
	}
	defer s.untrack(nc)
	defer func() { _ = nc.Close() }()

	_ = nc.SetDeadline(time.Now().Add(handshakeTimeout))
	nonce := newNonce()
	if err := writeFrame(nc, serverHello{Instance: s.o.Instance, Instances: s.o.Instances(), Nonce: nonce}); err != nil {
		return
	}
	var ch clientHello
	if err := readFrame(nc, &ch); err != nil {
		return
	}
	name, err := s.o.Authorize(ctx, ch.Hello, nonce, ch.Proof)
	if err == nil && ch.Role != roleControl && ch.Role != roleData {
		err = errors.New("unknown role")
	}
	if err != nil {
		_ = writeFrame(nc, result{Error: err.Error()})
		return
	}
	ok := result{OK: true}
	if s.o.Sign != nil && len(ch.Nonce) > 0 {
		ok.Proof = s.o.Sign(ch.Nonce)
	}
	switch ch.Role {
	case roleControl:
		s.control(ctx, name, nc, ok)
	case roleData:
		s.data(ctx, name, nc, ok)
	}
}

func (s *Server) control(ctx context.Context, name string, nc net.Conn, ok result) {
	p := &peer{control: nc, arrive: make(chan struct{}, 1), gone: make(chan struct{}), since: time.Now(), io: &peerIO{}}
	// a peer's coming and going are told in the order they happen
	s.notify.Lock()
	s.mu.Lock()
	old := s.peers[name]
	s.peers[name] = p
	s.mu.Unlock()
	if old == nil && s.o.OnPeer != nil {
		s.o.OnPeer(name, true)
	}
	s.notify.Unlock()
	if old != nil { // the initiator started over: the new connection is the one
		s.drop(name, old)
	}
	if err := writeFrame(nc, ok); err != nil {
		s.drop(name, p)
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	var wmu sync.Mutex
	// this instance pings too, numbered, to know the round trip
	var sent struct {
		sync.Mutex
		n  uint64
		at time.Time
	}
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		tick := time.NewTicker(s.o.Heartbeat)
		defer tick.Stop()
		for {
			ping := func() bool {
				sent.Lock()
				sent.n++
				n := sent.n
				sent.at = time.Now()
				sent.Unlock()
				wmu.Lock()
				defer wmu.Unlock()
				return writeFrame(nc, ctrlMsg{T: "ping", N: n}) == nil
			}
			if !ping() {
				return
			}
			select {
			case <-ended:
				return
			case <-tick.C:
			}
		}
	}()
	for {
		_ = nc.SetDeadline(time.Now().Add(s.o.Dead))
		var m ctrlMsg
		if err := readFrame(nc, &m); err != nil {
			break
		}
		switch m.T {
		case "ping":
			wmu.Lock()
			err := writeFrame(nc, ctrlMsg{T: "pong"})
			wmu.Unlock()
			if err != nil {
				s.drop(name, p)
				return
			}
		case "pong":
			sent.Lock()
			if m.N != 0 && m.N == sent.n {
				p.io.rtt.Store(int64(time.Since(sent.at)))
			}
			sent.Unlock()
		}
	}
	s.drop(name, p)
}

// drop ends a peer's presence: its control connection and the idle data
// connections that came with it. If it was the peer's current presence,
// its going is told — before the connections are closed, which can take
// seconds each when the other end is no longer there.
func (s *Server) drop(name string, p *peer) {
	s.notify.Lock()
	s.mu.Lock()
	current := s.peers[name] == p
	if current {
		delete(s.peers, name)
	}
	idle := p.idle
	p.idle = nil
	s.mu.Unlock()
	p.goneOnce.Do(func() { close(p.gone) })
	if current && s.o.OnPeer != nil {
		s.o.OnPeer(name, false)
	}
	s.notify.Unlock()
	go func() { _ = p.control.Close() }()
	for _, c := range idle {
		go func() { _ = c.Close() }()
	}
}

func (s *Server) data(ctx context.Context, name string, nc net.Conn, ok result) {
	c := &conn{Conn: nc, done: make(chan struct{})}
	s.mu.Lock()
	p := s.peers[name]
	if p == nil {
		s.mu.Unlock()
		_ = writeFrame(nc, result{Error: "no control connection from " + name + " on this instance"})
		return
	}
	s.mu.Unlock()
	if err := writeFrame(nc, ok); err != nil {
		return
	}
	_ = nc.SetDeadline(time.Time{})
	s.mu.Lock()
	if s.peers[name] != p { // the peer went meanwhile
		s.mu.Unlock()
		return
	}
	p.idle = append(p.idle, c)
	s.mu.Unlock()
	select {
	case p.arrive <- struct{}{}:
	default:
	}
	// while idle: a word every heartbeat. One that cannot be written is
	// taken out of the pool, so that no stream is opened on it.
	tick := time.NewTicker(s.o.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c.wmu.Lock()
		if c.taken {
			c.wmu.Unlock()
			break
		}
		_ = c.SetWriteDeadline(time.Now().Add(s.o.Dead))
		err := writeFrame(c, useMsg{Idle: true})
		_ = c.SetWriteDeadline(time.Time{})
		c.wmu.Unlock()
		if err != nil {
			s.mu.Lock()
			if i := slices.Index(p.idle, c); i >= 0 {
				p.idle = slices.Delete(p.idle, i, i+1)
			}
			s.mu.Unlock()
			return // the deferred Close in Accept ends it
		}
	}
	select { // a stream now: until whoever took it closes it
	case <-c.done:
	case <-ctx.Done():
	}
}

// Dial opens a stream to peer: it takes one of the peer's idle data
// connections, tells the peer what the stream is for, and returns it. With
// none idle it waits for the next to arrive (the peer refills its pool by
// itself). A peer with no control connection here is ErrPeerOffline at once.
func (s *Server) Dial(ctx context.Context, name string, purpose []byte) (net.Conn, error) {
	for {
		s.mu.Lock()
		p := s.peers[name]
		if p == nil {
			s.mu.Unlock()
			return nil, ErrPeerOffline
		}
		var c *conn
		if len(p.idle) > 0 {
			c = p.idle[0]
			p.idle = p.idle[1:]
		}
		more := len(p.idle) > 0
		s.mu.Unlock()
		if c == nil {
			select {
			case <-p.arrive:
				continue
			case <-p.gone:
				return nil, ErrPeerOffline
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if more { // others may be waiting for what is left
			select {
			case p.arrive <- struct{}{}:
			default:
			}
		}
		c.wmu.Lock()
		c.taken = true
		c.io = p.io
		p.io.streams.Add(1)
		_ = c.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		err := writeFrame(c, useMsg{Purpose: purpose})
		_ = c.SetWriteDeadline(time.Time{})
		c.wmu.Unlock()
		if err != nil {
			go func() { _ = c.Close() }() // it died while idle: take the next
			continue
		}
		return c, nil
	}
}
