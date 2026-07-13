// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// testPubkey generates a real OpenSSH-format ed25519 pubkey — the
// handler runs ssh.ParseAuthorizedKey on it, so it must be genuine.
func testPubkey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " test@example"
}

func TestMeAndPubkeys(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)

	// ensure User exists (auto-provision normally does this)
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-alice"}}
	_ = k8sClient.Create(context.Background(), u)

	t.Run("me reports identity, quota and usage", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d", rec.Code)
		}
		waitDevPod(t, "gl-alice-dev1")
		rec := doJSON(t, s.HandleMeForTest(), "GET", "/api/me", nil, alice, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		var body struct {
			User       string `json:"user"`
			Admin      bool   `json:"admin"`
			NameBudget int    `json:"nameBudget"`
			Usage      struct {
				DevPods int               `json:"devpods"`
				Compute map[string]string `json:"compute"`
			} `json:"usage"`
			Features struct {
				PubkeySelfService bool `json:"pubkeySelfService"`
				Kore              bool `json:"kore"`
			} `json:"features"`
			SSH struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"ssh"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.User != "gl-alice" || body.NameBudget != 13 || body.Usage.DevPods != 1 || body.Usage.Compute["cpu"] != "2" {
			t.Fatalf("body = %+v", body)
		}
		if !body.Features.PubkeySelfService || !body.Features.Kore {
			t.Fatalf("features = %+v", body.Features)
		}
		if body.SSH.Host != "gw.example.com" || body.SSH.Port != 2222 {
			t.Fatalf("ssh = %+v", body.SSH)
		}
	})

	t.Run("pubkey self-service disabled", func(t *testing.T) {
		s.PubkeySelfService = false
		defer func() { s.PubkeySelfService = true }()
		rec := doJSON(t, s.HandlePutPubkeysForTest(), "PUT", "/api/me/pubkeys", nil, alice,
			`{"pubkeys":[]}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("pubkeys roundtrip", func(t *testing.T) {
		rec := doJSON(t, s.HandlePutPubkeysForTest(), "PUT", "/api/me/pubkeys", nil, alice,
			`{"pubkeys":["`+testPubkey(t)+`"]}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ssh-ed25519") {
			t.Fatalf("put: %d %s", rec.Code, rec.Body)
		}
		var u devpodv1alpha1.User
		if err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: "gl-alice"}, &u); err != nil {
			t.Fatal(err)
		}
		if len(u.Spec.Pubkeys) != 1 {
			t.Fatalf("pubkeys = %v", u.Spec.Pubkeys)
		}
		// GET reads through the cache; assert only the shape (the cached
		// copy may lag the PUT by a beat).
		rec = doJSON(t, s.HandleGetPubkeysForTest(), "GET", "/api/me/pubkeys", nil, alice, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pubkeys") {
			t.Fatalf("get: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid pubkey rejected", func(t *testing.T) {
		rec := doJSON(t, s.HandlePutPubkeysForTest(), "PUT", "/api/me/pubkeys", nil, alice,
			`{"pubkeys":["not a key"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestTemplateList(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)

	binding := pinTemplate()
	binding.Name = "pin8-list"
	preset := &devpodv1alpha1.DevPodTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-preset"},
		Spec: devpodv1alpha1.DevPodTemplateSpec{
			DisplayName: "Plain Ubuntu",
			PodPreset:   &devpodv1alpha1.PodPresetSpec{Image: "ubuntu:24.04"},
		},
	}
	ctx := context.Background()
	for _, tpl := range []*devpodv1alpha1.DevPodTemplate{binding, preset} {
		tpl := tpl
		if err := k8sClient.Create(ctx, tpl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, tpl) })
	}

	// Wait for both templates to land in the cache.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var list devpodv1alpha1.DevPodTemplateList
		if err := k8sClient.List(ctx, &list); err == nil && len(list.Items) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	rec := doJSON(t, s.HandleListTemplatesForTest(), "GET", "/api/templates", nil, alice, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	both := rec.Body.String()
	if !strings.Contains(both, "pin8-list") || !strings.Contains(both, "plain-preset") {
		t.Fatalf("missing templates: %s", both)
	}

	s.KoreEnabled = false
	rec = doJSON(t, s.HandleListTemplatesForTest(), "GET", "/api/templates", nil, alice, "")
	filtered := rec.Body.String()
	if strings.Contains(filtered, "pin8-list") || !strings.Contains(filtered, "plain-preset") {
		t.Fatalf("kore-off filtering broken: %s", filtered)
	}
	s.KoreEnabled = true
}
