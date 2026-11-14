// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"

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
	srv    *tunnel.Server
	client *http.Client // to a replica, over its link
}

// NewGatewayLinks: reader reads Gateway objects; instance and instances
// name this console replica among all of them.
func NewGatewayLinks(reader client.Reader, instance string, instances func() []string) *GatewayLinks {
	if instances == nil {
		instances = func() []string { return []string{instance} }
	}
	g := &GatewayLinks{}
	g.client = &http.Client{Transport: &http.Transport{DialContext: g.DialContext}, Timeout: 4 * time.Second}
	g.srv = tunnel.NewServer(tunnel.ServerOptions{
		Instance: instance, Instances: instances,
		Authorize: func(ctx context.Context, hello tunnel.Hello, nonce, proof []byte) (string, error) {
			name, replica, ok := strings.Cut(hello.Name, ".")
			if hello.Kind != gwlink.LinkKind || !ok || len(name) > MaxClusterNameLen || !dns1123Label.MatchString(name) ||
				len(replica) > 253 || !dns1123Subdomain.MatchString(replica) {
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
	})
	return g
}

// Replicas are the replicas of gateway connected to this console, by Pod,
// with their links to it.
func (g *GatewayLinks) Replicas(gateway string) map[string]tunnel.LinkStats {
	out := map[string]tunnel.LinkStats{}
	for peer, l := range g.srv.Stats() {
		if pod, ok := strings.CutPrefix(peer, gateway+"."); ok && gateway != "" {
			out[pod] = l
		}
	}
	return out
}

// Status asks a replica of gateway what it holds now (gwlink.Status).
func (g *GatewayLinks) Status(ctx context.Context, gateway, pod string) (*gwlink.Status, error) {
	addr, ok := g.AddrOf(gateway, pod)
	if !ok {
		return nil, fmt.Errorf("gateway %s (replica %s) is not connected to this console", gateway, pod)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+gwlink.StatusPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the replica answered %s (a gateway of before v0.9.23 has no status)", resp.Status)
	}
	var st gwlink.Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Accept runs one connection of a gateway until it ends.
func (g *GatewayLinks) Accept(ctx context.Context, conn net.Conn) { g.srv.Accept(ctx, conn) }

// Close drops every connection.
func (g *GatewayLinks) Close() { g.srv.Close() }

// AddrOf is the address to give an HTTP client for the replica (Pod) of
// that gateway, if it holds a link to this console. The gateway's name is
// the one it proved with its key: a replica of one gateway is never taken
// for a replica of another.
func (g *GatewayLinks) AddrOf(gateway, pod string) (string, bool) {
	peer := gateway + "." + pod
	if _, ok := g.srv.Peers()[peer]; ok && gateway != "" && pod != "" {
		return peer + gatewayLinkHost + ":80", true
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

// DialSSH is an SSH connection to the gateway: a stream over the link
// of one of its replicas, any one that holds a link to this console.
func (g *GatewayLinks) DialSSH(ctx context.Context, gateway string) (net.Conn, error) {
	var replicas []string
	for peer := range g.srv.Peers() {
		if strings.HasPrefix(peer, gateway+".") {
			replicas = append(replicas, peer)
		}
	}
	if gateway == "" || len(replicas) == 0 {
		return nil, fmt.Errorf("no replica of gateway %q is connected to this console", gateway)
	}
	return g.srv.Dial(ctx, replicas[rand.IntN(len(replicas))], gwlink.PurposeSSH)
}

// Via has ls reach the replicas of gateways of several clusters over
// their links, and the rest as before.
func (ls *LiveSessions) Via(g *GatewayLinks) {
	ls.links = g
	ls.client = &http.Client{Transport: &http.Transport{DialContext: g.DialContext}}
}

// addrOf is where the replica holding a session is reached: over its
// link when the session came in at a gateway of several clusters (its
// name is in the session's record), else at its address in this cluster.
func (ls *LiveSessions) addrOf(ctx context.Context, gateway, pod string) (string, error) {
	if ls.links != nil && gateway != "" {
		if addr, ok := ls.links.AddrOf(gateway, pod); ok {
			return addr, nil
		}
		return "", fmt.Errorf("gateway %s (replica %s) is not connected to this console", gateway, pod)
	}
	return ls.resolve(ctx, pod)
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
