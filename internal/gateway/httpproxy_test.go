// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"strings"
	"testing"
)

func TestParseSubdomain(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantPort int
		wantErr  bool
	}{
		{"st43-dev-8080", "st43-dev", 8080, false},
		{"scratch-3000", "scratch", 3000, false},
		{"my-pod-443", "my-pod", 443, false},
		{"a-1", "a", 1, false},
		{"dev-65535", "dev", 65535, false},
		// errors
		{"", "", 0, true},
		{"noport", "", 0, true},
		{"-8080", "", 0, true},
		{"dev-0", "", 0, true},
		{"dev-99999", "", 0, true},
		{"dev-abc", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, port, err := parseSubdomain(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tc.wantName || port != tc.wantPort {
				t.Errorf("got (%q, %d), want (%q, %d)", name, port, tc.wantName, tc.wantPort)
			}
		})
	}
}

func TestHTTPProxy_SuffixStripping(t *testing.T) {
	// Simulate the full host→subdomain→parse flow with suffix "-dev"
	// Host: st43-dev-8080-dev.ktaas.approaching-ai.com
	host := "st43-dev-8080-dev.ktaas.approaching-ai.com"
	baseDomain := "ktaas.approaching-ai.com"
	suffix := "-dev"

	domainSuffix := "." + baseDomain
	if !strings.HasSuffix(host, domainSuffix) {
		t.Fatal("host does not match base domain")
	}
	subdomain := strings.TrimSuffix(host, domainSuffix)
	if !strings.HasSuffix(subdomain, suffix) {
		t.Fatalf("subdomain %q missing suffix %q", subdomain, suffix)
	}
	subdomain = strings.TrimSuffix(subdomain, suffix)

	name, port, err := parseSubdomain(subdomain)
	if err != nil {
		t.Fatal(err)
	}
	if name != "st43-dev" || port != 8080 {
		t.Errorf("got (%q, %d), want (st43-dev, 8080)", name, port)
	}
}
