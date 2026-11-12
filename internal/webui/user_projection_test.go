// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func userIn(t *testing.T, c client.Client, name string) *devpodv1alpha1.User {
	t.Helper()
	var u devpodv1alpha1.User
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &u); err != nil {
		return nil
	}
	return &u
}

func hosts(u *devpodv1alpha1.User) string {
	var out []string
	for _, r := range u.Spec.Registries {
		out = append(out, r.Host)
	}
	return strings.Join(out, ",")
}

// Users are the console's. A cluster is given only what its controller
// needs of one: the registries and the credential on its way to the
// pull secret.
func TestUsersAcrossClusters(t *testing.T) {
	w := setupWorld(t)
	ctx := context.Background()
	// who alice is, at the console
	if err := w.a.Create(ctx, &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "gl-alice"},
		Spec: devpodv1alpha1.UserSpec{
			Pubkeys:     []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFzmR0Dk5Lk5gGk4Sx0dH5ZyqVQk1o3o8o4S0r9o6m0f alice"},
			DisplayName: "Alice", PasswordHash: "$2a$10$abcdefghijklmnopqrstuuK3r0QzQ3s9H3mE8E1dZ0c2bq7g8k9lK", Admin: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.a.Delete(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-alice"}}) })
	w.until("the console sees her", func() bool {
		var u devpodv1alpha1.User
		return k8sClient.Get(ctx, types.NamespacedName{Name: "gl-alice"}, &u) == nil
	})

	// a registry credential goes to every cluster
	code, out, _ := w.call("gl-alice", "PUT", "/api/me/registries/reg.example.com", `{"username":"alice","password":"s3cret-one"}`)
	if code != http.StatusOK {
		t.Fatalf("save a credential: %d %v", code, out)
	}
	if cl, _ := out["clusters"].(map[string]any); cl["a"] != "ok" || cl["b"] != "ok" {
		t.Fatalf("where it went: %v", out["clusters"])
	}
	ub := userIn(t, w.b, "gl-alice")
	if ub == nil || hosts(ub) != "reg.example.com" || len(ub.Spec.RegistryAuth) != 1 || ub.Spec.RegistryAuth[0].Password != "s3cret-one" {
		t.Fatalf("cluster b's copy: %+v", ub)
	}
	// and nothing else of her
	if len(ub.Spec.Pubkeys) != 0 || ub.Spec.PasswordHash != "" || ub.Spec.Admin || ub.Spec.DisplayName != "" || len(ub.Spec.APITokens) != 0 || ub.Spec.Quota != nil {
		t.Fatalf("cluster b was given more than the registries: %+v", ub.Spec)
	}
	ua := userIn(t, w.a, "gl-alice")
	if hosts(ua) != "reg.example.com" || len(ua.Spec.Pubkeys) != 1 || !ua.Spec.Admin || ua.Spec.PasswordHash == "" {
		t.Fatalf("the console's own record was damaged: %+v", ua.Spec)
	}

	// a cluster that is away is said so, and the rest still get it
	w.stopB()
	w.until("b is away", func() bool { _, stale := w.s.Clusters.Stale("b"); return stale })
	code, out, _ = w.call("gl-alice", "PUT", "/api/me/registries/other.example.com", `{"username":"alice","password":"s3cret-two"}`)
	if cl, _ := out["clusters"].(map[string]any); code != http.StatusOK || cl["a"] != "ok" || cl["b"] != "offline" {
		t.Fatalf("saving while b is away: %d %v", code, out)
	}
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "s3cret") {
		t.Fatal("the answer carries a password")
	}
	if got := hosts(userIn(t, w.a, "gl-alice")); got != "reg.example.com,other.example.com" && got != "other.example.com,reg.example.com" {
		t.Fatalf("the console's record: %s", got)
	}
	if got := hosts(userIn(t, w.b, "gl-alice")); got != "reg.example.com" {
		t.Fatalf("b changed while away: %s", got)
	}
	w.startB()
	w.until("b is back", func() bool {
		_, stale := w.s.Clusters.Stale("b")
		ok, _ := w.s.Clusters.Connected("b")
		return ok && !stale
	})

	// removing one removes it everywhere it is
	code, out, _ = w.call("gl-alice", "DELETE", "/api/me/registries/reg.example.com", "")
	if cl, _ := out["clusters"].(map[string]any); code != http.StatusOK || cl["a"] != "ok" || cl["b"] != "ok" {
		t.Fatalf("remove a credential: %d %v", code, out)
	}
	if got := hosts(userIn(t, w.b, "gl-alice")); got != "other.example.com" {
		t.Fatalf("b after the removal (it now lists what the console lists): %s", got)
	}
	if n := len(userIn(t, w.b, "gl-alice").Spec.RegistryAuth); n != 0 {
		t.Fatalf("b holds %d credentials on their way; the removed one should be gone and the other never reached it", n)
	}
	// saving the one b missed, now that it is back
	code, out, _ = w.call("gl-alice", "PUT", "/api/me/registries/other.example.com", `{"username":"alice","password":"s3cret-two"}`)
	if cl, _ := out["clusters"].(map[string]any); code != http.StatusOK || cl["b"] != "ok" {
		t.Fatalf("saving again: %d %v", code, out)
	}
	if ub := userIn(t, w.b, "gl-alice"); len(ub.Spec.RegistryAuth) != 1 || ub.Spec.RegistryAuth[0].Host != "other.example.com" {
		t.Fatalf("b after saving again: %+v", ub.Spec.RegistryAuth)
	}

	// quotas: one per cluster, set by an admin
	if code, out, _ := w.call("root", "PATCH", "/api/admin/users/gl-alice", `{"quotas":{"b":{"maxDevPods":1},"nope":{"maxDevPods":1}}}`); code != http.StatusBadRequest {
		t.Fatalf("a quota for a cluster that does not exist: %d %v", code, out)
	}
	if code, out, _ := w.call("root", "PATCH", "/api/admin/users/gl-alice", `{"quotas":{"b":{"maxDevPods":1}}}`); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("set a quota for b: %d %v", code, out)
	}
	w.until("the console sees the quota", func() bool {
		var u devpodv1alpha1.User
		return k8sClient.Get(ctx, types.NamespacedName{Name: "gl-alice"}, &u) == nil && u.Spec.Quotas["b"].MaxDevPods != nil
	})
	// alice is an admin by her record; bob-like checks need a plain session
	plain := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(forge(w.sm, "gl-alice", false))
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	mk := func(name string) string { return `{"name":"` + name + `","image":"ubuntu:24.04"}` }
	if code, out := plain("POST", "/api/clusters/b/devpods", mk("q1")); code != http.StatusCreated {
		t.Fatalf("first in b: %d %v", code, out)
	}
	w.until("b's cache has it", func() bool {
		_, out := plain("GET", "/api/clusters/b/devpods", "")
		return len(items(out)) == 1
	})
	if code, out := plain("POST", "/api/clusters/b/devpods", mk("q2")); code != http.StatusConflict || out["code"] != "QUOTA_EXCEEDED" {
		t.Fatalf("second in b, which allows her one: %d %v", code, out)
	}
	for _, n := range []string{"q1", "q2"} {
		if code, out := plain("POST", "/api/clusters/a/devpods", mk(n)); code != http.StatusCreated {
			t.Fatalf("%s in a, where the console's default holds: %d %v", n, code, out)
		}
	}
	// what she is told about herself: usage and quota per cluster
	_, me := plain("GET", "/api/me", "")
	per, _ := me["clusters"].(map[string]any)
	pb, _ := per["b"].(map[string]any)
	pa, _ := per["a"].(map[string]any)
	if pb == nil || pa == nil || pb["usage"].(map[string]any)["devpods"] != float64(1) || pa["usage"].(map[string]any)["devpods"] != float64(2) ||
		pb["quota"].(map[string]any)["maxDevPods"] != float64(1) || pa["quota"].(map[string]any)["maxDevPods"] != float64(5) {
		t.Fatalf("/api/me per cluster: %v", me["clusters"])
	}

	// a token for one cluster
	code, out, _ = w.call("gl-alice", "POST", "/api/me/tokens", `{"name":"only-a","scopes":["devpods:read","devpods:create","registries:write"],"clusters":["a"]}`)
	token, _ := out["token"].(string)
	if code != http.StatusCreated || token == "" {
		t.Fatalf("create a token: %d %v", code, out)
	}
	if code, out, _ := w.call("gl-alice", "POST", "/api/me/tokens", `{"name":"bad","scopes":["devpods:read"],"clusters":["nope"]}`); code != http.StatusBadRequest {
		t.Fatalf("a token for a cluster that does not exist: %d %v", code, out)
	}
	bearer := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	w.until("the token is known", func() bool { code, _ := bearer("GET", "/api/clusters/a/devpods", ""); return code == http.StatusOK })
	if code, out := bearer("GET", "/api/clusters/b/devpods", ""); code != http.StatusNotFound || out["code"] != "UNKNOWN_CLUSTER" {
		t.Fatalf("the token on b: %d %v", code, out)
	}
	if code, out := bearer("POST", "/api/devpods?cluster=b", mk("t1")); code != http.StatusNotFound {
		t.Fatalf("the token creating in b: %d %v", code, out)
	}
	_, out = bearer("GET", "/api/devpods", "")
	for _, it := range items(out) {
		if !strings.HasPrefix(it, "a/") {
			t.Fatalf("the token's list shows %s", it)
		}
	}
	if len(items(out)) != 2 {
		t.Fatalf("the token's list: %v", items(out))
	}
	// a credential saved with it goes where the token reaches
	code, out = bearer("PUT", "/api/me/registries/third.example.com", `{"username":"alice","password":"s3cret-three"}`)
	if cl, _ := out["clusters"].(map[string]any); code != http.StatusOK || cl["a"] != "ok" || len(cl) != 1 {
		t.Fatalf("a credential saved with a token for cluster a: %d %v", code, out)
	}
	if got := hosts(userIn(t, w.b, "gl-alice")); strings.Contains(got, "third.example.com") {
		t.Fatalf("the token for a wrote to b: %s", got)
	}
	// removing what is already gone at the console still tells the clusters
	if code, out, _ := w.call("gl-alice", "DELETE", "/api/me/registries/reg.example.com", ""); code != http.StatusOK || out["clusters"] == nil {
		t.Fatalf("a removal said again: %d %v", code, out)
	}

	// b's DevPod by name alone: not there for this token
	if code, _ := bearer("GET", "/api/devpods/gl-alice-q1", ""); code != http.StatusOK {
		t.Fatalf("a's q1 by name (b's is out of the token's reach, so the name is not ambiguous): %d", code)
	}
}
