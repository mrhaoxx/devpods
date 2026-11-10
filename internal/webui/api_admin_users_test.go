// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func mkUser(t *testing.T, name string, mut func(*devpodv1alpha1.User)) {
	t.Helper()
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if mut != nil {
		mut(u)
	}
	if err := k8sClient.Create(context.Background(), u); err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), u) })
}

func getUser(t *testing.T, name string) devpodv1alpha1.User {
	t.Helper()
	var u devpodv1alpha1.User
	// the API reader: the cached client can trail a write just made
	if err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: name}, &u); err != nil {
		t.Fatalf("get user %s: %v", name, err)
	}
	return u
}

func TestAdminUserListFields(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	root := forge(sm, "lf-root", true)
	maxSize := resource.MustParse("10Mi")
	mkUser(t, "lf-oidc", func(u *devpodv1alpha1.User) { u.Spec.OIDCSubject = "sub-1" })
	mkUser(t, "lf-pw", func(u *devpodv1alpha1.User) {
		u.Spec.PasswordHash = "$2a$10$abcdefghijklmnopqrstuuJ0123456789012345678901234567890"
		u.Spec.AuditPolicy = &devpodv1alpha1.AuditPolicy{MaxFileSize: &maxSize}
	})
	mkUser(t, "lf-ldap", func(u *devpodv1alpha1.User) { u.Spec.Pubkeys = []string{testPubkey(t), testPubkey(t)} })

	rec := doJSON(t, s.HandleListUsersForTest(), "GET", "/api/admin/users", nil, root, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []struct {
			Name         string `json:"name"`
			Source       string `json:"source"`
			CustomPolicy bool   `json:"customPolicy"`
			Pubkeys      int    `json:"pubkeys"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range body.Items {
		got[it.Name] = fmt.Sprintf("%s/%v/%d", it.Source, it.CustomPolicy, it.Pubkeys)
	}
	for name, want := range map[string]string{"lf-oidc": "oidc/false/0", "lf-pw": "password/true/0", "lf-ldap": "ldap/false/2"} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

func TestPatchUserAdmin(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	root := forge(sm, "pa-root", true)
	mkUser(t, "pa-root", func(u *devpodv1alpha1.User) { u.Spec.Admin = true })
	mkUser(t, "pa-bob", nil)
	patch := func(name, body string, want int) {
		t.Helper()
		rec := doJSON(t, s.HandlePatchUserForTest(), "PATCH", "/api/admin/users/"+name, map[string]string{"name": name}, root, body)
		if rec.Code != want {
			t.Fatalf("PATCH %s %s: %d (want %d) %s", name, body, rec.Code, want, rec.Body)
		}
	}
	patch("pa-bob", `{"admin":true}`, http.StatusNoContent)
	if !getUser(t, "pa-bob").Spec.Admin {
		t.Fatal("grant did not stick")
	}
	patch("pa-bob", `{"admin":false}`, http.StatusNoContent)
	if getUser(t, "pa-bob").Spec.Admin {
		t.Fatal("revoke did not stick")
	}
	patch("pa-root", `{"admin":false}`, http.StatusBadRequest) // no self-revoke
	if !getUser(t, "pa-root").Spec.Admin {
		t.Fatal("self-revoke went through")
	}
}

type batchResult struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	Password   string `json:"password"`
	Hibernated int    `json:"hibernated"`
}

func TestUsersBatch(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.PasswordAuth = true
	root := forge(sm, "bt-root", true)
	mkUser(t, "bt-a", nil)
	mkUser(t, "bt-b", nil)
	batch := func(t *testing.T, c *http.Cookie, body string, want int) []batchResult {
		t.Helper()
		rec := doJSON(t, s.HandleUsersBatchForTest(), "POST", "/api/admin/users:batch", nil, c, body)
		if rec.Code != want {
			t.Fatalf("batch %s: %d (want %d) %s", body, rec.Code, want, rec.Body)
		}
		var out struct {
			Results []batchResult `json:"results"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Results
	}
	allOK := func(t *testing.T, rs []batchResult, n int) {
		t.Helper()
		if len(rs) != n {
			t.Fatalf("results = %+v", rs)
		}
		for _, r := range rs {
			if !r.OK {
				t.Fatalf("result %+v", r)
			}
		}
	}

	t.Run("setQuota", func(t *testing.T) {
		allOK(t, batch(t, root, `{"names":["bt-a","bt-b"],"op":"setQuota","args":{"quota":{"cpu":"4"}}}`, http.StatusOK), 2)
		for _, n := range []string{"bt-a", "bt-b"} {
			q := getUser(t, n).Spec.Quota
			if q == nil || q.Compute.Cpu().String() != "4" {
				t.Fatalf("%s quota = %+v", n, q)
			}
		}
	})
	t.Run("setPolicy sets and null clears", func(t *testing.T) {
		allOK(t, batch(t, root, `{"names":["bt-a"],"op":"setPolicy","args":{"policy":{"maxFileSize":"5Mi"}}}`, http.StatusOK), 1)
		if p := getUser(t, "bt-a").Spec.AuditPolicy; p == nil || p.MaxFileSize == nil || p.MaxFileSize.String() != "5Mi" {
			t.Fatalf("policy = %+v", p)
		}
		allOK(t, batch(t, root, `{"names":["bt-a"],"op":"setPolicy","args":{"policy":null}}`, http.StatusOK), 1)
		if p := getUser(t, "bt-a").Spec.AuditPolicy; p != nil {
			t.Fatalf("policy not cleared: %+v", p)
		}
		batch(t, root, `{"names":["bt-a"],"op":"setPolicy","args":{}}`, http.StatusBadRequest) // policy key required
	})
	t.Run("resetPassword returns a 16-char password that logs in", func(t *testing.T) {
		rs := batch(t, root, `{"names":["bt-b"],"op":"resetPassword"}`, http.StatusOK)
		allOK(t, rs, 1)
		if len(rs[0].Password) != 16 {
			t.Fatalf("password %q", rs[0].Password)
		}
		rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil,
			fmt.Sprintf(`{"username":"bt-b","password":%q}`, rs[0].Password))
		if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
			t.Fatalf("login with the new password: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("hibernateAll stops running DevPods and reports the count", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		a := forge(sm, "bt-a", false)
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, a, createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
		waitDevPod(t, "bt-a-dev1")
		rs := batch(t, root, `{"names":["bt-a"],"op":"hibernateAll"}`, http.StatusOK)
		allOK(t, rs, 1)
		if rs[0].Hibernated != 1 {
			t.Fatalf("hibernated = %d", rs[0].Hibernated)
		}
		waitRunning(t, "bt-a-dev1", false)
	})
	t.Run("partial failure", func(t *testing.T) {
		rs := batch(t, root, `{"names":["bt-a","bt-ghost"],"op":"setQuota","args":{"quota":{"cpu":"2"}}}`, http.StatusOK)
		if len(rs) != 2 || !rs[0].OK || rs[1].OK || !strings.Contains(rs[1].Error, "not found") {
			t.Fatalf("results = %+v", rs)
		}
	})
	t.Run("non-admin 403", func(t *testing.T) {
		batch(t, forge(sm, "bt-a", false), `{"names":["bt-b"],"op":"resetPassword"}`, http.StatusForbidden)
	})
	t.Run("bad requests", func(t *testing.T) {
		names := make([]string, 501)
		for i := range names {
			names[i] = fmt.Sprintf("%q", fmt.Sprintf("u%d", i))
		}
		batch(t, root, `{"names":[`+strings.Join(names, ",")+`],"op":"setQuota","args":{"quota":{}}}`, http.StatusBadRequest)
		batch(t, root, `{"names":[],"op":"setQuota","args":{"quota":{}}}`, http.StatusBadRequest)
		batch(t, root, `{"names":["bt-a"],"op":"explode"}`, http.StatusBadRequest)
		s.PasswordAuth = false
		defer func() { s.PasswordAuth = true }()
		batch(t, root, `{"names":["bt-a"],"op":"resetPassword"}`, http.StatusBadRequest)
	})
}
