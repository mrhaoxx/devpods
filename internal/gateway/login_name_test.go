// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"errors"
	"testing"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

func TestParseLoginName(t *testing.T) {
	tests := []struct {
		in       string
		wantUser string
		wantPod  string
		wantErr  bool
	}{
		{"alice+frontend-dev", "alice", "frontend-dev", false},
		{"alice", "", "", true},        // pod required
		{"alice+", "", "", true},
		{"+pod", "", "", true},
		{"alice+pod+extra", "", "", true},
		{"alice+../pod", "", "", true},
		{"alice+pod ", "", "", true},   // trailing whitespace
		{"", "", "", true},
		{"ALICE+pod", "", "", true},    // owner must match [a-z0-9-]
		{"alice+POD", "", "", true},    // pod must match [a-z0-9-]
		{"alice+p", "alice", "p", false},
		{"a+b", "a", "b", false},
		// Length-budget boundary (the {1,32} bound in the regex is load-bearing
		// for the User CRD contract; pin it in both directions).
		{
			"a234567890123456789012345678901+b234567890123456789012345678901",
			"a234567890123456789012345678901",
			"b234567890123456789012345678901",
			false,
		},
		{
			"a2345678901234567890123456789012+b",
			"a2345678901234567890123456789012",
			"b",
			false,
		},
		{"a23456789012345678901234567890123+b", "", "", true}, // 33-char owner: rejected
		// Newline-injection guard ($ in Go regex is end-of-string, not end-of-line).
		{"alice+pod\nevil", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			user, pod, err := gateway.ParseLoginName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if user != tc.wantUser || pod != tc.wantPod {
				t.Errorf("got (%q,%q), want (%q,%q)", user, pod, tc.wantUser, tc.wantPod)
			}
		})
	}
}

func TestParseLoginName_SentinelError(t *testing.T) {
	_, _, err := gateway.ParseLoginName("nope")
	if !errors.Is(err, gateway.ErrInvalidLoginName) {
		t.Fatalf("err = %v, want wraps ErrInvalidLoginName", err)
	}
}
