// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/ingest"
	"github.com/mrhaoxx/devpod/internal/webui/leader"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

// telemetryFlags configure the phase-2 live data layer (spec §5.10).
type telemetryFlags struct {
	ingestListen    string
	ingestKeyFile   string
	leaderNamespace string
	ingestService   string
	idleCPUPct      float64
	idleGPUPct      float64
	silentAfter     time.Duration
}

func addTelemetryFlags(fs *flag.FlagSet) *telemetryFlags {
	tf := &telemetryFlags{}
	fs.StringVar(&tf.ingestListen, "ingest-listen", fmt.Sprintf(":%d", render.IngestPort), "telemetry ingest + internal live listener")
	fs.StringVar(&tf.ingestKeyFile, "ingest-key-file", "", "shared telemetry ingest HMAC key; empty = telemetry off")
	fs.StringVar(&tf.leaderNamespace, "leader-election-namespace", "devpod-system", "namespace of the webui leader Lease and the ingest EndpointSlice")
	fs.StringVar(&tf.ingestService, "ingest-service", "devpod-webui-ingest", "selectorless Service the leader publishes itself behind")
	fs.Float64Var(&tf.idleCPUPct, "idle-cpu-pct", 5, "a DevPod is idle below this average CPU % (of its limit) over 24 h")
	fs.Float64Var(&tf.idleGPUPct, "idle-gpu-pct", 5, "… and below this average GPU utilisation %")
	fs.DurationVar(&tf.silentAfter, "agent-silent-after", 3*time.Minute, "alert agent_silent when a Running DevPod's telemetry stops this long")
	return tf
}

func (tf *telemetryFlags) enabled() bool { return tf.ingestKeyFile != "" }

func (tf *telemetryFlags) key() ([]byte, error) {
	b, err := os.ReadFile(tf.ingestKeyFile)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) < 32 {
		return nil, fmt.Errorf("ingest key %s: need >= 32 bytes", tf.ingestKeyFile)
	}
	return b, nil
}

// newElector builds this replica's elector; POD_NAME / POD_IP come from
// the downward API.
func (tf *telemetryFlags) newElector(cfg *rest.Config) (*leader.Elector, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	id := os.Getenv("POD_NAME")
	if id == "" {
		id, _ = os.Hostname()
	}
	return &leader.Elector{Client: cs, Namespace: tf.leaderNamespace, Name: "devpod-webui-leader", Identity: id}, nil
}

// publishSelf points the ingest Service at this pod while it leads,
// re-asserting every 30 s in case the slice was edited or deleted.
func (tf *telemetryFlags) publishSelf(ctx context.Context, c client.Client) {
	name, ip := os.Getenv("POD_NAME"), os.Getenv("POD_IP")
	if name == "" || ip == "" {
		slog.Error("telemetry leader: POD_NAME / POD_IP unset; ingest endpoint not published")
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if err := leader.PublishIngestEndpoint(ctx, c, tf.leaderNamespace, tf.ingestService, name, ip, render.IngestPort); err != nil {
			if ctx.Err() == nil {
				slog.Error("telemetry leader: publish ingest endpoint", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// startTelemetry runs the live layer: the :8081 listener (ingest and
// the leader's internal endpoints), the election, and — on the leader —
// the session resolver, the input refresher and the snapshot ticker;
// on followers the link to the leader. The returned wait blocks (up to
// 5 s) until the elector has released the lease after ctx ends; main
// must call it before exiting or a successor waits out the lease.
func (tf *telemetryFlags) startTelemetry(ctx context.Context, cfg *rest.Config, mgr manager.Manager, srv *webui.Server) (wait func(), err error) {
	key, err := tf.key()
	if err != nil {
		return nil, err
	}
	if srv.Audit == nil {
		return nil, errors.New("telemetry needs the audit store (--clickhouse-*)")
	}
	ch, ok := srv.Audit.Store.(*lazyCH)
	if !ok {
		return nil, errors.New("telemetry: unexpected audit store type")
	}
	el, err := tf.newElector(cfg)
	if err != nil {
		return nil, err
	}
	// The EndpointSlice lives in one namespace the webui may access; a
	// cached client would start a cluster-wide informer for it.
	direct, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, err
	}

	state := live.NewState(nil)
	hub := live.NewHub()
	hub.SetConnected(false)
	resolver := &ingest.Resolver{Source: ch}
	state.ResolveUser = resolver.User
	writer := ingest.NewWriter(ch)
	writer.IsLeader = el.IsLeader
	h := &ingest.Handler{
		Key: key, IsLeader: el.IsLeader, Writer: writer, State: state, Resolver: resolver,
		DevPods: func(uid string) (string, string, string, bool) {
			dp, cluster, ok := srv.DevPodByUID(context.Background(), uid)
			if !ok {
				return "", "", "", false
			}
			return dp.Spec.Owner, dp.Name, cluster, true
		},
		// on the DevPod, in the cluster it is in
		Events: func(uid, kind, detail string) {
			if err := srv.DevPodEvent(context.Background(), uid, corev1.EventTypeWarning, "TelemetryAlert", kind+": "+detail); err != nil {
				slog.Warn("telemetry: devpod event", "uid", uid, "err", err)
			}
		},
	}

	// the telemetry of the clusters that join: over their agents' links,
	// into this receiver, while this replica leads
	if srv.Clusters != nil {
		srv.Clusters.SetTelemetry(h, el.IsLeader)
	}

	token := wire.Token(key, "internal-live")
	addr := func() (string, error) {
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return leader.LeaderAddr(actx, direct, tf.leaderNamespace, tf.ingestService)
	}
	inputs := &webui.LiveInputs{S: srv}
	running := func(ctx context.Context) []leader.RunningDevPod {
		var out []leader.RunningDevPod
		for _, d := range inputs.Build(ctx).DevPods {
			if d.Phase == "Running" {
				out = append(out, leader.RunningDevPod{UID: d.UID, Name: d.Name, Cluster: d.Cluster, PodStart: d.PhaseSince})
			}
		}
		return out
	}
	srv.Live = &webui.LiveDeps{Hub: hub, Source: live.SwitchSource{
		IsLeader: el.IsLeader,
		Local:    live.LocalSource{State: state},
		Remote:   &live.RemoteSource{Addr: addr, Token: token},
	}, Snapshot: func(ctx context.Context) (live.Snapshot, bool) {
		if el.IsLeader() {
			return state.Snapshot(inputs.Build(ctx)), true
		}
		return hub.Last()
	}}

	mux := http.NewServeMux()
	mux.Handle("POST /ingest/v1/batch", h)
	mux.Handle("GET "+live.LivePath, live.LiveHandler(hub, token, el.IsLeader))
	mux.Handle("GET "+live.DevPodPath, live.DevPodHandler(state, token, el.IsLeader))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	// CPU / heap profiles for sizing the leader, behind the replicas' token.
	mux.Handle("GET /internal/debug/pprof/", requireToken(token, http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /internal/debug/pprof/profile", requireToken(token, http.HandlerFunc(pprof.Profile)))
	ingestSrv := &http.Server{Addr: tf.ingestListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ingestSrv.Shutdown(sctx)
	}()
	go func() {
		slog.Info("telemetry ingest listening", "addr", tf.ingestListen)
		if err := ingestSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(err)
		}
	}()

	onStart := func(lctx context.Context) {
		slog.Info("telemetry: this replica leads")
		go tf.publishSelf(lctx, direct)
		go resolver.Run(lctx)
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				if err := inputs.Refresh(lctx); err != nil && lctx.Err() == nil {
					slog.Warn("telemetry: refresh presence/alerts", "err", err)
				}
				// prune deleted DevPods — not when the list failed (a failed
				// list is not "every DevPod is gone")
				if keep, err := inputs.UIDs(lctx); err == nil {
					state.Retain(func(uid string) bool { return keep[uid] })
				}
				select {
				case <-lctx.Done():
					return
				case <-t.C:
				}
			}
		}()
		go live.Tick(lctx, hub, time.Second, func() live.Snapshot { return state.Snapshot(inputs.Build(lctx)) })
		jobs := &leader.Jobs{Store: ch, Running: running, LastSample: state.LastSample, SetIdle: inputs.SetIdle, Event: h.Events,
			IdleCPUPct: tf.idleCPUPct, IdleGPUPct: tf.idleGPUPct, SilentAfter: tf.silentAfter, Since: time.Now()}
		go jobs.Run(lctx)
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-lctx.Done():
					return
				case <-t.C:
					st := writer.TakeStats()
					slog.Info("telemetry writer", "flushes", st.Flushes, "rows", st.Rows, "errors", st.Errors,
						"p50", st.P50.Round(time.Millisecond), "p99", st.P99.Round(time.Millisecond), "max", st.Max.Round(time.Millisecond))
				}
			}
		}()
	}
	onStop := func() { slog.Info("telemetry: leadership lost") }
	elDone := make(chan struct{})
	go func() {
		el.Run(ctx, onStart, onStop) // releases the lease when ctx ends
		close(elDone)
	}()
	go followWhileNotLeading(ctx, el, addr, token, hub)
	return func() {
		select {
		case <-elDone:
		case <-time.After(5 * time.Second):
		}
	}, nil
}

// followWhileNotLeading relays the leader's snapshots whenever this
// replica is a follower.
func followWhileNotLeading(ctx context.Context, el *leader.Elector, addr func() (string, error), token string, hub *live.Hub) {
	for ctx.Err() == nil {
		if el.IsLeader() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		fctx, cancel := context.WithCancel(ctx)
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-fctx.Done():
					return
				case <-t.C:
					if el.IsLeader() {
						cancel()
						return
					}
				}
			}
		}()
		live.Follow(fctx, addr, token, hub)
		cancel()
	}
}

func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// pprof.Index serves /debug/pprof/<name>; map our prefix onto it
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.Replace(r.URL.Path, "/internal/debug/pprof/", "/debug/pprof/", 1)
		next.ServeHTTP(w, r2)
	})
}
