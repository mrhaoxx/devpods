// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"time"
)

// ClientOptions configures an initiator.
type ClientOptions struct {
	// Dial makes one new connection to the server's address; which
	// instance it reaches is not the caller's to choose.
	Dial func(ctx context.Context) (net.Conn, error)
	// Hello is who this is; Sign proves it over the server's nonce.
	Hello Hello
	Sign  func(nonce []byte) []byte
	// Idle is how many unused data connections to keep on each instance.
	Idle int
	// Handle is given each stream the server opens, with what it is for.
	// It owns conn and is called on its own goroutine.
	Handle func(ctx context.Context, purpose []byte, conn net.Conn)
	// Heartbeat and Dead: a ping every Heartbeat; a control connection
	// silent for Dead is given up. BackoffMin and BackoffMax bound the
	// wait between attempts that fail.
	Heartbeat, Dead        time.Duration
	BackoffMin, BackoffMax time.Duration
}

// InstanceStatus is what an initiator holds on one server instance.
type InstanceStatus struct {
	Control bool
	Idle    int
}

// Client keeps a control connection and Idle data connections on every
// instance of the server.
type Client struct {
	o    ClientOptions
	mu   sync.Mutex
	inst map[string]*instance
	wake chan struct{}
	err  error
}

type instance struct {
	control net.Conn
	idle    map[net.Conn]struct{}
}

// NewClient returns an initiator; Run starts it.
func NewClient(o ClientOptions) *Client {
	if o.Idle <= 0 {
		o.Idle = 8
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = defaultHeartbeat
	}
	if o.Dead <= 0 {
		o.Dead = defaultDead
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 30 * time.Second
	}
	return &Client{o: o, inst: map[string]*instance{}, wake: make(chan struct{}, 1)}
}

// Status reports, per known instance, what is held there.
func (c *Client) Status() map[string]InstanceStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]InstanceStatus, len(c.inst))
	for name, in := range c.inst {
		out[name] = InstanceStatus{Control: in.control != nil, Idle: len(in.idle)}
	}
	return out
}

// LastError is why the latest attempt failed, nil after a success.
func (c *Client) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) nudge() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// wanting reports whether anything is missing: no instance known yet, or
// one without its control connection or its full pool.
func (c *Client) wanting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inst) == 0 {
		return true
	}
	for _, in := range c.inst {
		if in.control == nil || len(in.idle) < c.o.Idle {
			return true
		}
	}
	return false
}

// Run keeps the connections up until ctx ends.
func (c *Client) Run(ctx context.Context) error {
	defer c.closeAll()
	backoff := c.o.BackoffMin
	sleep := func(d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-ctx.Done():
			return false
		}
	}
	surplus := 0
	for ctx.Err() == nil {
		if !c.wanting() {
			surplus = 0
			select {
			case <-c.wake:
			case <-ctx.Done():
			}
			continue
		}
		used, err := c.connect(ctx)
		switch {
		case err != nil:
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			if !sleep(backoff) {
				return ctx.Err()
			}
			if backoff *= 2; backoff > c.o.BackoffMax {
				backoff = c.o.BackoffMax
			}
		case !used:
			// reached an instance that already has everything while
			// another still lacks something: try again, ever more slowly
			// if the lacking one cannot be reached at all
			if surplus++; surplus > 8 {
				d := c.o.BackoffMin * time.Duration(surplus-8)
				if d > c.o.BackoffMax {
					d = c.o.BackoffMax
				}
				if !sleep(d) {
					return ctx.Err()
				}
			}
		default:
			surplus = 0
			backoff = c.o.BackoffMin
			c.mu.Lock()
			c.err = nil
			c.mu.Unlock()
		}
	}
	return ctx.Err()
}

func (c *Client) closeAll() {
	c.mu.Lock()
	var conns []net.Conn
	for _, in := range c.inst {
		if in.control != nil {
			conns = append(conns, in.control)
		}
		for d := range in.idle {
			conns = append(conns, d)
		}
	}
	c.inst = map[string]*instance{}
	c.mu.Unlock()
	for _, nc := range conns {
		_ = nc.Close()
	}
}

// connect makes one connection and gives it the role the instance it
// reached still needs. used is false when that instance needs nothing.
func (c *Client) connect(ctx context.Context) (used bool, err error) {
	nc, err := c.o.Dial(ctx)
	if err != nil {
		return false, err
	}
	_ = nc.SetDeadline(time.Now().Add(handshakeTimeout))
	var sh serverHello
	if err := readFrame(nc, &sh); err != nil {
		_ = nc.Close()
		return false, err
	}

	// the instances there are now: forget those that are gone
	c.mu.Lock()
	var gone []net.Conn
	for name, in := range c.inst {
		if !slices.Contains(sh.Instances, name) {
			if in.control != nil {
				gone = append(gone, in.control)
			}
			for d := range in.idle {
				gone = append(gone, d)
			}
			delete(c.inst, name)
		}
	}
	for _, name := range sh.Instances {
		if c.inst[name] == nil {
			c.inst[name] = &instance{idle: map[net.Conn]struct{}{}}
		}
	}
	in := c.inst[sh.Instance]
	role := ""
	switch {
	case in == nil: // an instance that does not list itself
	case in.control == nil:
		role = roleControl
	case len(in.idle) < c.o.Idle:
		role = roleData
	}
	c.mu.Unlock()
	for _, g := range gone {
		_ = g.Close()
	}
	if role == "" {
		_ = nc.Close()
		return false, nil
	}

	if err := writeFrame(nc, clientHello{Hello: c.o.Hello, Role: role, Proof: c.o.Sign(sh.Nonce)}); err != nil {
		_ = nc.Close()
		return false, err
	}
	var res result
	if err := readFrame(nc, &res); err != nil {
		_ = nc.Close()
		return false, err
	}
	if !res.OK {
		_ = nc.Close()
		return false, errors.New("tunnel: refused by " + sh.Instance + ": " + res.Error)
	}
	_ = nc.SetDeadline(time.Time{})

	c.mu.Lock()
	in = c.inst[sh.Instance]
	if in == nil || (role == roleControl && in.control != nil) {
		c.mu.Unlock()
		_ = nc.Close()
		return false, nil
	}
	if role == roleControl {
		in.control = nc
	} else {
		in.idle[nc] = struct{}{}
	}
	c.mu.Unlock()
	if role == roleControl {
		go c.controlLoop(ctx, sh.Instance, nc)
	} else {
		go c.dataWait(ctx, sh.Instance, nc)
	}
	return true, nil
}

// controlLoop pings, and gives the connection up when the instance has
// been silent for Dead.
func (c *Client) controlLoop(ctx context.Context, name string, nc net.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_ = nc.SetReadDeadline(time.Now().Add(c.o.Dead))
			var m ctrlMsg
			if err := readFrame(nc, &m); err != nil {
				return
			}
		}
	}()
	tick := time.NewTicker(c.o.Heartbeat)
	defer tick.Stop()
loop:
	for {
		select {
		case <-tick.C:
			_ = nc.SetWriteDeadline(time.Now().Add(c.o.Dead))
			if err := writeFrame(nc, ctrlMsg{T: "ping"}); err != nil {
				break loop
			}
		case <-done:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	_ = nc.Close()
	<-done
	c.mu.Lock()
	if in := c.inst[name]; in != nil && in.control == nc {
		in.control = nil
	}
	c.mu.Unlock()
	c.nudge()
}

// dataWait holds an idle data connection until the server puts it to
// use, or it breaks; either way the pool is one short and is refilled.
func (c *Client) dataWait(ctx context.Context, name string, nc net.Conn) {
	var use useMsg
	err := readFrame(nc, &use)
	c.mu.Lock()
	if in := c.inst[name]; in != nil {
		delete(in.idle, nc)
	}
	c.mu.Unlock()
	c.nudge()
	if err != nil {
		_ = nc.Close()
		return
	}
	c.o.Handle(ctx, use.Purpose, nc)
}
