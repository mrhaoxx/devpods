// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/gwlink"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// The console tells a gateway two things about a session it holds: let
// an admin watch it, and end it. A gateway next to the console is reached
// at its Pod's address; one elsewhere is not reachable at all — so every
// replica of a gateway of several clusters keeps connections to the
// console (as agents do), and the console speaks to it over those.

// gatewayLinkHost marks an address as "the replica <peer>, over its link".
const gatewayLinkHost = ".gateway-link.invalid"

// GatewayLinks accepts the gateways' connections.
type GatewayLinks struct {
	srv *tunnel.Server
}

// NewGatewayLinks: reader reads Gateway objects; instance and instances
// name this console replica among all of them.
func NewGatewayLinks(reader client.Reader, instance string, instances func() []string) *GatewayLinks {
	if instances == nil {
		instances = func() []string { return []string{instance} }
	}
	return &GatewayLinks{srv: tunnel.NewServer(tunnel.ServerOptions{
		Instance: instance, Instances: instances,
		Authorize: func(ctx context.Context, hello tunnel.Hello, nonce, proof []byte) (string, error) {
			name, replica, ok := strings.Cut(hello.Name, ".")
			if hello.Kind != gwlink.LinkKind || !ok || replica == "" || len(name) > MaxClusterNameLen || !dns1123Label.MatchString(name) {
				return "", errors.New("not a gateway")
			}
			var g devpodv1alpha1.Gateway
			if err := reader.Get(ctx, types.NamespacedName{Name: name}, &g); err != nil || g.Spec.Paused || g.Spec.PublicKey == "" {
				return "", errors.New("not a gateway this console knows")
			}
			if err := agent.VerifyFor(g.Spec.PublicKey, gwlink.LinkContext, nonce, proof); err != nil {
				return "", err
			}
			return hello.Name, nil
		},
	})}
}

// Accept runs one connection of a gateway until it ends.
func (g *GatewayLinks) Accept(ctx context.Context, conn net.Conn) { g.srv.Accept(ctx, conn) }

// Close drops every connection.
func (g *GatewayLinks) Close() { g.srv.Close() }

// AddrOf is the address to give an HTTP client for the gateway replica
// (Pod) of that name, if it holds a link to this console.
func (g *GatewayLinks) AddrOf(pod string) (string, bool) {
	for peer := range g.srv.Peers() {
		if _, replica, _ := strings.Cut(peer, "."); replica == pod {
			return peer + gatewayLinkHost + ":80", true
		}
	}
	return "", false
}

// DialContext is an http.Transport's dialer: an AddrOf address goes over
// the replica's link, anything else over the network.
func (g *GatewayLinks) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if peer, ok := strings.CutSuffix(host, gatewayLinkHost); err == nil && ok {
		conn, err := g.srv.Dial(ctx, peer, gwlink.PurposeLive)
		if errors.Is(err, tunnel.ErrPeerOffline) {
			return nil, fmt.Errorf("gateway replica %s is not connected to this console", peer)
		}
		return conn, err
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// Via has ls reach gateway replicas that hold a link over it, and the
// rest as before.
func (ls *LiveSessions) Via(g *GatewayLinks) {
	local := ls.resolve
	ls.resolve = func(ctx context.Context, pod string) (string, error) {
		if addr, ok := g.AddrOf(pod); ok {
			return addr, nil
		}
		return local(ctx, pod)
	}
	ls.client = &http.Client{Transport: &http.Transport{DialContext: g.DialContext}}
}

// handleGatewayConnect: GET /gateway/connect — one connection of a
// gateway replica, as a WebSocket.
func (s *Server) handleGatewayConnect(w http.ResponseWriter, r *http.Request) {
	if s.GatewayLinks == nil {
		http.NotFound(w, r)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(-1)
	s.GatewayLinks.Accept(r.Context(), websocket.NetConn(r.Context(), c, websocket.MessageBinary))
}
