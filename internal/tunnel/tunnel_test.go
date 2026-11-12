// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package tunnel_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// site is a server address with several instances behind it: a new
// connection lands on one of them in turn, as behind a load balancer.
type site struct {
	mu        sync.Mutex
	instances []*tunnel.Server
	names     []string
	next      int
	dials     atomic.Int64
	ctx       context.Context
	wires     []*wire // the client's end of every connection made, in order
}

// wire is the client's end of a connection; once deaf, nothing the server
// sends arrives (as behind a middlebox that dropped the connection
// without telling either side).
type wire struct {
	net.Conn
	deaf atomic.Bool
}

func (w *wire) Read(p []byte) (int, error) {
	for {
		n, err := w.Conn.Read(p)
		if err != nil || !w.deaf.Load() {
			return n, err
		}
	}
}

func (s *site) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

func (s *site) add(t *testing.T, name string, authorize func(context.Context, tunnel.Hello, []byte, []byte) (string, error), onPeer func(string, bool)) *tunnel.Server {
	t.Helper()
	if authorize == nil {
		authorize = func(_ context.Context, h tunnel.Hello, nonce, proof []byte) (string, error) {
			if !bytes.Equal(proof, append([]byte("sig:"), nonce...)) {
				return "", errors.New("bad proof")
			}
			return h.Name, nil
		}
	}
	srv := tunnel.NewServer(tunnel.ServerOptions{
		Instance: name, Instances: s.list, Authorize: authorize, OnPeer: onPeer,
		Heartbeat: 20 * time.Millisecond, Dead: 80 * time.Millisecond,
	})
	s.mu.Lock()
	s.instances = append(s.instances, srv)
	s.names = append(s.names, name)
	s.mu.Unlock()
	return srv
}

func (s *site) remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range s.names {
		if n == name {
			s.instances[i].Close()
			s.instances = append(s.instances[:i], s.instances[i+1:]...)
			s.names = append(s.names[:i], s.names[i+1:]...)
			return
		}
	}
}

// dial is what a client is given: one new connection to "the address".
func (s *site) dial(context.Context) (net.Conn, error) {
	s.dials.Add(1)
	s.mu.Lock()
	if len(s.instances) == 0 {
		s.mu.Unlock()
		return nil, errors.New("connection refused")
	}
	srv := s.instances[s.next%len(s.instances)]
	s.next++
	a, b := net.Pipe()
	w := &wire{Conn: a}
	s.wires = append(s.wires, w)
	s.mu.Unlock()
	go srv.Accept(s.ctx, b)
	return w, nil
}

type stream struct {
	purpose string
	conn    net.Conn
}

func newClient(t *testing.T, ctx context.Context, s *site, name string, idle int) (*tunnel.Client, chan stream) {
	t.Helper()
	got := make(chan stream, 64)
	c := tunnel.NewClient(tunnel.ClientOptions{
		Dial:  s.dial,
		Hello: tunnel.Hello{Kind: "agent", Name: name},
		Sign:  func(nonce []byte) []byte { return append([]byte("sig:"), nonce...) },
		Idle:  idle,
		Handle: func(_ context.Context, purpose []byte, conn net.Conn) {
			got <- stream{string(purpose), conn}
		},
		Heartbeat: 20 * time.Millisecond, Dead: 80 * time.Millisecond,
		BackoffMin: 5 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
	})
	go func() { _ = c.Run(ctx) }()
	return c, got
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func setup(t *testing.T) (context.Context, *site) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, &site{ctx: ctx}
}

func settled(c *tunnel.Client, instances []string, idle int) bool {
	st := c.Status()
	if len(st) != len(instances) {
		return false
	}
	for _, n := range instances {
		if s, ok := st[n]; !ok || !s.Control || s.Idle != idle {
			return false
		}
	}
	return true
}

func TestDial_HandsTheStreamToTheHandler(t *testing.T) {
	ctx, s := setup(t)
	srv := s.add(t, "i0", nil, nil)
	c, got := newClient(t, ctx, s, "mulan", 2)
	eventually(t, "connected", func() bool { return settled(c, []string{"i0"}, 2) })

	conn, err := srv.Dial(ctx, "mulan", []byte("kube"))
	if err != nil {
		t.Fatal(err)
	}
	st := <-got
	if st.purpose != "kube" {
		t.Fatalf("purpose %q", st.purpose)
	}
	// bytes both ways, nothing of the handshake left in between
	go func() { _, _ = conn.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(st.conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("server → client: %q %v", buf, err)
	}
	go func() { _, _ = st.conn.Write([]byte("pong")) }()
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("client → server: %q %v", buf, err)
	}
	// closing one end ends the other
	_ = conn.Close()
	if _, err := st.conn.Read(buf); err == nil {
		t.Fatal("the client's end is still open")
	}
	// the stream that was taken is replaced
	eventually(t, "pool refilled", func() bool { return settled(c, []string{"i0"}, 2) })
}

func TestClient_ConnectsToEveryInstance(t *testing.T) {
	ctx, s := setup(t)
	srvs := []*tunnel.Server{s.add(t, "i0", nil, nil), s.add(t, "i1", nil, nil), s.add(t, "i2", nil, nil)}
	c, _ := newClient(t, ctx, s, "mulan", 8)
	eventually(t, "every instance has a control connection and 8 idle ones", func() bool { return settled(c, []string{"i0", "i1", "i2"}, 8) })
	for i, srv := range srvs {
		p, ok := srv.Peers()["mulan"]
		if !ok || p.Idle != 8 {
			t.Fatalf("instance %d sees %+v", i, p)
		}
	}
	// once settled, the client stops dialling
	before := s.dials.Load()
	time.Sleep(100 * time.Millisecond)
	if after := s.dials.Load(); after != before {
		t.Fatalf("still dialling when settled: %d → %d", before, after)
	}
}

func TestClient_FollowsInstanceChanges(t *testing.T) {
	ctx, s := setup(t)
	s.add(t, "i0", nil, nil)
	c, _ := newClient(t, ctx, s, "mulan", 2)
	eventually(t, "one instance", func() bool { return settled(c, []string{"i0"}, 2) })
	srv1 := s.add(t, "i1", nil, nil)
	// the client learns of the new instance from the next handshake it makes:
	// using a stream makes it dial again
	eventually(t, "the new instance is joined", func() bool {
		if conn, err := s.instances[0].Dial(ctx, "mulan", []byte("x")); err == nil {
			_ = conn.Close()
		}
		return settled(c, []string{"i0", "i1"}, 2)
	})
	if p := srv1.Peers()["mulan"]; p.Idle != 2 {
		t.Fatalf("new instance sees %+v", p)
	}
	s.remove("i0")
	eventually(t, "the removed instance is dropped", func() bool { return settled(c, []string{"i1"}, 2) })
}

func TestIdlePool_RefillsAndGrows(t *testing.T) {
	ctx, s := setup(t)
	srv := s.add(t, "i0", nil, nil)
	c, got := newClient(t, ctx, s, "mulan", 2)
	eventually(t, "connected", func() bool { return settled(c, []string{"i0"}, 2) })

	// more streams at once than there are idle connections: the server asks for more
	const n = 7
	var wg sync.WaitGroup
	conns := make([]net.Conn, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := srv.Dial(ctx, "mulan", []byte("s"))
			if err != nil {
				t.Errorf("dial %d: %v", i, err)
				return
			}
			conns[i] = conn
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		<-got
	}
	eventually(t, "pool back to its size", func() bool { return settled(c, []string{"i0"}, 2) })
	for _, conn := range conns {
		if conn != nil {
			_ = conn.Close()
		}
	}

	// a peer this instance has no connection from: said at once, not after a wait
	start := time.Now()
	if _, err := srv.Dial(ctx, "st2", []byte("s")); !errors.Is(err, tunnel.ErrPeerOffline) || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("unknown peer: %v after %v", err, time.Since(start))
	}
}

// An idle data connection carries a word from the server now and then:
// one that has gone silent is given up by the client and replaced, and
// the server stops offering one it can no longer write to. The control
// connection is not disturbed.
func TestIdleConnections_SilentOnesAreReplaced(t *testing.T) {
	ctx, s := setup(t)
	var ups atomic.Int64
	srv := s.add(t, "i0", nil, func(_ string, up bool) {
		if up {
			ups.Add(1)
		}
	})
	c, got := newClient(t, ctx, s, "mulan", 2)
	eventually(t, "connected", func() bool { return settled(c, []string{"i0"}, 2) })
	// idle for several heartbeats: nothing is given up while the server keeps talking
	before := s.dials.Load()
	time.Sleep(250 * time.Millisecond)
	if after := s.dials.Load(); after != before {
		t.Fatalf("healthy idle connections were replaced: %d → %d dials", before, after)
	}
	// the first connection is the control one; the two after it are the idle data ones
	s.mu.Lock()
	for _, w := range s.wires[1:] {
		w.deaf.Store(true)
	}
	s.mu.Unlock()
	eventually(t, "the silent ones are replaced", func() bool {
		return s.dials.Load() >= before+2 && settled(c, []string{"i0"}, 2) && srv.Peers()["mulan"].Idle == 2
	})
	if ups.Load() != 1 {
		t.Fatalf("the control connection was dropped %d times", ups.Load()-1)
	}
	conn, err := srv.Dial(ctx, "mulan", []byte("after"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case st := <-got:
		if st.purpose != "after" {
			t.Fatalf("purpose %q", st.purpose)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stream opened after the replacement never reached the client")
	}
}

func TestOneStreamDoesNotAffectAnother(t *testing.T) {
	ctx, s := setup(t)
	srv := s.add(t, "i0", nil, nil)
	c, got := newClient(t, ctx, s, "mulan", 2)
	eventually(t, "connected", func() bool { return settled(c, []string{"i0"}, 2) })
	a, _ := srv.Dial(ctx, "mulan", []byte("a"))
	b, _ := srv.Dial(ctx, "mulan", []byte("b"))
	sa, sb := <-got, <-got
	if sa.purpose == "b" {
		sa, sb = sb, sa
	}
	_ = a.Close()
	_ = sa.conn.Close()
	go func() { _, _ = b.Write([]byte("still")) }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(sb.conn, buf); err != nil || string(buf) != "still" {
		t.Fatalf("the other stream: %q %v", buf, err)
	}
}

func TestHeartbeat_ASilentPeerIsDroppedAndTheClientComesBack(t *testing.T) {
	ctx, s := setup(t)
	var ups, downs atomic.Int64
	srv := s.add(t, "i0", nil, func(peer string, up bool) {
		if up {
			ups.Add(1)
		} else {
			downs.Add(1)
		}
	})
	cctx, stop := context.WithCancel(ctx)
	c, _ := newClient(t, cctx, s, "mulan", 1)
	eventually(t, "connected", func() bool { return settled(c, []string{"i0"}, 1) })
	if ups.Load() != 1 {
		t.Fatalf("ups %d", ups.Load())
	}
	// the client goes away without a word: the server notices
	stop()
	eventually(t, "the server drops the peer", func() bool { _, ok := srv.Peers()["mulan"]; return !ok && downs.Load() == 1 })
	if _, err := srv.Dial(ctx, "mulan", nil); !errors.Is(err, tunnel.ErrPeerOffline) {
		t.Fatalf("dial after the drop: %v", err)
	}
	// and a new client is taken back
	c2, _ := newClient(t, ctx, s, "mulan", 1)
	eventually(t, "reconnected", func() bool { return settled(c2, []string{"i0"}, 1) && ups.Load() == 2 })
}

func TestClient_RetriesWithBackoffWhileTheServerIsAway(t *testing.T) {
	ctx, s := setup(t)
	c, _ := newClient(t, ctx, s, "mulan", 1) // nothing behind the address yet
	time.Sleep(200 * time.Millisecond)
	// backoff 5 ms doubling to 40 ms: far fewer than one attempt per millisecond
	if n := s.dials.Load(); n < 3 || n > 40 {
		t.Fatalf("%d dials in 200 ms", n)
	}
	s.add(t, "i0", nil, nil)
	eventually(t, "connects once a server is there", func() bool { return settled(c, []string{"i0"}, 1) })
}

func TestAuthorize(t *testing.T) {
	ctx, s := setup(t)
	srv := s.add(t, "i0", func(_ context.Context, h tunnel.Hello, nonce, proof []byte) (string, error) {
		if h.Kind != "agent" || h.Name != "mulan" || !bytes.Equal(proof, append([]byte("sig:"), nonce...)) {
			return "", errors.New("not you")
		}
		return h.Name, nil
	}, nil)
	// a stranger: refused, and never becomes a peer
	stranger, _ := newClient(t, ctx, s, "st2", 1)
	time.Sleep(60 * time.Millisecond)
	if len(srv.Peers()) != 0 {
		t.Fatalf("a stranger got in: %v", srv.Peers())
	}
	for name, st := range stranger.Status() { // it knows the instance exists, and holds nothing on it
		if st.Control || st.Idle != 0 {
			t.Fatalf("the stranger holds connections on %s: %+v", name, st)
		}
	}
	if err := stranger.LastError(); err == nil || !bytes.Contains([]byte(err.Error()), []byte("not you")) {
		t.Fatalf("the reason did not reach the client: %v", err)
	}
	// said once, however many times it is refused
	var told atomic.Int64
	c := tunnel.NewClient(tunnel.ClientOptions{
		Dial: s.dial, Hello: tunnel.Hello{Kind: "agent", Name: "st3"}, Sign: func(n []byte) []byte { return n },
		Handle: func(context.Context, []byte, net.Conn) {}, BackoffMin: 2 * time.Millisecond, BackoffMax: 5 * time.Millisecond,
		OnError: func(error) { told.Add(1) },
	})
	go func() { _ = c.Run(ctx) }()
	time.Sleep(80 * time.Millisecond)
	if told.Load() != 1 {
		t.Fatalf("OnError called %d times for one lasting refusal", told.Load())
	}
	ok, _ := newClient(t, ctx, s, "mulan", 1)
	eventually(t, "the right one gets in", func() bool { return settled(ok, []string{"i0"}, 1) })
	// two nonces are never the same
	n1, n2 := tunnel.NewNonceForTest(), tunnel.NewNonceForTest()
	if bytes.Equal(n1, n2) || len(n1) < 16 {
		t.Fatalf("nonces %x %x", n1, n2)
	}
}
