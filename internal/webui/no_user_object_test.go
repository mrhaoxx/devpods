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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func userObject(t *testing.T, name string) (*devpodv1alpha1.User, bool) {
	t.Helper()
	var u devpodv1alpha1.User
	err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: name}, &u)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return &u, true
}

func dropUser(name string) {
	_ = k8sClient.Delete(context.Background(), &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

// Someone whose identity is the LDAP directory's has no User object until
// they keep something of their own here. Their settings pages are empty,
// not broken, and the first thing they save makes the object.
func TestSettingsWithoutAUserObject(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	t.Cleanup(func() { dropUser("nu-reg"); dropUser("nu-tok") })
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		return apiCall(t, srv, method, path, auth, body, want)
	}

	reg := forge(sm, "nu-reg", false)
	if items := call("GET", "/api/me/registries", reg, "", http.StatusOK)["items"].([]any); len(items) != 0 {
		t.Fatalf("registries %v", items)
	}
	call("DELETE", "/api/me/registries/harbor.test", reg, "", http.StatusNotFound)
	if _, there := userObject(t, "nu-reg"); there {
		t.Fatal("looking made a User object")
	}
	call("PUT", "/api/me/registries/harbor.test", reg, `{"username":"robot","password":"s3cret"}`, http.StatusOK)
	u, there := userObject(t, "nu-reg")
	if !there || len(u.Spec.Registries) != 1 || u.Spec.Registries[0].Host != "harbor.test" || len(u.Spec.Pubkeys) != 0 {
		t.Fatalf("after saving a registry: %v %+v", there, u)
	}
	if items := call("GET", "/api/me/registries", reg, "", http.StatusOK)["items"].([]any); len(items) != 1 {
		t.Fatalf("registries after saving: %v", items)
	}

	tok := forge(sm, "nu-tok", false)
	list := call("GET", "/api/me/tokens", tok, "", http.StatusOK)
	if items := list["items"].([]any); len(items) != 0 || list["scopes"] == nil {
		t.Fatalf("tokens %v", list)
	}
	call("DELETE", "/api/me/tokens/nope", tok, "", http.StatusNotFound)
	created := call("POST", "/api/me/tokens", tok, `{"name":"ci","scopes":["devpods:read"]}`, http.StatusCreated)
	if created["token"] == nil {
		t.Fatalf("created %v", created)
	}
	if u, there := userObject(t, "nu-tok"); !there || len(u.Spec.APITokens) != 1 {
		t.Fatalf("after making a token: %v %+v", there, u)
	}
	// the token works (the cache needs a moment to see the new User)
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, _ := http.NewRequest("GET", srv.URL+"/api/devpods", nil)
		req.Header.Set("Authorization", "Bearer "+created["token"].(string))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new token: %d", resp.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
