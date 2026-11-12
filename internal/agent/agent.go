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
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/client-go/rest"

	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// Kind is what an agent calls itself in the tunnel handshake.
const Kind = "agent"

// Purpose says what a stream is for.
type Purpose struct {
	Kind string `json:"kind"`
	// Addr and For: of a "dial" stream — a DevPod's address, and what
	// the connection is for (see DialAllowed).
	Addr string `json:"addr,omitempty"`
	For  string `json:"for,omitempty"`
	// UID of the DevPod the connection is to, and (for SSH) the User the
	// session is for.
	UID  string `json:"uid,omitempty"`
	User string `json:"user,omitempty"`
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

	// Gateways. The console names the gateways that serve this cluster;
	// the agent dials out to each (DialGateway makes one connection to a
	// gateway's address) and connects it to DevPods it asks for — the
	// DevPods of Namespace, read through DevPods. Without DialGateway
	// the agent has nothing to do with gateways.
	Namespace     string
	DevPods       client.Reader
	DialGateway   func(ctx context.Context, address string) (net.Conn, error)
	GatewayTunnel tunnel.ClientOptions // timings and pool size; the rest is the agent's
}

// Agent serves the streams the console opens.
type Agent struct {
	o    Options
	kube *http.Server
	ln   *tunnel.Listener

	runCtx   context.Context
	gmu      sync.Mutex
	gateways map[string]*gatewayLink
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
	a := &Agent{o: o, ln: tunnel.NewListener(), runCtx: context.Background(), gateways: map[string]*gatewayLink{}}
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
	if t.OnError == nil {
		t.OnError = func(err error) { slog.Warn("agent_connect", "cluster", a.o.Cluster, "err", err.Error()) }
	}
	rctx, stop := context.WithCancel(ctx)
	defer stop() // the connections to gateways end with the agent
	a.gmu.Lock()
	a.runCtx = rctx
	a.gmu.Unlock()
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
		a.ln.Offer(conn) // served as HTTP by a.kube
	case "info":
		a.serveInfo(ctx, conn)
	case "gateways":
		a.serveGateways(ctx, conn)
	case "dial": // the console asks for DevPods' state, not for connections to them
		answerDial(conn, errors.New("only a gateway is given connections to DevPods"))
		_ = conn.Close()
	default:
		_ = conn.Close() // not something this agent does
	}
}
