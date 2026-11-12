// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/mrhaoxx/devpod/internal/agent"
)

// ClusterClients is how the console reads and writes one cluster: every
// request is a stream to that cluster's agent.
type ClusterClients struct {
	Name string
	// Client reads from Cache and writes to the cluster.
	Client client.Client
	// Reader always asks the cluster.
	Reader client.Reader
	Cache  cache.Cache

	cancel context.CancelFunc
}

// A cluster's clients are built when its agent first connects and kept
// while it is away: reads go on answering from the cache (Stale says
// since when), writes and uncached reads fail at once with
// ErrClusterOffline, and when the agent returns the informers catch up by
// themselves.

// restConfig reaches a cluster's API server through its agent.
func (r *Registry) restConfig(cluster string) *rest.Config {
	return &rest.Config{
		Host: "http://" + cluster, // never resolved: Dial is the way there
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return r.Dial(ctx, cluster, agent.PurposeKube)
		},
		QPS: 50, Burst: 100,
	}
}

func (r *Registry) buildClients(ctx context.Context, cluster string) (*ClusterClients, error) {
	cfg := r.restConfig(cluster)
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, hc)
	if err != nil {
		return nil, err
	}
	ca, err := cache.New(cfg, cache.Options{Scheme: r.o.Scheme, Mapper: mapper, HTTPClient: hc, ByObject: r.o.ByObject})
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithCancel(ctx)
	fail := func(err error) (*ClusterClients, error) {
		cancel()
		return nil, err
	}
	if r.o.Indexes != nil {
		if err := r.o.Indexes(cctx, ca); err != nil {
			return fail(err)
		}
	}
	for _, obj := range r.o.Warm {
		if _, err := ca.GetInformer(cctx, obj, cache.BlockUntilSynced(false)); err != nil {
			return fail(err)
		}
	}
	go func() {
		if err := ca.Start(cctx); err != nil {
			slog.Warn("cluster_cache", "cluster", cluster, "err", err)
		}
	}()
	sctx, done := context.WithTimeout(cctx, time.Minute)
	defer done()
	if !ca.WaitForCacheSync(sctx) {
		return fail(fmt.Errorf("the cache of cluster %s did not sync", cluster))
	}
	opts := client.Options{Scheme: r.o.Scheme, Mapper: mapper, HTTPClient: hc}
	reader, err := client.New(cfg, opts)
	if err != nil {
		return fail(err)
	}
	opts.Cache = &client.CacheOptions{Reader: ca}
	cached, err := client.New(cfg, opts)
	if err != nil {
		return fail(err)
	}
	return &ClusterClients{Name: cluster, Client: cached, Reader: reader, Cache: ca, cancel: cancel}, nil
}

// ensureClients builds a cluster's clients if there are none yet.
func (r *Registry) ensureClients(cluster string) {
	r.mu.Lock()
	if r.o.Scheme == nil || r.clients[cluster] != nil || r.building[cluster] {
		r.mu.Unlock()
		return
	}
	r.building[cluster] = true
	ctx := r.ctx
	r.mu.Unlock()

	cc, err := r.buildClients(ctx, cluster)

	r.mu.Lock()
	delete(r.building, cluster)
	if err == nil {
		r.clients[cluster] = cc
	}
	r.mu.Unlock()
	if err != nil { // tried again at the next sweep, or when the agent reconnects
		slog.Warn("cluster_clients", "cluster", cluster, "err", err)
	}
}

// dropClients forgets a cluster that is no longer managed.
func (r *Registry) dropClients(cluster string) {
	r.mu.Lock()
	cc := r.clients[cluster]
	delete(r.clients, cluster)
	r.mu.Unlock()
	if cc != nil {
		cc.cancel()
	}
}

// Clients returns a cluster's clients. A cluster whose agent has not
// connected since this console started is ErrClusterOffline; one that is
// away after having connected still has its clients (see Stale).
func (r *Registry) Clients(name string) (*ClusterClients, error) {
	r.mu.Lock()
	cc := r.clients[name]
	r.mu.Unlock()
	if cc != nil {
		return cc, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.cluster(ctx, name); err != nil {
		return nil, err
	}
	return nil, ErrClusterOffline
}

// Each calls fn for every cluster that has clients, by name: the
// connected ones and those that are away with a cache still to read.
func (r *Registry) Each(fn func(*ClusterClients)) {
	r.mu.Lock()
	all := make([]*ClusterClients, 0, len(r.clients))
	for _, cc := range r.clients {
		all = append(all, cc)
	}
	r.mu.Unlock()
	slices.SortFunc(all, func(a, b *ClusterClients) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	for _, cc := range all {
		fn(cc)
	}
}

// Stale reports whether what is read of a cluster comes from a cache its
// agent no longer feeds, and since when.
func (r *Registry) Stale(name string) (since time.Time, stale bool) {
	connected, seen := r.Connected(name)
	r.mu.Lock()
	has := r.clients[name] != nil
	r.mu.Unlock()
	if connected || !has {
		return time.Time{}, false
	}
	return seen, true
}
