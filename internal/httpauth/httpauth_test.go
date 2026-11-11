// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package httpauth

import (
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestTokens(t *testing.T) {
	key, now := []byte("k3y"), time.Unix(1000, 0)
	tok := Sign(key, Claims{Kind: KindTicket, ID: "abc", User: "alice", Exp: now.Add(time.Minute).Unix()})
	c, err := Verify(key, tok, KindTicket, now)
	if err != nil || c.ID != "abc" || c.User != "alice" {
		t.Fatalf("verify: %+v %v", c, err)
	}
	if _, err := Verify(key, tok, KindCookie, now); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a ticket passed as a cookie: %v", err)
	}
	if _, err := Verify([]byte("other"), tok, KindTicket, now); !errors.Is(err, ErrBadToken) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := Verify(key, tok[:len(tok)-2]+"xx", KindTicket, now); !errors.Is(err, ErrBadToken) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := Verify(key, tok, KindTicket, now.Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := Verify(nil, tok, KindTicket, now); !errors.Is(err, ErrBadToken) {
		t.Fatalf("no key must never verify: %v", err)
	}
}

func TestIDsPortsAccess(t *testing.T) {
	a, b := NewID(), NewID()
	if !ValidID(a) || a == b || ValidID("has-hyphen12") || ValidID("short") {
		t.Fatalf("ids %q %q", a, b)
	}
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{PublicPortsAnnotation: "8888, 8080,bad,0,8080"}},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "alice", Collaborators: []string{"bob"}}}
	if got := FormatPorts(PublicPorts(dp)); got != "8080,8888" {
		t.Fatalf("ports %q", got)
	}
	for user, want := range map[string]bool{"alice": true, "bob": true, "carol": false, "": false} {
		if Allowed(dp, user, false) != want {
			t.Fatalf("allowed(%q) != %v", user, want)
		}
	}
	if !Allowed(dp, "root", true) {
		t.Fatal("admin")
	}
}

func TestPortAccess(t *testing.T) {
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{PublicPortsAnnotation: "8080,9000", UserPortsAnnotation: "8888,9000"}},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "alice", Collaborators: []string{"bob"}}}
	for port, want := range map[int]string{8080: AccessPublic, 8888: AccessUsers, 9000: AccessPublic, 7000: AccessPrivate} {
		if got := Access(dp, port); got != want {
			t.Fatalf("Access(%d) = %s, want %s", port, got, want)
		}
	}
	for _, tc := range []struct {
		port  int
		user  string
		admin bool
		want  bool
	}{
		{8080, "", false, true},       // public: no sign-in
		{8888, "", false, false},      // signed-in users only
		{8888, "carol", false, true},  // any signed-in user
		{7000, "carol", false, false}, // private: not a collaborator
		{7000, "bob", false, true},
		{7000, "alice", false, true},
		{7000, "root", true, true},
	} {
		if got := MayOpen(dp, tc.port, tc.user, tc.admin); got != tc.want {
			t.Fatalf("MayOpen(%d, %q, admin=%v) = %v", tc.port, tc.user, tc.admin, got)
		}
	}
}
