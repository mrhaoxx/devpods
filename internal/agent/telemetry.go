// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// TelemetryPath is where DevPods post their telemetry batches.
const TelemetryPath = "/ingest/v1/batch"

// MaxTelemetryBatch bounds one forwarded batch.
const MaxTelemetryBatch = 16 << 20

// TelemetryForward passes telemetry batches posted in the cluster on to
// the console's receiver at target: the DevPods of a managed cluster have
// no way out of it of their own, the agent has. The batch, who it says it
// is from (the receiver checks that) and the receiver's answer pass; no
// other path, method or header does.
func TelemetryForward(target string, hc *http.Client) http.Handler {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TelemetryPath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxTelemetryBatch))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "cannot read the batch", http.StatusBadRequest)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "bad receiver address", http.StatusInternalServerError)
			return
		}
		for name, vals := range r.Header { // the batch's own: its form, and who it says it is from
			switch {
			case name == "Authorization", name == "Content-Type", name == "Content-Encoding", strings.HasPrefix(name, "X-Devpod-"):
				out.Header[name] = vals
			}
		}
		resp, err := hc.Do(out)
		if err != nil {
			http.Error(w, "the telemetry receiver cannot be reached", http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for _, h := range []string{"Content-Type", "Retry-After"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
	})
}
