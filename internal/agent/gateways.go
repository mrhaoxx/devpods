// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"slices"

	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// Gateway is a gateway the console tells the agent of: where to reach
// it, and the key it has to prove.
type Gateway struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	PublicKey string `json:"publicKey"`
}

// GatewayProofContext is what a gateway's proof to an agent is made for.
const GatewayProofContext = "devpod gateway accept v1"

// AtGatewayContext is what an agent's proof to the gateway of that name
// is made for. It is not the proof the console takes, nor the one another
// gateway takes: a gateway cannot pass an agent's proof on and be taken
// for that agent elsewhere.
func AtGatewayContext(gateway string) string { return "devpod agent at gateway v1:" + gateway }

// PurposeGateways: the console's list of gateways, as JSON values — the
// whole list each time it changes.
var PurposeGateways = mustPurpose("gateways")

type gatewayLink struct {
	gw     Gateway
	client *tunnel.Client
	cancel context.CancelFunc
}

// serveGateways follows the console's list. When the stream ends the
// gateways known so far are kept: sessions go on while the console is
// away.
func (a *Agent) serveGateways(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	dec := json.NewDecoder(conn)
	for {
		var list []Gateway
		if err := dec.Decode(&list); err != nil {
			return
		}
		a.setGateways(list)
	}
}

// setGateways dials out to the gateways of list and leaves the others.
func (a *Agent) setGateways(list []Gateway) {
	if a.o.DialGateway == nil {
		return
	}
	a.gmu.Lock()
	defer a.gmu.Unlock()
	want := map[string]Gateway{}
	for _, g := range list {
		if g.Name != "" && g.Address != "" && g.PublicKey != "" {
			want[g.Name] = g
		}
	}
	for name, l := range a.gateways {
		if g, ok := want[name]; !ok || g != l.gw {
			l.cancel() // gone, or moved, or re-keyed
			delete(a.gateways, name)
		}
	}
	for name, g := range want {
		if a.gateways[name] != nil {
			continue
		}
		opts := a.o.GatewayTunnel
		opts.Dial = func(ctx context.Context) (net.Conn, error) { return a.o.DialGateway(ctx, g.Address) }
		opts.Hello = tunnel.Hello{Kind: Kind, Name: a.o.Cluster}
		opts.Sign = func(nonce []byte) []byte { return a.o.Identity.SignFor(AtGatewayContext(g.Name), nonce) }
		opts.Handle = a.handleGateway
		// the gateway is the one the console named, or it gets nothing
		opts.VerifyServer = func(_ string, nonce, proof []byte) error {
			return VerifyFor(g.PublicKey, GatewayProofContext, nonce, proof)
		}
		opts.OnError = func(err error) {
			slog.Warn("agent_gateway", "cluster", a.o.Cluster, "gateway", g.Name, "err", err.Error())
		}
		ctx, cancel := context.WithCancel(a.runCtx)
		l := &gatewayLink{gw: g, client: tunnel.NewClient(opts), cancel: cancel}
		a.gateways[name] = l
		go func() { _ = l.client.Run(ctx) }()
	}
}

// connectedGateways names the gateways the agent holds a control
// connection on.
func (a *Agent) connectedGateways() []string {
	a.gmu.Lock()
	defer a.gmu.Unlock()
	var out []string
	for name, l := range a.gateways {
		for _, st := range l.client.Status() {
			if st.Control {
				out = append(out, name)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// handleGateway is given every stream a gateway opens: a gateway gets
// connections to DevPods, and nothing else of the cluster.
func (a *Agent) handleGateway(ctx context.Context, purpose []byte, conn net.Conn) {
	var p Purpose
	if err := json.Unmarshal(purpose, &p); err != nil || p.Kind != "dial" {
		_ = conn.Close()
		return
	}
	a.serveDial(ctx, p, conn)
}
