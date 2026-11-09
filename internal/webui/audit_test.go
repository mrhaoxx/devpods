// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestFreshSession(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	_ = k8sClient.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "ex-admin"}, Spec: devpodv1alpha1.UserSpec{Admin: false}})
	_ = k8sClient.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "crd-admin"}, Spec: devpodv1alpha1.UserSpec{Admin: true}})
	waitUser(t, "ex-admin")
	waitUser(t, "crd-admin")
	s.Admins = map[string]bool{"listed": true}

	check := func(user string, cookieAdmin, want bool) {
		t.Helper()
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(forge(sm, user, cookieAdmin))
		sess, ok := s.FreshSessionForTest(httptest.NewRecorder(), req)
		if !ok || sess.User != user || sess.Admin != want {
			t.Errorf("%s (cookie admin=%v): ok=%v admin=%v, want %v", user, cookieAdmin, ok, sess.Admin, want)
		}
	}
	t.Run("RevokedAdmin", func(t *testing.T) { check("ex-admin", true, false) })
	t.Run("AllowlistAdmin", func(t *testing.T) { check("listed", false, true) })
	t.Run("CRDAdmin", func(t *testing.T) { check("crd-admin", false, true) })
	t.Run("NoUserCR", func(t *testing.T) { check("ghost", true, false) })
}

func TestMe_FeatureAudit(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	feature := func() bool {
		rec := doJSON(t, s.HandleMeForTest(), "GET", "/api/me", nil, forge(sm, "gl-bob", false), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var body struct {
			Features map[string]bool `json:"features"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Features["audit"]
	}
	if feature() {
		t.Fatal("audit feature must be off without audit storage")
	}
	s.Audit = &webui.AuditDeps{}
	if !feature() {
		t.Fatal("audit feature must be on with audit storage")
	}
}

func waitUser(t *testing.T, name string) {
	t.Helper()
	waitFor(t, func() bool {
		var u devpodv1alpha1.User
		return k8sClient.Get(context.Background(), clientKey(name), &u) == nil
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func clientKey(name string) types.NamespacedName { return types.NamespacedName{Name: name} }

func clientKeyNS(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "devpods", Name: name}
}
