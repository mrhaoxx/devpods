// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// A session says who signed in; whether they are an admin is the User
// object's say on every request, not what it was at sign-in.
func TestSessionAdminFollowsUser(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "sf-carol"}, Spec: devpodv1alpha1.UserSpec{Admin: true}}
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), u) })
	// made an admin after signing in: an admin now
	apiCall(t, srv, "GET", "/api/admin/users", forge(sm, "sf-carol", false), "", http.StatusOK)
	// no longer one: a cookie from when she was is not enough
	u.Spec.Admin = false
	if err := k8sClient.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	apiCall(t, srv, "GET", "/api/admin/users", forge(sm, "sf-carol", true), "", http.StatusForbidden)
}

// A password user whose User object is gone (deleted) is signed out at
// once; one of the directory, who may have no object, is not.
func TestSessionOfADeletedPasswordUser(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "sf-dave"}}
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), u) })
	cookie := func(user, src string) *http.Cookie {
		return &http.Cookie{Name: webui.SessionCookie, Value: sm.MintFrom(user, false, src, time.Now())}
	}
	apiCall(t, srv, "GET", "/api/me", cookie("sf-dave", webui.SignedInWithPassword), "", http.StatusOK)
	if err := k8sClient.Delete(ctx, u); err != nil {
		t.Fatal(err)
	}
	apiCall(t, srv, "GET", "/api/me", cookie("sf-dave", webui.SignedInWithPassword), "", http.StatusUnauthorized)
	apiCall(t, srv, "GET", "/api/me", cookie("sf-erin", webui.SignedInWithLDAP), "", http.StatusOK)
}
