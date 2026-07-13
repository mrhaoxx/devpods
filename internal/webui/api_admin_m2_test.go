// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestAdminGlobalDevPods(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	admin := forge(sm, "gl-root", true)
	alice := forge(sm, "gl-alice", false)
	t.Cleanup(func() { cleanupDevPods(t) })

	// Two owners' DevPods.
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("alice create: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "gl-bob", false),
		`{"name":"box","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bob create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "gl-alice-dev1")
	waitDevPod(t, "gl-bob-box")

	t.Run("admin sees all owners", func(t *testing.T) {
		rec := doJSON(t, s.HandleAdminListDevPodsForTest(), "GET", "/api/admin/devpods", nil, admin, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "gl-alice-dev1") || !strings.Contains(body, "gl-bob-box") {
			t.Fatalf("admin should see both: %s", body)
		}
	})

	t.Run("non-admin 403", func(t *testing.T) {
		rec := doJSON(t, s.HandleAdminListDevPodsForTest(), "GET", "/api/admin/devpods", nil, alice, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestAdminQuotaEdit(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.PasswordAuth = true
	s.PasswordMinLength = 8
	s.Admins = map[string]bool{"gl-root": true}
	admin := forge(sm, "gl-root", true)
	ctx := context.Background()

	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "quotauser"}}
	if err := s.Client.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Client.Delete(ctx, u) })

	t.Run("set quota", func(t *testing.T) {
		rec := doJSON(t, s.HandlePatchUserForTest(), "PATCH", "/api/admin/users/quotauser",
			map[string]string{"name": "quotauser"}, admin,
			`{"quota":{"maxDevPods":3,"cpu":"8","memory":"16Gi","storage":"50Gi"}}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("patch = %d %s", rec.Code, rec.Body)
		}
		var got devpodv1alpha1.User
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: "quotauser"}, &got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Quota == nil || got.Spec.Quota.MaxDevPods == nil || *got.Spec.Quota.MaxDevPods != 3 {
			t.Fatalf("quota not set: %+v", got.Spec.Quota)
		}
		if got.Spec.Quota.Compute.Cpu().String() != "8" || got.Spec.Quota.Storage.String() != "50Gi" {
			t.Fatalf("compute/storage wrong: %+v", got.Spec.Quota)
		}
	})

	t.Run("clear quota with empty patch", func(t *testing.T) {
		rec := doJSON(t, s.HandlePatchUserForTest(), "PATCH", "/api/admin/users/quotauser",
			map[string]string{"name": "quotauser"}, admin, `{"quota":{}}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("patch = %d %s", rec.Code, rec.Body)
		}
		var got devpodv1alpha1.User
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: "quotauser"}, &got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Quota != nil {
			t.Fatalf("quota should be cleared, got %+v", got.Spec.Quota)
		}
	})

	t.Run("invalid quantity 400", func(t *testing.T) {
		rec := doJSON(t, s.HandlePatchUserForTest(), "PATCH", "/api/admin/users/quotauser",
			map[string]string{"name": "quotauser"}, admin, `{"quota":{"cpu":"lots"}}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("list reports quota + usage", func(t *testing.T) {
		// set a quota, then confirm the list surfaces it
		if rec := doJSON(t, s.HandlePatchUserForTest(), "PATCH", "/api/admin/users/quotauser",
			map[string]string{"name": "quotauser"}, admin, `{"quota":{"cpu":"4"}}`); rec.Code != http.StatusNoContent {
			t.Fatalf("set: %d", rec.Code)
		}
		// The list reads through the cache; wait for the update to land.
		for i := 0; i < 100; i++ {
			var u devpodv1alpha1.User
			if err := s.Client.Get(ctx, types.NamespacedName{Name: "quotauser"}, &u); err == nil &&
				u.Spec.Quota != nil && u.Spec.Quota.Compute.Cpu().String() == "4" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		rec := doJSON(t, s.HandleListUsersForTest(), "GET", "/api/admin/users", nil, admin, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d", rec.Code)
		}
		var body struct {
			Items []struct {
				Name  string                    `json:"name"`
				Quota *devpodv1alpha1.UserQuota `json:"quota"`
			} `json:"items"`
			DefaultQuota devpodv1alpha1.UserQuota `json:"defaultQuota"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, it := range body.Items {
			if it.Name == "quotauser" {
				found = true
				if it.Quota == nil || it.Quota.Compute.Cpu().String() != "4" {
					t.Fatalf("quota not surfaced: %+v", it.Quota)
				}
			}
		}
		if !found {
			t.Fatal("quotauser missing from list")
		}
	})
}
