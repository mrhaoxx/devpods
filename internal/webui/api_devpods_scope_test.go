// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListDevPodsScopeAll(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	t.Cleanup(func() { cleanupDevPods(t) })
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "sc-alice", false), createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "sc-bob", false), createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "sc-alice-dev1")
	waitDevPod(t, "sc-bob-dev1")
	list := func(c *http.Cookie, target string, want int) []string {
		t.Helper()
		rec := doJSON(t, s.HandleListDevPodsForTest(), "GET", target, nil, c, "")
		if rec.Code != want {
			t.Fatalf("GET %s: %d (want %d) %s", target, rec.Code, want, rec.Body)
		}
		var body struct {
			Items []struct {
				Metadata struct{ Name string } `json:"metadata"`
			} `json:"items"`
			Pods map[string]any `json:"pods"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		var names []string
		for _, it := range body.Items {
			if strings.HasPrefix(it.Metadata.Name, "sc-") {
				names = append(names, it.Metadata.Name)
			}
		}
		return names
	}
	if got := list(forge(sm, "sc-root", true), "/api/devpods?scope=all", http.StatusOK); strings.Join(got, ",") != "sc-alice-dev1,sc-bob-dev1" {
		t.Fatalf("admin scope=all = %v", got)
	}
	if got := list(forge(sm, "sc-alice", false), "/api/devpods", http.StatusOK); strings.Join(got, ",") != "sc-alice-dev1" {
		t.Fatalf("mine = %v", got)
	}
	list(forge(sm, "sc-alice", false), "/api/devpods?scope=all", http.StatusForbidden)
	list(forge(sm, "sc-root", true), "/api/devpods?scope=everyone", http.StatusBadRequest)
}

func TestWatchScopeAll(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	srv := httptest.NewServer(http.HandlerFunc(s.HandleWatchDevPodsForTest()))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/api/devpods?watch=true&scope=all", nil)
	req.AddCookie(forge(sm, "sw-root", true))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	t.Cleanup(func() { cleanupDevPods(t) })
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "sw-bob", false), createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "sw-bob-dev1") {
				return
			}
		case <-ctx.Done():
			t.Fatal("admin scope=all watch never saw bob's DevPod")
		}
	}
}
