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
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestAccessTokens(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.Admins = map[string]bool{"tk-root": true}
	s.CLIKey = []byte("gateway-shared-key")
	ctx := context.Background()
	for _, name := range []string{"tk-alice", "tk-bob", "tk-root"} {
		u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8sClient.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, u) })
	}
	t.Cleanup(func() { cleanupDevPods(t) })
	alice, root := forge(sm, "tk-alice", false), forge(sm, "tk-root", true)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	// call sends a request with a cookie or a bearer token; writes reach
	// the cached client a moment later, so it retries until `want`.
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
			switch a := auth.(type) {
			case *http.Cookie:
				req.AddCookie(a)
			case string:
				req.Header.Set("Authorization", "Bearer "+a)
			case [2]string:
				req.Header.Set("Authorization", "Bearer "+a[0])
				req.Header.Set("X-DevPod-User", a[1])
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
	mint := func(c *http.Cookie, body string) string {
		t.Helper()
		return call("POST", "/api/me/tokens", c, body, http.StatusCreated)["token"].(string)
	}

	for _, n := range []string{"dev1", "dev2"} {
		call("POST", "/api/devpods", alice, strings.Replace(createBody, `"dev1"`, `"`+n+`"`, 1), http.StatusCreated)
	}
	waitDevPod(t, "tk-alice-dev1")
	waitDevPod(t, "tk-alice-dev2")

	t.Run("a token is shown once and only its hash is stored", func(t *testing.T) {
		tok := mint(alice, `{"name":"ci","scopes":["devpods:read"],"expiresInDays":30}`)
		if !strings.HasPrefix(tok, "dpt_") || strings.Count(tok, "_") != 2 {
			t.Fatalf("token %q", tok)
		}
		var u devpodv1alpha1.User
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: "tk-alice"}, &u); err != nil {
			t.Fatal(err)
		}
		secret := tok[strings.LastIndex(tok, "_")+1:]
		if len(u.Spec.APITokens) != 1 || len(u.Spec.APITokens[0].Hash) != 64 || strings.Contains(u.Spec.APITokens[0].Hash, secret) || u.Spec.APITokens[0].ExpiresAt == nil {
			t.Fatalf("stored %+v", u.Spec.APITokens)
		}
		list := call("GET", "/api/me/tokens", alice, "", http.StatusOK)
		if b, _ := json.Marshal(list); strings.Contains(string(b), secret) || strings.Contains(string(b), u.Spec.APITokens[0].Hash) {
			t.Fatalf("list leaks the secret or hash: %s", b)
		}
		// non-admins are not offered admin scopes
		if b, _ := json.Marshal(list["scopes"]); strings.Contains(string(b), "admin:") {
			t.Fatalf("admin scopes offered to a non-admin: %s", b)
		}
		call("POST", "/api/me/tokens", alice, `{"name":"x","scopes":["admin:read"]}`, http.StatusForbidden)
		call("POST", "/api/me/tokens", alice, `{"name":"x","scopes":["nope"]}`, http.StatusBadRequest)
		call("POST", "/api/me/tokens", alice, `{"name":"x","scopes":[]}`, http.StatusBadRequest)
	})

	t.Run("scopes gate each endpoint", func(t *testing.T) {
		read := mint(alice, `{"name":"read","scopes":["devpods:read"]}`)
		if items := call("GET", "/api/devpods", read, "", http.StatusOK)["items"].([]any); len(items) != 2 {
			t.Fatalf("list: %d items", len(items))
		}
		call("GET", "/api/devpods/tk-alice-dev1", read, "", http.StatusOK)
		call("GET", "/api/me", read, "", http.StatusOK)
		call("PATCH", "/api/devpods/tk-alice-dev1", read, `{"running":false}`, http.StatusForbidden)
		call("DELETE", "/api/devpods/tk-alice-dev1", read, "", http.StatusForbidden)
		call("GET", "/api/me/pubkeys", read, "", http.StatusForbidden)
		// a token never manages tokens or the password
		call("GET", "/api/me/tokens", read, "", http.StatusForbidden)
		call("POST", "/api/me/tokens", read, `{"name":"x","scopes":["devpods:read"]}`, http.StatusForbidden)
		call("PUT", "/api/me/password", read, `{}`, http.StatusForbidden)

		// start / stop and editing are separate scopes on the same PATCH
		power := mint(alice, `{"name":"power","scopes":["devpods:power"]}`)
		call("PATCH", "/api/devpods/tk-alice-dev1", power, `{"running":false}`, http.StatusOK)
		call("PATCH", "/api/devpods/tk-alice-dev1", power, `{"image":"ubuntu:25.04"}`, http.StatusForbidden)
		call("GET", "/api/devpods", power, "", http.StatusForbidden)
		update := mint(alice, `{"name":"update","scopes":["devpods:update"]}`)
		call("PATCH", "/api/devpods/tk-alice-dev1", update, `{"image":"ubuntu:25.04"}`, http.StatusOK)
		call("PATCH", "/api/devpods/tk-alice-dev1", update, `{"running":true}`, http.StatusForbidden)

		call("GET", "/api/devpods", "dpt_nosuchid_secret", "", http.StatusUnauthorized)
		last := "a"
		if strings.HasSuffix(read, "a") {
			last = "b"
		}
		call("GET", "/api/devpods", read[:len(read)-1]+last, "", http.StatusUnauthorized)
	})

	t.Run("a token limited to DevPods sees and touches only those", func(t *testing.T) {
		one := mint(alice, `{"name":"one","scopes":["devpods:read","devpods:power","devpods:delete"],"devpods":["tk-alice-dev2"]}`)
		items := call("GET", "/api/devpods", one, "", http.StatusOK)["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["metadata"].(map[string]any)["name"] != "tk-alice-dev2" {
			t.Fatalf("limited list: %v", items)
		}
		call("GET", "/api/devpods/tk-alice-dev2", one, "", http.StatusOK)
		call("GET", "/api/devpods/tk-alice-dev1", one, "", http.StatusForbidden)
		call("DELETE", "/api/devpods/tk-alice-dev1", one, "", http.StatusForbidden)
		call("GET", "/api/devpods?watch=true", one, "", http.StatusForbidden)
		call("POST", "/api/me/tokens", alice, `{"name":"x","scopes":["devpods:read"],"devpods":["a"],"expiresInDays":-1}`, http.StatusBadRequest)
	})

	t.Run("admin rights need the admin scopes", func(t *testing.T) {
		plain := mint(root, `{"name":"plain","scopes":["devpods:read","devpods:delete"]}`)
		// an admin's token without admin scopes is an ordinary user's
		call("GET", "/api/devpods/tk-alice-dev1", plain, "", http.StatusNotFound)
		call("GET", "/api/admin/users", plain, "", http.StatusForbidden)
		ro := mint(root, `{"name":"ro","scopes":["devpods:read","devpods:delete","admin:read"]}`)
		call("GET", "/api/devpods/tk-alice-dev1", ro, "", http.StatusOK)
		call("GET", "/api/admin/users", ro, "", http.StatusOK)
		// reading as admin does not allow changing someone else's DevPod
		call("DELETE", "/api/devpods/tk-alice-dev1", ro, "", http.StatusNotFound)
		call("POST", "/api/me/tokens", root, `{"name":"x","scopes":["admin:read"],"devpods":["a"]}`, http.StatusBadRequest)
	})

	t.Run("expired and revoked tokens stop working", func(t *testing.T) {
		tok := mint(alice, `{"name":"gone","scopes":["devpods:read"]}`)
		call("GET", "/api/devpods", tok, "", http.StatusOK)
		id := strings.Split(tok, "_")[1]
		var u devpodv1alpha1.User
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: "tk-alice"}, &u); err != nil {
			t.Fatal(err)
		}
		past := metav1.NewTime(time.Now().Add(-time.Minute))
		for i := range u.Spec.APITokens {
			if u.Spec.APITokens[i].ID == id {
				u.Spec.APITokens[i].ExpiresAt = &past
			}
		}
		if err := k8sClient.Update(ctx, &u); err != nil {
			t.Fatal(err)
		}
		call("GET", "/api/devpods", tok, "", http.StatusUnauthorized)
		call("DELETE", "/api/me/tokens/"+id, alice, "", http.StatusNoContent)
		call("DELETE", "/api/me/tokens/"+id, alice, "", http.StatusNotFound)
	})

	t.Run("the gateway acts for a user with the shared key", func(t *testing.T) {
		gw := [2]string{"gwk_gateway-shared-key", "tk-alice"}
		if items := call("GET", "/api/devpods", gw, "", http.StatusOK)["items"].([]any); len(items) != 2 {
			t.Fatalf("gateway list: %d", len(items))
		}
		if me := call("GET", "/api/me", [2]string{"gwk_gateway-shared-key", "tk-root"}, "", http.StatusOK); me["admin"] != true {
			t.Fatalf("admin through the gateway: %v", me)
		}
		call("GET", "/api/devpods", [2]string{"gwk_wrong", "tk-alice"}, "", http.StatusUnauthorized)
		// a person of the directory has no User object until they first sign in:
		// the gateway signed them in, so they are a user (and now have one)
		if me := call("GET", "/api/me", [2]string{"gwk_gateway-shared-key", "tk-nobody"}, "", http.StatusOK); me["user"] != "tk-nobody" || me["admin"] == true {
			t.Fatalf("a directory user through the gateway: %v", me)
		}
		var nobody devpodv1alpha1.User
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tk-nobody"}, &nobody); err != nil {
			t.Fatalf("no User object after their first use: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, &nobody) })
		call("GET", "/api/devpods", [2]string{"gwk_gateway-shared-key", ""}, "", http.StatusUnauthorized)
		s.CLIKey = nil
		call("GET", "/api/devpods", gw, "", http.StatusUnauthorized)
	})

	t.Run("every route is either scoped or closed to tokens", func(t *testing.T) {
		doc := call("GET", "/api/openapi.json", alice, "", http.StatusOK)
		paths := doc["paths"].(map[string]any)
		if op := paths["/api/devpods/{name}"].(map[string]any)["delete"].(map[string]any); op["x-scope"] != "devpods:delete" {
			t.Fatalf("openapi: %v", op)
		}
		if op := paths["/api/me/tokens"].(map[string]any)["post"].(map[string]any); op["x-scope"] != "browser session only" {
			t.Fatalf("openapi tokens: %v", op)
		}
	})
}
