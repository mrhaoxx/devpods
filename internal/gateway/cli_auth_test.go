// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

func TestParseCLILogin(t *testing.T) {
	for _, tc := range []struct {
		cli, login, user string
		ok               bool
	}{
		{"devpod", "devpod", "", true},
		{"devpod", "devpod+alice", "alice", true},
		{"devpod", "alice+devpod", "", false}, // alice's DevPod named devpod
		{"devpod", "devpod+Alice", "", false},
		{"devpod", "devpod+a+b", "", false},
		{"devpod", "devpods", "", false},
		{"", "devpod", "", false}, // disabled
		{"cli", "cli+bob", "bob", true},
	} {
		if u, ok := gateway.ParseCLILogin(tc.cli, tc.login); u != tc.user || ok != tc.ok {
			t.Errorf("ParseCLILogin(%q, %q) = %q, %v", tc.cli, tc.login, u, ok)
		}
	}
}

func TestAuthenticateCLI(t *testing.T) {
	ctx := context.Background()
	alice, aliceLine := ed25519Pubkey(t)
	shared, sharedLine := ed25519Pubkey(t)
	proxy, _ := ed25519Pubkey(t)
	stranger, _ := ed25519Pubkey(t)
	user := func(name string, keys ...string) *devpodv1alpha1.User {
		return &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: devpodv1alpha1.UserSpec{Pubkeys: keys}}
	}
	c := fakeClient(t, user("alice", aliceLine, sharedLine), user("bob", sharedLine))
	a := gateway.NewAuthenticator(c, "devpods").WithProxyKeys(map[string]string{ssh.FingerprintSHA256(proxy): "openng"})

	// the bare login finds the user by key
	if u, ap, err := a.AuthenticateCLI(ctx, "", alice); err != nil || u != "alice" || ap.Kind != "direct" || ap.Source != "crd" {
		t.Fatalf("bare: %q %+v %v", u, ap, err)
	}
	if _, _, err := a.AuthenticateCLI(ctx, "", shared); !errors.Is(err, gateway.ErrCLIAmbiguousKey) {
		t.Fatalf("a key two users hold: %v", err)
	}
	if _, _, err := a.AuthenticateCLI(ctx, "", stranger); !errors.Is(err, gateway.ErrPubkeyMismatch) {
		t.Fatalf("unknown key: %v", err)
	}
	// naming the user: the key must be theirs
	if u, _, err := a.AuthenticateCLI(ctx, "bob", shared); err != nil || u != "bob" {
		t.Fatalf("named: %q %v", u, err)
	}
	if _, _, err := a.AuthenticateCLI(ctx, "bob", alice); !errors.Is(err, gateway.ErrPubkeyMismatch) {
		t.Fatalf("someone else's name: %v", err)
	}
	if _, _, err := a.AuthenticateCLI(ctx, "carol", alice); !errors.Is(err, gateway.ErrUserNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	// a trusted proxy must say whom it acts for, and that user must exist
	if u, ap, err := a.AuthenticateCLI(ctx, "alice", proxy); err != nil || u != "alice" || ap.Kind != "trusted_proxy" || ap.Alias != "openng" {
		t.Fatalf("proxy: %q %+v %v", u, ap, err)
	}
	if _, _, err := a.AuthenticateCLI(ctx, "", proxy); !errors.Is(err, gateway.ErrCLINeedsUser) {
		t.Fatalf("proxy without a user: %v", err)
	}
	if _, _, err := a.AuthenticateCLI(ctx, "carol", proxy); !errors.Is(err, gateway.ErrUserNotFound) {
		t.Fatalf("proxy for an unknown user: %v", err)
	}
}
