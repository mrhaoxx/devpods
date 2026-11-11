// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// apiCall sends one request to a served API with a session cookie or a
// bearer token and insists on the status; writes reach the cached
// client a moment later, so it retries until `want`.
func apiCall(t *testing.T, srv *httptest.Server, method, path string, auth any, body string, want int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		switch a := auth.(type) {
		case *http.Cookie:
			req.AddCookie(a)
		case string:
			req.Header.Set("Authorization", "Bearer "+a)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == want {
			var out map[string]any
			_ = json.Unmarshal(b, &out)
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s: %d (want %d) %s", method, path, resp.StatusCode, want, b)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func cleanupConfigs(t *testing.T) {
	t.Helper()
	_ = k8sClient.DeleteAllOf(context.Background(), &devpodv1alpha1.DevPodConfig{}, client.InNamespace("devpods"))
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func configNames(list map[string]any) []string {
	var out []string
	for _, it := range list["items"].([]any) {
		out = append(out, it.(map[string]any)["name"].(string))
	}
	return out
}

const configYAML = "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: ${NAME}\nspec:\n  owner: ${OWNER}\n  running: true\n  pod:\n    spec:\n      nodeSelector:\n        kubernetes.io/hostname: ${NODE}\n      containers:\n        - name: dev\n          image: ${IMAGE:-ubuntu:24.04}\n          resources:\n            limits: {cpu: '1', memory: 1Gi}\n"

func TestConfigAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	t.Cleanup(func() { cleanupConfigs(t) })
	alice, bob, carol, root := forge(sm, "cf-alice", false), forge(sm, "cf-bob", false), forge(sm, "cf-carol", false), forge(sm, "cf-root", true)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		return apiCall(t, srv, method, path, auth, body, want)
	}
	create := map[string]any{"id": "sg", "title": "SGLang", "description": "eight cards", "content": configYAML}

	v := call("POST", "/api/configs", alice, jsonBody(create), http.StatusCreated)
	if v["name"] != "cf-alice-sg" || v["owner"] != "cf-alice" || v["mine"] != true || v["content"] != configYAML || v["updatedAt"] == "" {
		t.Fatalf("created %+v", v)
	}
	vars := v["variables"].([]any)
	var names []string
	for _, x := range vars {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "NAME,OWNER,NODE,IMAGE" || vars[3].(map[string]any)["default"] != "ubuntu:24.04" {
		t.Fatalf("variables %+v", vars)
	}
	call("POST", "/api/configs", alice, jsonBody(create), http.StatusConflict)

	t.Run("what a body may not be", func(t *testing.T) {
		bad := func(change func(m map[string]any)) {
			t.Helper()
			m := map[string]any{"id": "x", "title": "t", "content": "a: b"}
			change(m)
			call("POST", "/api/configs", alice, jsonBody(m), http.StatusBadRequest)
		}
		bad(func(m map[string]any) { m["id"] = "Bad_Id" })
		bad(func(m map[string]any) { m["id"] = strings.Repeat("a", 60) }) // cf-alice-<60> is past 63
		bad(func(m map[string]any) { m["title"] = "" })
		bad(func(m map[string]any) { m["title"] = strings.Repeat("t", 81) })
		bad(func(m map[string]any) { m["description"] = strings.Repeat("d", 501) })
		bad(func(m map[string]any) { m["content"] = "" })
		bad(func(m map[string]any) { m["content"] = strings.Repeat("a", 65537) })
		bad(func(m map[string]any) { m["sharedWith"] = []string{"Not A User"} })
		bad(func(m map[string]any) { m["unknown"] = 1 })
		many := make([]string, 65)
		for i := range many {
			many[i] = "u" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		}
		bad(func(m map[string]any) { m["sharedWith"] = many })
		// content is text: not parsed when saved
		call("POST", "/api/configs", alice, jsonBody(map[string]any{"id": "odd", "title": "t", "content": "gpu: ${GPU}\n  - not yaml"}), http.StatusCreated)
	})

	t.Run("a config nobody shared is not there for others", func(t *testing.T) {
		call("GET", "/api/configs/cf-alice-sg", bob, "", http.StatusNotFound)
		call("PATCH", "/api/configs/cf-alice-sg", bob, `{"title":"x"}`, http.StatusNotFound)
		call("DELETE", "/api/configs/cf-alice-sg", bob, "", http.StatusNotFound)
		call("GET", "/api/configs/cf-alice-nope", alice, "", http.StatusNotFound)
		if got := configNames(call("GET", "/api/configs", bob, "", http.StatusOK)); len(got) != 0 {
			t.Fatalf("bob sees %v", got)
		}
		call("GET", "/api/configs?scope=bogus", bob, "", http.StatusBadRequest)
	})

	t.Run("shared with someone: they read and use, they do not change", func(t *testing.T) {
		// the owner's own name and repeats are dropped
		v := call("PATCH", "/api/configs/cf-alice-sg", alice, `{"sharedWith":["cf-bob","cf-alice","cf-bob"]}`, http.StatusOK)
		if sw := v["sharedWith"].([]any); len(sw) != 1 || sw[0] != "cf-bob" {
			t.Fatalf("sharedWith %v", sw)
		}
		got := call("GET", "/api/configs/cf-alice-sg", bob, "", http.StatusOK)
		if got["mine"] != false || got["content"] != configYAML {
			t.Fatalf("bob's view %+v", got)
		}
		call("PATCH", "/api/configs/cf-alice-sg", bob, `{"title":"x"}`, http.StatusForbidden)
		call("PATCH", "/api/configs/cf-alice-sg", bob, `{"public":true}`, http.StatusForbidden)
		call("DELETE", "/api/configs/cf-alice-sg", bob, "", http.StatusForbidden)
		call("GET", "/api/configs/cf-alice-sg", carol, "", http.StatusNotFound)

		list := call("GET", "/api/configs?scope=shared", bob, "", http.StatusOK)
		if got := configNames(list); len(got) != 1 || got[0] != "cf-alice-sg" {
			t.Fatalf("shared list %v", got)
		}
		if _, has := list["items"].([]any)[0].(map[string]any)["content"]; has {
			t.Fatal("a list carries the content")
		}
		if got := configNames(call("GET", "/api/configs?scope=mine", bob, "", http.StatusOK)); len(got) != 0 {
			t.Fatalf("bob's own %v", got)
		}
		if got := configNames(call("GET", "/api/configs?scope=shared", alice, "", http.StatusOK)); len(got) != 0 {
			t.Fatalf("alice's shared list has her own: %v", got)
		}
	})

	t.Run("public: everyone signed in", func(t *testing.T) {
		call("PATCH", "/api/configs/cf-alice-sg", alice, `{"public":true,"title":"SGLang 8"}`, http.StatusOK)
		if got := call("GET", "/api/configs/cf-alice-sg", carol, "", http.StatusOK); got["title"] != "SGLang 8" || got["public"] != true {
			t.Fatalf("carol's view %+v", got)
		}
		if got := configNames(call("GET", "/api/configs?scope=public", carol, "", http.StatusOK)); len(got) != 1 {
			t.Fatalf("public list %v", got)
		}
		// all scopes together list a config once
		if got := configNames(call("GET", "/api/configs", bob, "", http.StatusOK)); len(got) != 1 {
			t.Fatalf("bob's full list %v", got)
		}
		call("PATCH", "/api/configs/cf-alice-sg", carol, `{"public":false}`, http.StatusForbidden)
	})

	t.Run("an admin may take a config down, not rewrite it", func(t *testing.T) {
		call("PATCH", "/api/configs/cf-alice-sg", root, `{"title":"mine now"}`, http.StatusForbidden)
		call("PATCH", "/api/configs/cf-alice-sg", root, `{"public":true}`, http.StatusForbidden)
		call("PATCH", "/api/configs/cf-alice-sg", root, `{"public":false,"title":"x"}`, http.StatusForbidden)
		if v := call("PATCH", "/api/configs/cf-alice-sg", root, `{"public":false}`, http.StatusOK); v["public"] != false || v["title"] != "SGLang 8" {
			t.Fatalf("after unpublish %+v", v)
		}
		call("GET", "/api/configs/cf-alice-sg", carol, "", http.StatusNotFound)
		// an admin sees every config, in the full list too
		call("GET", "/api/configs/cf-alice-odd", root, "", http.StatusOK)
		call("DELETE", "/api/configs/cf-alice-odd", root, "", http.StatusNoContent)
		call("GET", "/api/configs/cf-alice-odd", alice, "", http.StatusNotFound)
	})

	t.Run("the owner edits and deletes", func(t *testing.T) {
		v := call("PATCH", "/api/configs/cf-alice-sg", alice, jsonBody(map[string]any{"content": "name: ${NAME}\n", "description": ""}), http.StatusOK)
		if v["content"] != "name: ${NAME}\n" || v["description"] != "" || len(v["variables"].([]any)) != 1 {
			t.Fatalf("edited %+v", v)
		}
		call("PATCH", "/api/configs/cf-alice-sg", alice, `{"id":"other"}`, http.StatusBadRequest)
		call("PATCH", "/api/configs/cf-alice-sg", alice, `{"content":""}`, http.StatusBadRequest)
		call("DELETE", "/api/configs/cf-alice-sg", alice, "", http.StatusNoContent)
		call("GET", "/api/configs/cf-alice-sg", alice, "", http.StatusNotFound)
	})
}

func TestConfigTokens(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "ct-alice"}}
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, u); cleanupConfigs(t) })
	alice := forge(sm, "ct-alice", false)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		return apiCall(t, srv, method, path, auth, body, want)
	}
	mint := func(scopes ...string) string {
		t.Helper()
		return call("POST", "/api/me/tokens", alice, jsonBody(map[string]any{"name": strings.Join(scopes, "+"), "scopes": scopes}), http.StatusCreated)["token"].(string)
	}
	read, write, pods := mint("configs:read"), mint("configs:write"), mint("devpods:read")
	body := jsonBody(map[string]any{"id": "a", "title": "t", "content": "a: b"})

	call("POST", "/api/configs", read, body, http.StatusForbidden)
	call("POST", "/api/configs", pods, body, http.StatusForbidden)
	call("POST", "/api/configs", write, body, http.StatusCreated)
	call("GET", "/api/configs/ct-alice-a", read, "", http.StatusOK)
	call("GET", "/api/configs", read, "", http.StatusOK)
	call("GET", "/api/configs/ct-alice-a", write, "", http.StatusForbidden)
	call("GET", "/api/configs", pods, "", http.StatusForbidden)
	call("PATCH", "/api/configs/ct-alice-a", read, `{"title":"x"}`, http.StatusForbidden)
	call("DELETE", "/api/configs/ct-alice-a", read, "", http.StatusForbidden)
	call("DELETE", "/api/configs/ct-alice-a", write, "", http.StatusNoContent)

	doc := call("GET", "/api/openapi.json", alice, "", http.StatusOK)
	if op := doc["paths"].(map[string]any)["/api/configs/{name}"].(map[string]any)["patch"].(map[string]any); op["x-scope"] != "configs:write" {
		t.Fatalf("openapi: %v", op)
	}
}

// A deleted user leaves nothing behind for a later user of the same
// name: their configs go, and so does their place in others' shares.
func TestConfigsFollowUserDeletion(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	gone := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "cd-gone"}}
	if err := k8sClient.Create(ctx, gone); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, gone); cleanupConfigs(t) })
	goneC, stay, root := forge(sm, "cd-gone", false), forge(sm, "cd-stay", false), forge(sm, "cd-root", true)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		return apiCall(t, srv, method, path, auth, body, want)
	}
	call("POST", "/api/configs", goneC, jsonBody(map[string]any{"id": "own", "title": "t", "content": "a: b", "public": true}), http.StatusCreated)
	call("POST", "/api/configs", stay, jsonBody(map[string]any{"id": "kept", "title": "t", "content": "a: b", "sharedWith": []string{"cd-gone", "cd-other"}}), http.StatusCreated)
	call("GET", "/api/configs/cd-stay-kept", goneC, "", http.StatusOK)

	call("DELETE", "/api/admin/users/cd-gone", root, "", http.StatusNoContent)

	call("GET", "/api/configs/cd-gone-own", stay, "", http.StatusNotFound)
	// a new session under the old name inherits nothing
	call("GET", "/api/configs/cd-stay-kept", forge(sm, "cd-gone", false), "", http.StatusNotFound)
	v := call("GET", "/api/configs/cd-stay-kept", stay, "", http.StatusOK)
	if sw := v["sharedWith"].([]any); len(sw) != 1 || sw[0] != "cd-other" {
		t.Fatalf("sharedWith after the deletion: %v", sw)
	}
	_ = webui.SessionCookie
}
