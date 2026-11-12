// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package agent is the part of DevPod that runs in every cluster and
// dials out to the console. It forwards and nothing else: the console's
// Kubernetes requests to this cluster's API server under the agent's own
// service account, and word of what this cluster is. It holds no DevPod
// logic; what the console may do here is exactly what the agent's RBAC
// allows.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"k8s.io/client-go/rest"

	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// Kind is what an agent calls itself in the tunnel handshake.
const Kind = "agent"

// Purpose says what a stream is for.
type Purpose struct {
	Kind string `json:"kind"`
}

// The streams the console opens on the agent's connections.
var (
	PurposeKube = mustPurpose("kube") // HTTP to this cluster's API server
	PurposeInfo = mustPurpose("info") // what this cluster is, as JSON values
)

func mustPurpose(kind string) []byte {
	b, err := json.Marshal(Purpose{Kind: kind})
	if err != nil {
		panic(err)
	}
	return b
}

// Options configures an agent.
type Options struct {
	Cluster  string
	Identity *Identity
	// API is how the agent itself reaches this cluster's API server
	// (in a cluster: rest.InClusterConfig).
	API *rest.Config
	// Info describes the cluster; nil reports the name alone.
	Info func(ctx context.Context) Info
	// InfoInterval is how often Info is asked again (default 30 s).
	InfoInterval time.Duration
}

// Agent serves the streams the console opens.
type Agent struct {
	o    Options
	kube *http.Server
	ln   *streamListener
}

// New prepares an agent.
func New(o Options) (*Agent, error) {
	if o.Cluster == "" || o.Identity == nil || o.API == nil {
		return nil, errors.New("agent: cluster, identity and API config are required")
	}
	if o.InfoInterval <= 0 {
		o.InfoInterval = 30 * time.Second
	}
	h, err := kubeHandler(o.API)
	if err != nil {
		return nil, err
	}
	a := &Agent{o: o, ln: newStreamListener()}
	a.kube = &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	return a, nil
}

// Run connects to the console and serves until ctx ends. Of t, Dial and
// the timings are used; who the agent is and what it does with a stream
// are its own.
func (a *Agent) Run(ctx context.Context, t tunnel.ClientOptions) error {
	t.Hello = tunnel.Hello{Kind: Kind, Name: a.o.Cluster}
	t.Sign = a.o.Identity.Sign
	t.Handle = a.handle
	go func() { _ = a.kube.Serve(a.ln) }()
	defer func() { _ = a.kube.Close() }()
	return tunnel.NewClient(t).Run(ctx)
}

// handle is given every stream the console opens.
func (a *Agent) handle(ctx context.Context, purpose []byte, conn net.Conn) {
	var p Purpose
	if err := json.Unmarshal(purpose, &p); err != nil {
		_ = conn.Close()
		return
	}
	switch p.Kind {
	case "kube":
		a.ln.offer(conn) // served as HTTP by a.kube
	case "info":
		a.serveInfo(ctx, conn)
	default:
		_ = conn.Close() // not something this agent does
	}
}

// streamListener hands streams to an http.Server as accepted connections.
type streamListener struct {
	conns chan net.Conn
	done  chan struct{}
}

func newStreamListener() *streamListener {
	return &streamListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *streamListener) offer(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *streamListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *streamListener) Addr() net.Addr { return streamAddr{} }

type streamAddr struct{}

func (streamAddr) Network() string { return "tunnel" }
func (streamAddr) String() string  { return "tunnel" }
