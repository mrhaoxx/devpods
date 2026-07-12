// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// waitLogin polls the password-login handler (which reads through the
// informer cache) until it returns want, so tests don't race the cache.
func waitLogin(t *testing.T, s *webui.Server, user, pass string, want int) {
	t.Helper()
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	for i := 0; i < 100; i++ {
		rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil, body)
		if rec.Code == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("login for %q never reached status %d", user, want)
}

func TestPasswordAuth(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.PasswordAuth = true
	s.PasswordMinLength = 8
	s.Admins = map[string]bool{}
	ctx := context.Background()

	// Seed a password user (as admin create would).
	hash, err := webui.HashPassword("hunter2secret", 8)
	if err != nil {
		t.Fatal(err)
	}
	pw := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "pwuser"},
		Spec:       devpodv1alpha1.UserSpec{PasswordHash: hash},
	}
	if err := k8sClient.Create(ctx, pw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pw) })

	t.Run("config reflects flags", func(t *testing.T) {
		rec := doJSON(t, s.HandleAuthConfigForTest(), "GET", "/api/auth/config", nil, nil, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"password":true`) {
			t.Fatalf("config = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("login success sets session", func(t *testing.T) {
		rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil,
			`{"username":"pwuser","password":"hunter2secret"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("login = %d %s", rec.Code, rec.Body)
		}
		var c *http.Cookie
		for _, ck := range rec.Result().Cookies() {
			if ck.Name == webui.SessionCookie {
				c = ck
			}
		}
		if c == nil {
			t.Fatal("no session cookie")
		}
	})

	t.Run("wrong password 401", func(t *testing.T) {
		rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil,
			`{"username":"pwuser","password":"wrongwrong"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("unknown user 401", func(t *testing.T) {
		rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil,
			`{"username":"ghost","password":"whatever!"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("disabled 404", func(t *testing.T) {
		s.PasswordAuth = false
		defer func() { s.PasswordAuth = true }()
		rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil,
			`{"username":"pwuser","password":"hunter2secret"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("self change password", func(t *testing.T) {
		cookie := forge(sm, "pwuser", false)
		rec := doJSON(t, s.HandleChangePasswordForTest(), "PUT", "/api/me/password", nil, cookie,
			`{"oldPassword":"hunter2secret","newPassword":"newpassword9"}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("change = %d %s", rec.Code, rec.Body)
		}
		// Login reads through the informer cache; wait for it to reflect
		// the update before asserting the new password works.
		waitLogin(t, s, "pwuser", "newpassword9", http.StatusOK)
		if rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil,
			`{"username":"pwuser","password":"hunter2secret"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("old password should fail: %d", rec.Code)
		}
	})

	t.Run("change wrong old password 401", func(t *testing.T) {
		cookie := forge(sm, "pwuser", false)
		rec := doJSON(t, s.HandleChangePasswordForTest(), "PUT", "/api/me/password", nil, cookie,
			`{"oldPassword":"nope","newPassword":"whatever12"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestAdminUserCRUD(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.PasswordAuth = true
	s.PasswordMinLength = 8
	s.Admins = map[string]bool{"gl-root": true}
	admin := forge(sm, "gl-root", true)
	alice := forge(sm, "gl-alice", false)

	t.Run("non-admin 403", func(t *testing.T) {
		rec := doJSON(t, s.HandleListUsersForTest(), "GET", "/api/admin/users", nil, alice, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("create then login works", func(t *testing.T) {
		rec := doJSON(t, s.HandleCreateUserForTest(), "POST", "/api/admin/users", nil, admin,
			`{"username":"bob","displayName":"Bob","password":"bobsecret1"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "bob"}})
		})
		// The created user has NO admin (kubectl-only).
		var u devpodv1alpha1.User
		if err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: "bob"}, &u); err != nil {
			t.Fatal(err)
		}
		if u.Spec.Admin || u.Spec.PasswordHash == "" {
			t.Fatalf("unexpected spec: admin=%v hasHash=%v", u.Spec.Admin, u.Spec.PasswordHash != "")
		}
		waitLogin(t, s, "bob", "bobsecret1", http.StatusOK)
	})

	t.Run("create rejects short password", func(t *testing.T) {
		rec := doJSON(t, s.HandleCreateUserForTest(), "POST", "/api/admin/users", nil, admin,
			`{"username":"carol","password":"short"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("create rejects admin field", func(t *testing.T) {
		// admin is not an accepted field (DisallowUnknownFields).
		rec := doJSON(t, s.HandleCreateUserForTest(), "POST", "/api/admin/users", nil, admin,
			`{"username":"dave","password":"davesecret","admin":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("reset password via patch", func(t *testing.T) {
		if rec := doJSON(t, s.HandleCreateUserForTest(), "POST", "/api/admin/users", nil, admin,
			`{"username":"erin","password":"erinsecret1"}`); rec.Code != http.StatusCreated {
			t.Fatalf("create erin: %d", rec.Code)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "erin"}})
		})
		rec := doJSON(t, s.HandlePatchUserForTest(), "PATCH", "/api/admin/users/erin",
			map[string]string{"name": "erin"}, admin, `{"password":"resetsecret9"}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("patch = %d %s", rec.Code, rec.Body)
		}
		waitLogin(t, s, "erin", "resetsecret9", http.StatusOK)
	})
}
