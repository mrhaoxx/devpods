// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestMapUsername(t *testing.T) {
	cases := []struct {
		name, prefix, gitlab, want string
		wantErr                    bool
	}{
		{"plain", "", "alice", "alice", false},
		{"prefixed", "gl-", "alice", "gl-alice", false},
		{"uppercase rejected", "", "Alice", "", true},
		{"dot rejected", "gl-", "a.lice", "", true},
		{"underscore rejected", "", "a_lice", "", true},
		{"empty gitlab rejected", "gl-", "", "", true},
		{"no room for pod names", "verylongprefix-", "verylonguser", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := webui.MapUsername(tc.prefix, tc.gitlab)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNameBudget(t *testing.T) {
	if got := webui.NameBudget("gl-alice"); got != 13 { // 22 - 8 - 1
		t.Fatalf("budget = %d, want 13", got)
	}
}
