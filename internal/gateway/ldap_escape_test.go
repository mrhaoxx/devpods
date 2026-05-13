// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import "testing"

func TestEscapeRFC4515(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"safe", "alice", "alice"},
		{"hyphen-and-digits", "alice-42", "alice-42"},
		{"paren-open", "alice(", `alice\28`},
		{"paren-close", "alice)", `alice\29`},
		{"asterisk", "*", `\2a`},
		{"backslash", `alice\`, `alice\5c`},
		{"nul", "alice\x00", `alice\00`},
		{"injection-attempt", "alice)(uid=*", `alice\29\28uid=\2a`},
		{"backslash-must-be-first", `\(`, `\5c\28`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeRFC4515(tc.in); got != tc.want {
				t.Errorf("escapeRFC4515(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
