// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// The whole path a cluster takes: an admin registers it, its agent joins
// with the token and connects over a WebSocket, everyone sees it.
func TestClusters(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Origin = "https://console.example.com"
	s.Clusters = clusters.New(clusters.Options{
		Hub: k8sClient, Reader: k8sManager.GetAPIReader(), Instance: "console-0",
		StatusInterval: time.Hour, Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond,
	})
	go s.Clusters.Run(ctx)
	web := httptest.NewServer(s.Routes())
	defer web.Close()

	admin, alice := forge(sm, "root", true), forge(sm, "gl-alice", false)
	call := func(method, path string, cookie *http.Cookie, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, web.URL+path, strings.NewReader(body))
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// only admins register clusters
	if code, _ := call("POST", "/api/admin/clusters", alice, `{"name":"mulan"}`); code != http.StatusForbidden {
		t.Fatalf("a user registering a cluster: %d", code)
	}
	if code, _ := call("GET", "/api/admin/clusters", alice, ""); code != http.StatusForbidden {
		t.Fatalf("a user on the admin list: %d", code)
	}
	for _, bad := range []string{`{"name":"devpod"}`, `{"name":"Bad_Name"}`, `{"name":"` + strings.Repeat("a", 33) + `"}`, `{}`} {
		if code, out := call("POST", "/api/admin/clusters", admin, bad); code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", bad, code, out)
		}
	}
	code, out := call("POST", "/api/admin/clusters", admin, `{"name":"mulan","displayName":"木兰"}`)
	if code != http.StatusCreated {
		t.Fatalf("register: %d %v", code, out)
	}
	token, _ := out["joinToken"].(string)
	command, _ := out["command"].(string)
	if !strings.HasPrefix(token, "dpj_") || !strings.Contains(command, token) || !strings.Contains(command, "mulan") || !strings.Contains(command, "https://console.example.com") {
		t.Fatalf("token %q command %q", token, command)
	}
	if code, _ := call("POST", "/api/admin/clusters", admin, `{"name":"mulan"}`); code != http.StatusConflict {
		t.Fatalf("registering the same name twice: %d", code)
	}
	// the token is not kept: not on the object, not in any later answer
	var obj devpodv1alpha1.Cluster
	if err := k8sManager.GetAPIReader().Get(ctx, types.NamespacedName{Name: "mulan"}, &obj); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(obj); strings.Contains(string(raw), token) {
		t.Fatal("the join token is stored in the clear")
	}
	_, out = call("GET", "/api/admin/clusters", admin, "")
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), token) || !strings.Contains(string(raw), `"joined":false`) {
		t.Fatalf("admin list: %s", raw)
	}

	// the agent joins: no session, no cookie — the token is the credential
	id, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	if err != nil {
		t.Fatal(err)
	}
	join := func(tok, key string) int {
		body, _ := json.Marshal(map[string]string{"cluster": "mulan", "token": tok, "publicKey": key})
		code, _ := call("POST", "/agent/join", nil, string(body))
		return code
	}
	if code := join("dpj_guess", id.PublicKey()); code != http.StatusForbidden {
		t.Fatalf("join with a wrong token: %d", code)
	}
	if code := join(token, id.PublicKey()); code != http.StatusNoContent {
		t.Fatalf("join: %d", code)
	}
	other, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	if code := join(token, other.PublicKey()); code != http.StatusForbidden {
		t.Fatalf("the token a second time: %d", code)
	}

	// and connects, over a WebSocket as it would from another cluster
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()
	a, err := agent.New(agent.Options{Cluster: "mulan", Identity: id, API: &rest.Config{Host: api.URL},
		Info: func(context.Context) agent.Info {
			return agent.Info{AgentVersion: "v0.7.0", Capabilities: []string{"snapshots"}}
		}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "ws" + strings.TrimPrefix(web.URL, "http") + "/agent/connect"
	go func() {
		_ = a.Run(ctx, tunnel.ClientOptions{
			Idle: 2, Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond, BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
			Dial: func(context.Context) (net.Conn, error) {
				c, _, err := websocket.Dial(ctx, endpoint, nil)
				if err != nil {
					return nil, err
				}
				c.SetReadLimit(-1)
				return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
			},
		})
	}()
	find := func(cookie *http.Cookie, path string) map[string]any {
		_, out := call("GET", path, cookie, "")
		items, _ := out["items"].([]any)
		for _, it := range items {
			if m, _ := it.(map[string]any); m["name"] == "mulan" {
				return m
			}
		}
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m := find(alice, "/api/clusters"); m != nil && m["connected"] == true && m["capabilities"] != nil {
			if m["displayName"] != "木兰" {
				t.Fatalf("what a user sees: %v", m)
			}
			// a user is not told what an admin is
			for _, k := range []string{"agentVersion", "joined", "defaultQuota", "tokenExpires"} {
				if _, has := m[k]; has {
					t.Fatalf("a user sees %s", k)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster never showed as connected: %v", find(alice, "/api/clusters"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m := find(admin, "/api/admin/clusters"); m["agentVersion"] != "v0.7.0" || m["joined"] != true {
		t.Fatalf("what an admin sees: %v", m)
	}

	// edit: a quota for the cluster, a new display name
	if code, out := call("PATCH", "/api/admin/clusters/mulan", admin, `{"displayName":"Mulan","defaultQuota":{"maxDevPods":3,"cpu":"64"}}`); code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, out)
	}
	if m := find(admin, "/api/admin/clusters"); m["displayName"] != "Mulan" || m["defaultQuota"] == nil {
		t.Fatalf("after the edit: %v", m)
	}
	if code, _ := call("PATCH", "/api/admin/clusters/mulan", admin, `{"defaultQuota":{"cpu":"lots"}}`); code != http.StatusBadRequest {
		t.Fatalf("a bad quota: %d", code)
	}
	if code, _ := call("PATCH", "/api/admin/clusters/mulan", alice, `{"paused":true}`); code != http.StatusForbidden {
		t.Fatalf("a user pausing a cluster: %d", code)
	}

	// pause: the agent is dropped at once and stays out
	if code, _ := call("PATCH", "/api/admin/clusters/mulan", admin, `{"paused":true}`); code != http.StatusOK {
		t.Fatalf("pause: %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if ok, _ := s.Clusters.Connected("mulan"); ok {
		t.Fatal("a paused cluster is still connected")
	}
	if m := find(alice, "/api/clusters"); m["connected"] != false || m["paused"] != true {
		t.Fatalf("a paused cluster as shown: %v", m)
	}
	if code, _ := call("PATCH", "/api/admin/clusters/mulan", admin, `{"paused":false}`); code != http.StatusOK {
		t.Fatal("resume")
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		if ok, _ := s.Clusters.Connected("mulan"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent did not come back after the cluster was resumed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// a new token withdraws the agent's key
	code, out = call("POST", "/api/admin/clusters/mulan/token", admin, "")
	if token2, _ := out["joinToken"].(string); code != http.StatusOK || token2 == token || !strings.HasPrefix(token2, "dpj_") {
		t.Fatalf("reissue: %d %v", code, out)
	}
	time.Sleep(300 * time.Millisecond)
	if ok, _ := s.Clusters.Connected("mulan"); ok {
		t.Fatal("the agent is still connected after its key was withdrawn")
	}

	if code, _ := call("DELETE", "/api/admin/clusters/mulan", alice, ""); code != http.StatusForbidden {
		t.Fatalf("a user deleting a cluster: %d", code)
	}
	if code, _ := call("DELETE", "/api/admin/clusters/mulan", admin, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call("DELETE", "/api/admin/clusters/mulan", admin, ""); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}
	if find(alice, "/api/clusters") != nil {
		t.Fatal("a deleted cluster is still listed")
	}
}

// Without a registry (a console of one cluster, as before) there is no
// way in for agents.
func TestClusters_OffWithoutARegistry(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	web := httptest.NewServer(s.Routes())
	defer web.Close()
	for _, path := range []string{"/agent/join", "/agent/connect"} {
		method := "GET"
		if path == "/agent/join" {
			method = "POST"
		}
		req, _ := http.NewRequest(method, web.URL+path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s without a registry: %d", path, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", web.URL+"/api/clusters", nil)
	req.AddCookie(forge(sm, "gl-alice", false))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if items, ok := out["items"].([]any); resp.StatusCode != http.StatusOK || !ok || len(items) != 0 {
		t.Fatalf("the list without a registry: %d %v", resp.StatusCode, out)
	}
}
