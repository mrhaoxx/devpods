// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package clusters is the console's side of the clusters it manages:
// their agents connect here, and everything the console does in a cluster
// goes through a stream to that cluster's agent.
package clusters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// Errors a caller tells apart.
var (
	ErrClusterOffline = errors.New("the cluster's agent is not connected")
	ErrUnknownCluster = errors.New("no such cluster")
)

// Options configures a Registry.
type Options struct {
	// Hub reads and writes the console's own cluster, where Cluster
	// objects live; Reader is its uncached reader.
	Hub    client.Client
	Reader client.Reader
	// Instance names this console instance among Instances.
	Instance  string
	Instances func() []string
	// StatusInterval: how often a connected cluster is checked again
	// (paused, removed, re-keyed) and its status refreshed. Default 30 s.
	StatusInterval time.Duration
	// Heartbeat and Dead are the tunnel's; zero = its defaults.
	Heartbeat, Dead time.Duration
	// GatewaysInterval: how often the list of gateways told to agents is
	// looked at again. Default 5 s.
	GatewaysInterval time.Duration

	// Scheme, when set, has the registry keep a client and a cache for
	// each cluster (see ClusterClients). Indexes registers field indexes
	// on a new cluster's cache; Warm are the kinds whose informers start
	// with it, so that they are there to read when the cluster is away.
	Scheme  *runtime.Scheme
	Indexes func(ctx context.Context, indexer client.FieldIndexer) error
	Warm    []client.Object
	// ByObject scopes a cluster's cache per kind (the DevPod namespace
	// for DevPods, Pods and Events), as the console's own cache is.
	ByObject map[client.Object]cache.ByObject
	// TelemetryInterval: how often the telemetry streams to each agent
	// are looked at (leading? enough open?). Default 1 s.
	TelemetryInterval time.Duration
}

// Registry accepts agents and opens streams to them.
type Registry struct {
	o   Options
	srv *tunnel.Server

	mu   sync.Mutex
	info map[string]agent.Info         // latest word from each connected cluster
	seen map[string]time.Time          // last moment each was connected here
	stop map[string]context.CancelFunc // per connected cluster: its info watcher
	ctx  context.Context

	clients  map[string]*ClusterClients
	building map[string]bool

	sink atomic.Pointer[telemetrySink] // SetTelemetry
}

// telemetrySink is where clusters' telemetry goes, while leading.
type telemetrySink struct {
	h       http.Handler
	leading func() bool
}

// New returns a Registry; Run must be called for status upkeep.
func New(o Options) *Registry {
	if o.StatusInterval <= 0 {
		o.StatusInterval = 30 * time.Second
	}
	if o.GatewaysInterval <= 0 {
		o.GatewaysInterval = 5 * time.Second
	}
	if o.TelemetryInterval <= 0 {
		o.TelemetryInterval = time.Second
	}
	if o.Instances == nil {
		o.Instances = func() []string { return []string{o.Instance} }
	}
	r := &Registry{o: o, info: map[string]agent.Info{}, seen: map[string]time.Time{}, stop: map[string]context.CancelFunc{}, ctx: context.Background(),
		clients: map[string]*ClusterClients{}, building: map[string]bool{}}
	r.srv = tunnel.NewServer(tunnel.ServerOptions{
		Instance: o.Instance, Instances: o.Instances,
		Authorize: r.authorize, OnPeer: r.onPeer,
		Heartbeat: o.Heartbeat, Dead: o.Dead,
	})
	return r
}

// Accept runs one connection from an agent until it ends.
func (r *Registry) Accept(ctx context.Context, conn net.Conn) { r.srv.Accept(ctx, conn) }

// authorize: an agent is who it says when its proof matches the key the
// cluster joined with, and the cluster is not paused.
func (r *Registry) authorize(ctx context.Context, hello tunnel.Hello, nonce, proof []byte) (string, error) {
	if hello.Kind != agent.Kind {
		return "", fmt.Errorf("%q does not connect here", hello.Kind)
	}
	c, err := r.cluster(ctx, hello.Name)
	if err != nil {
		return "", err
	}
	switch {
	case c.Spec.Paused:
		return "", errors.New("the cluster is paused")
	case c.Spec.AgentPublicKey == "":
		return "", errors.New("the cluster has not joined: start the agent with its join token")
	}
	if err := agent.Verify(c.Spec.AgentPublicKey, nonce, proof); err != nil {
		return "", err
	}
	return c.Name, nil
}

func (r *Registry) cluster(ctx context.Context, name string) (*devpodv1alpha1.Cluster, error) {
	var c devpodv1alpha1.Cluster
	if err := r.o.Reader.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrUnknownCluster
		}
		return nil, err
	}
	return &c, nil
}

// Dial opens a stream to a cluster's agent.
func (r *Registry) Dial(ctx context.Context, cluster string, purpose []byte) (net.Conn, error) {
	conn, err := r.srv.Dial(ctx, cluster, purpose)
	if errors.Is(err, tunnel.ErrPeerOffline) {
		return nil, ErrClusterOffline
	}
	return conn, err
}

// Connected reports whether the cluster's agent is connected to this
// instance, and when it last was.
func (r *Registry) Connected(cluster string) (bool, time.Time) {
	_, ok := r.srv.Peers()[cluster]
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		return true, time.Now()
	}
	return false, r.seen[cluster]
}

// Link is the cluster's agent's link to this instance: since when, its
// round trip and what it carries.
func (r *Registry) Link(cluster string) (tunnel.LinkStats, bool) {
	l, ok := r.srv.Stats()[cluster]
	return l, ok
}

// Info is the latest a connected cluster said about itself.
func (r *Registry) Info(cluster string) (agent.Info, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	in, ok := r.info[cluster]
	return in, ok
}

// Names lists the clusters connected to this instance, sorted.
func (r *Registry) Names() []string {
	peers := r.srv.Peers()
	out := make([]string, 0, len(peers))
	for name := range peers {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Disconnect drops a cluster's connections to this instance.
func (r *Registry) Disconnect(cluster string) { r.srv.Disconnect(cluster) }

func (r *Registry) onPeer(cluster string, up bool) {
	r.mu.Lock()
	r.seen[cluster] = time.Now()
	if cancel := r.stop[cluster]; cancel != nil {
		cancel()
		delete(r.stop, cluster)
	}
	var ctx context.Context
	if up {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(r.ctx)
		r.stop[cluster] = cancel
	} else {
		delete(r.info, cluster)
	}
	r.mu.Unlock()
	if up {
		go r.watchInfo(ctx, cluster)
		go r.tellGateways(ctx, cluster)
		go r.telemetry(ctx, cluster)
		go r.ensureClients(cluster)
	}
	// what is written is what holds at the time of writing, so the order
	// of two writes does not matter; the tunnel is not kept waiting
	go r.writeStatus(cluster)
}

// watchInfo keeps an info stream to the cluster and records what it says.
func (r *Registry) watchInfo(ctx context.Context, cluster string) {
	for ctx.Err() == nil {
		conn, err := r.srv.Dial(ctx, cluster, agent.PurposeInfo)
		if err != nil {
			return // offline: onPeer starts a new watcher when it is back
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		dec := json.NewDecoder(conn)
		for {
			var in agent.Info
			if err := dec.Decode(&in); err != nil {
				break
			}
			in.Cluster = cluster // the name it authenticated as, whatever it says
			r.mu.Lock()
			r.info[cluster] = in
			r.mu.Unlock()
			r.writeStatus(cluster)
		}
		stop()
		_ = conn.Close()
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
}

// gatewayList is what agents are told: the gateways that joined, are not
// paused and say where agents reach them.
func (r *Registry) gatewayList(ctx context.Context) ([]agent.Gateway, error) {
	var list devpodv1alpha1.GatewayList
	if err := r.o.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	out := []agent.Gateway{}
	for i := range list.Items {
		g := &list.Items[i]
		if !g.Spec.Paused && g.Spec.PublicKey != "" && g.Spec.AgentAddress != "" {
			out = append(out, agent.Gateway{Name: g.Name, Address: g.Spec.AgentAddress, PublicKey: g.Spec.PublicKey, InternalKey: g.Status.InternalKey})
		}
	}
	slices.SortFunc(out, func(a, b agent.Gateway) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// tellGateways keeps a stream to the cluster's agent on which the list
// of gateways is said: at once, and again whenever it changes.
func (r *Registry) tellGateways(ctx context.Context, cluster string) {
	for ctx.Err() == nil {
		conn, err := r.srv.Dial(ctx, cluster, agent.PurposeGateways)
		if err != nil {
			return // offline: onPeer starts over when it is back
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		enc := json.NewEncoder(conn)
		var last []agent.Gateway
		first := true
		tick := time.NewTicker(r.o.GatewaysInterval)
	stream:
		for {
			if list, err := r.gatewayList(ctx); err == nil {
				if first || !slices.Equal(list, last) {
					if enc.Encode(list) != nil {
						break stream
					}
					first, last = false, list
				} else if _, err := conn.Write([]byte("\n")); err != nil {
					break stream // (a blank line: the stream is not left silent)
				}
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				break stream
			}
		}
		tick.Stop()
		stop()
		_ = conn.Close()
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
}

// writeStatus records in the Cluster object what this instance sees. With
// several console instances each writes its own view; one that still holds
// the connection writes "connected" again within StatusInterval.
func (r *Registry) writeStatus(cluster string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// two writes can meet (a connect and the first word from the agent):
	// the one that loses reads again and writes what holds then
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c, err := r.cluster(ctx, cluster)
		if err != nil {
			return nil // gone, or the console's own API is away: the next sweep writes
		}
		connected, seen := r.Connected(cluster)
		in, _ := r.Info(cluster)
		st := devpodv1alpha1.ClusterStatus{
			Connected: connected, AgentVersion: in.AgentVersion, ControllerVersion: in.ControllerVersion,
			KubernetesVersion: in.KubernetesVersion, Capabilities: in.Capabilities, Gateways: in.Gateways,
		}
		if !seen.IsZero() {
			t := metav1.NewTime(seen.Truncate(time.Second))
			st.LastSeen = &t
		}
		if !connected { // keep what was last known of a cluster that is away
			st.AgentVersion, st.ControllerVersion = c.Status.AgentVersion, c.Status.ControllerVersion
			st.KubernetesVersion, st.Capabilities = c.Status.KubernetesVersion, c.Status.Capabilities
			st.Gateways = nil
		}
		c.Status = st
		return r.o.Hub.Status().Update(ctx, c)
	})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("cluster_status", "cluster", cluster, "err", err)
	}
}

// Run keeps the connected clusters honest until ctx ends: one that was
// paused, removed or given a new key since it connected is dropped, and
// every status is refreshed.
func (r *Registry) Run(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	tick := time.NewTicker(r.o.StatusInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.srv.Close()
			return
		case <-tick.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep does one round of Run's upkeep.
func (r *Registry) Sweep(ctx context.Context) {
	names := r.Names()
	r.mu.Lock()
	for name := range r.clients {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	r.mu.Unlock()
	for _, name := range names {
		c, err := r.cluster(ctx, name)
		switch {
		case errors.Is(err, ErrUnknownCluster): // no longer managed
			r.Disconnect(name)
			r.dropClients(name)
		case err != nil:
		case c.Spec.Paused || c.Spec.AgentPublicKey == "":
			r.Disconnect(name)
		default:
			if connected, _ := r.Connected(name); connected {
				go r.ensureClients(name) // if an earlier attempt failed
				r.writeStatus(name)
			}
		}
	}
}
