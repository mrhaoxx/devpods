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
	"strconv"
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

// ?shared=1: my DevPods and those I am a collaborator of (the web's 我的
// DevPod shows both); without it, mine alone — what scripts and the SSH
// CLI's list act on.
func TestListDevPodsShared(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	t.Cleanup(func() { cleanupDevPods(t) })
	alice := forge(sm, "sh-alice", false)
	for _, body := range []string{
		`{"name":"open","image":"ubuntu:24.04","cpu":"2","memory":"4Gi","collaborators":["sh-bob"]}`,
		`{"name":"closed","image":"ubuntu:24.04","cpu":"2","memory":"4Gi"}`,
	} {
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "sh-bob", false), createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "sh-alice-open")
	waitDevPod(t, "sh-alice-closed")
	waitDevPod(t, "sh-bob-dev1")
	list := func(user, target string) string {
		t.Helper()
		rec := doJSON(t, s.HandleListDevPodsForTest(), "GET", target, nil, forge(sm, user, false), "")
		var body struct {
			Items []struct {
				Metadata struct{ Name string } `json:"metadata"`
			} `json:"items"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body)
		}
		var names []string
		for _, it := range body.Items {
			if strings.HasPrefix(it.Metadata.Name, "sh-") {
				names = append(names, it.Metadata.Name)
			}
		}
		return strings.Join(names, ",")
	}
	if got := list("sh-bob", "/api/devpods"); got != "sh-bob-dev1" {
		t.Fatalf("bob's own: %s", got)
	}
	if got := list("sh-bob", "/api/devpods?shared=1"); got != "sh-alice-open,sh-bob-dev1" {
		t.Fatalf("bob's with shared: %s", got)
	}
	if got := list("sh-carol", "/api/devpods?shared=1"); got != "" {
		t.Fatalf("carol's: %s", got)
	}
}

// The watch of 我的 DevPod with ?shared=1 carries those shared with me too.
func TestWatchShared(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	srv := httptest.NewServer(http.HandlerFunc(s.HandleWatchDevPodsForTest()))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/api/devpods?watch=true&shared=1", nil)
	req.AddCookie(forge(sm, "ws-bob", false))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	t.Cleanup(func() { cleanupDevPods(t) })
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "ws-alice", false),
		`{"name":"open","image":"ubuntu:24.04","cpu":"2","memory":"4Gi","collaborators":["ws-bob"]}`); rec.Code != http.StatusCreated {
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
			if strings.Contains(l, "ws-alice-open") {
				return
			}
		case <-ctx.Done():
			t.Fatal("bob's watch with shared=1 never saw the DevPod alice shares with him")
		}
	}
}

// A DevPod's shell is a bundled one, host, or a login command of the
// user's; one that would not be a single line is refused.
func TestCreateDevPodCustomShell(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	t.Cleanup(func() { cleanupDevPods(t) })
	alice := forge(sm, "csh-alice", false)
	for name, shell := range map[string]string{"tm": "tmux new -A -s main", "hs": "host", "zs": "/usr/bin/zsh"} {
		body := `{"name":"` + name + `","image":"ubuntu:24.04","cpu":"1","memory":"1Gi","shell":` + strconv.Quote(shell) + `}`
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body); rec.Code != http.StatusCreated {
			t.Fatalf("%q: %d %s", shell, rec.Code, rec.Body)
		}
		if dp := waitDevPod(t, "csh-alice-"+name); dp.Spec.Shell != shell {
			t.Fatalf("%q stored as %q", shell, dp.Spec.Shell)
		}
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice,
		`{"name":"bad","image":"ubuntu:24.04","cpu":"1","memory":"1Gi","shell":"zsh\nrm -rf /"}`); rec.Code == http.StatusCreated {
		t.Fatalf("a shell of two lines was taken")
	}
}
