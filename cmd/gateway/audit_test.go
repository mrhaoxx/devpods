// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"text/template"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func gcWith(a *devpodv1alpha1.AuditSpec) *devpodv1alpha1.GatewayConfig {
	return &devpodv1alpha1.GatewayConfig{Spec: devpodv1alpha1.GatewayConfigSpec{Audit: a}}
}

func TestAuditEnabled(t *testing.T) {
	if auditEnabled(nil) || auditEnabled(gcWith(nil)) || auditEnabled(gcWith(&devpodv1alpha1.AuditSpec{})) {
		t.Fatal("audit must be off unless spec.audit.enabled")
	}
	if !auditEnabled(gcWith(&devpodv1alpha1.AuditSpec{Enabled: true})) {
		t.Fatal("enabled audit not detected")
	}
}

func TestSessionPolicy_UserOverride(t *testing.T) {
	deny, allow := devpodv1alpha1.PolicyDeny, devpodv1alpha1.PolicyAllow
	gc := gcWith(&devpodv1alpha1.AuditSpec{Enabled: true, Policy: devpodv1alpha1.AuditPolicy{Download: &deny, Upload: &deny}})
	u := &devpodv1alpha1.User{Spec: devpodv1alpha1.UserSpec{AuditPolicy: &devpodv1alpha1.AuditPolicy{Download: &allow}}}
	p := sessionPolicy(gc, u)
	if p.DenyDownload || !p.DenyUpload {
		t.Fatalf("got %+v", p)
	}
	if p := sessionPolicy(gc, nil); !p.DenyDownload {
		t.Fatal("no User CR: cluster default applies")
	}
	if p := sessionPolicy(nil, u); p.DenyDownload {
		t.Fatal("no GatewayConfig: user override still applies")
	}
}

type fakeMeta struct{ user string }

func (m fakeMeta) User() string          { return m.user }
func (m fakeMeta) SessionID() []byte     { return nil }
func (m fakeMeta) ClientVersion() []byte { return nil }
func (m fakeMeta) ServerVersion() []byte { return nil }
func (m fakeMeta) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5555} }
func (m fakeMeta) LocalAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 22} }

func TestBannerText_AuditNotice(t *testing.T) {
	tmpl := template.Must(template.New("b").Parse("hi {{.User}}\n"))
	meta := fakeMeta{user: "alice+dev"}

	on := gcWith(&devpodv1alpha1.AuditSpec{Enabled: true})
	got := bannerText(on, tmpl, meta)
	if first, rest, _ := strings.Cut(got, "\n"); first != DefaultAuditNotice || rest != "hi alice\n" {
		t.Fatalf("enabled, default notice: %q", got)
	}
	custom := gcWith(&devpodv1alpha1.AuditSpec{Enabled: true, Notice: "recorded"})
	if got := bannerText(custom, tmpl, meta); !strings.HasPrefix(got, "recorded\n") {
		t.Fatalf("custom notice: %q", got)
	}
	if got := bannerText(gcWith(nil), tmpl, meta); got != "hi alice\n" {
		t.Fatalf("disabled must be the template only: %q", got)
	}
	if got := bannerText(on, nil, meta); strings.TrimSpace(got) != DefaultAuditNotice {
		t.Fatalf("no template: %q", got)
	}
	if got := bannerText(nil, nil, meta); got != "" {
		t.Fatalf("nothing configured: %q", got)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"21474836480": 21474836480, "20Gi": 20 << 30, "512Mi": 512 << 20}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "lots", "-1", "0"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should fail", bad)
		}
	}
}

func TestAuditDeps_GatewayConfigName(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = devpodv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&devpodv1alpha1.GatewayConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&devpodv1alpha1.GatewayConfig{ObjectMeta: metav1.ObjectMeta{Name: "audit-e2e"}, Spec: devpodv1alpha1.GatewayConfigSpec{Audit: &devpodv1alpha1.AuditSpec{Enabled: true}}},
	).Build()
	if gc := (&auditDeps{cache: c, gcName: "audit-e2e"}).gatewayConfig(context.Background()); !auditEnabled(gc) {
		t.Fatalf("named GatewayConfig not used: %+v", gc)
	}
	if gc := (&auditDeps{cache: c, gcName: "default"}).gatewayConfig(context.Background()); auditEnabled(gc) {
		t.Fatal("default GatewayConfig has audit off")
	}
}
