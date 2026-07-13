// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWatchStreamsOwnedEvents(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWatchDevPodsForTest()))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("GET", srv.URL+"/api/devpods?watch=true", nil)
	req.AddCookie(forge(sm, "gl-alice", false))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// Create one DevPod for alice and one for bob; only alice's may arrive.
	rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "gl-alice", false), createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec = doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "gl-bob", false),
		`{"name":"bobpod","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bob: %d %s", rec.Code, rec.Body)
	}
	t.Cleanup(func() { cleanupDevPods(t) })

	// Generous: under the full -race suite several envtest apiservers
	// run concurrently and informer delivery can lag.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if l := scanner.Text(); strings.HasPrefix(l, "data: ") {
				lines <- l
			}
		}
	}()
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "gl-bob-bobpod") {
				t.Fatalf("leaked bob's event to alice: %s", l)
			}
			if strings.Contains(l, "gl-alice-dev1") {
				return // success
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for alice's event")
		}
	}
}
