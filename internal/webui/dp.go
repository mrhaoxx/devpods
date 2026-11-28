// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"

	"github.com/mrhaoxx/devpod/internal/gatewaycli"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// dp, the command inside every DevPod (design 2026-11-26, stage 2): it
// posts what it was asked to the console — over its cluster's agent, or
// to the console's own listener in the console's own cluster — with the
// DevPod's credential (wire.DPToken of its UID, which only a controller
// holding the telemetry key can make). The console runs the CLI for the
// DevPod's owner, about that DevPod, and sends back what it writes as it
// writes it: one JSON line a piece — {"o": stdout}, {"e": stderr} — and
// last {"x": exit code}.

// dpMaxBody bounds one call (argv, cwd and the files it names).
const dpMaxBody = 4 << 20

type dpRequest struct {
	Argv  []string          `json:"argv"`
	Cwd   string            `json:"cwd"`
	Files map[string]string `json:"files"`
	Env   map[string]string `json:"env"` // the shell's, with a submit
}

// HandleDP serves POST /dp/v1/cli.
func (s *Server) HandleDP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.CLI == nil {
		http.Error(w, "dp is not available on this console", http.StatusServiceUnavailable)
		return
	}
	dp, cluster, ok := s.dpCaller(w, r)
	if !ok {
		return
	}
	var req dpRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, dpMaxBody)).Decode(&req); err != nil {
		http.Error(w, "bad call: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fw := &frameWriter{w: w}
	fl, _ := w.(http.Flusher)
	fw.flush = func() {
		if fl != nil {
			fl.Flush()
		}
	}
	origin := &gatewaycli.Origin{Cluster: cluster, DevPod: dp.Name, Cwd: req.Cwd, Env: req.Env}
	// dp says it can attach (it reached /dp/v1/attach first), and whether
	// it is on a terminal: a job's ranks it submits wait for it, and it is
	// told when to attach — a frame {"a": …} of its own
	if r.Header.Get("X-DP-Attach") == "1" {
		origin.Attach = func(a gatewaycli.AttachTo) { fw.frame(map[string]any{"a": a}) }
		origin.TTY = r.Header.Get("X-DP-TTY") == "1"
		origin.Term = r.Header.Get("X-DP-Term")
	}
	code := s.CLI.Exec(r.Context(), gatewaycli.Meta{User: dp.Spec.Owner, Remote: r.RemoteAddr, AuthKind: "dp"}, req.Argv, origin, req.Files, fw.stream("o"), fw.stream("e"))
	fw.frame(map[string]any{"x": code})
}

// dpCaller: the DevPod a dp call is from — its UID and its dp credential,
// which only a controller holding the key can make — and its cluster; an
// answer (and false) when it is none.
func (s *Server) dpCaller(w http.ResponseWriter, r *http.Request) (*devpodv1alpha1.DevPod, string, bool) {
	if len(s.IngestKey) == 0 {
		http.Error(w, "dp is not available on this console", http.StatusServiceUnavailable)
		return nil, "", false
	}
	uid := r.Header.Get("X-DevPod-UID")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if uid == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(wire.DPToken(s.IngestKey, uid))) != 1 {
		http.Error(w, "not a DevPod's dp", http.StatusUnauthorized)
		return nil, "", false
	}
	dp, cluster, ok := s.DevPodByUID(r.Context(), uid)
	if !ok {
		http.Error(w, "no such DevPod", http.StatusUnauthorized)
		return nil, "", false
	}
	// over a cluster's link, only that cluster's DevPods: one cluster's
	// controller (which holds the key) speaks for no other's
	if link, over := clusters.LinkCluster(r.Context()); over && link != cluster {
		slog.Warn("dp_refused", "devpod", dp.Name, "cluster", cluster, "link", link)
		http.Error(w, "not a DevPod of this cluster", http.StatusUnauthorized)
		return nil, "", false
	}
	return dp, cluster, true
}

// frameWriter writes JSON lines, one per write, flushed at once; stdout
// and stderr share it.
type frameWriter struct {
	mu    sync.Mutex
	w     io.Writer
	flush func()
}

func (f *frameWriter) frame(v any) {
	b, _ := json.Marshal(v)
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.w.Write(append(b, '\n'))
	f.flush()
}

type frameStream struct {
	f   *frameWriter
	key string
}

func (f *frameWriter) stream(key string) io.Writer { return frameStream{f, key} }

func (s frameStream) Write(p []byte) (int, error) {
	s.f.frame(map[string]string{s.key: string(p)})
	return len(p), nil
}
