// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrhaoxx/devpod/internal/agent"
)

// DevPods of a managed cluster send their telemetry to the agent, in the
// cluster; the agent passes each batch on to the console's receiver —
// the batch, who it is from, and the answer back — and nothing else.
func TestTelemetryForward(t *testing.T) {
	type got struct {
		method, path, auth, ctype, enc, cookie, uid string
		body                                        []byte
	}
	var seen []got
	status := http.StatusAccepted
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, got{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), r.Header.Get("Cookie"), r.Header.Get("X-DevPod-UID"), b})
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "noted")
	}))
	defer central.Close()
	fwd := httptest.NewServer(agent.TelemetryForward(central.URL+"/ingest/v1/batch", nil))
	defer fwd.Close()

	post := func(path string, body []byte, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, fwd.URL+path, bytes.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	batch := []byte("\x1f\x8b a batch, as sent")
	resp := post("/ingest/v1/batch", batch, map[string]string{"Authorization": "Bearer uid.key", "Content-Type": "application/x-protobuf", "Content-Encoding": "zstd", "Cookie": "session=x", "X-DevPod-UID": "uid-1"})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted || string(b) != "noted" || resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("the answer passed back: %d %q %q", resp.StatusCode, b, resp.Header.Get("Retry-After"))
	}
	if len(seen) != 1 || seen[0].method != "POST" || seen[0].path != "/ingest/v1/batch" || seen[0].auth != "Bearer uid.key" ||
		seen[0].ctype != "application/x-protobuf" || seen[0].enc != "zstd" || seen[0].uid != "uid-1" || !bytes.Equal(seen[0].body, batch) {
		t.Fatalf("what reached the receiver: %+v", seen)
	}
	if seen[0].cookie != "" {
		t.Fatal("a header that is not the batch's was passed on")
	}
	// the receiver's refusals are the sender's to see
	status = http.StatusTooManyRequests
	if resp := post("/ingest/v1/batch", batch, nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a refusal: %d", resp.StatusCode)
	}
	// nothing else is forwarded: no other path, no other method, nothing oversized
	n := len(seen)
	if resp := post("/api/admin/users", batch, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another path: %d", resp.StatusCode)
	}
	if resp, err := http.Get(fwd.URL + "/ingest/v1/batch"); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a GET: %v %v", resp, err)
	}
	if resp := post("/ingest/v1/batch", []byte(strings.Repeat("x", agent.MaxTelemetryBatch+1)), nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized batch: %d", resp.StatusCode)
	}
	if len(seen) != n {
		t.Fatalf("%d requests reached the receiver that should not have", len(seen)-n)
	}
	// the receiver being away is a 502, soon
	central.Close()
	if resp := post("/ingest/v1/batch", batch, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("receiver away: %d", resp.StatusCode)
	}
}
