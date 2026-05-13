// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import "strings"

// escapeRFC4515 escapes a value for safe substitution inside an LDAP
// search filter (RFC 4515 §3). Order matters: backslash MUST be
// rewritten first so the subsequent escapes don't get re-escaped.
func escapeRFC4515(s string) string {
	if s == "" {
		return s
	}
	// Fast path: all bytes safe.
	safe := true
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', 0x00, '(', ')', '*':
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\5c`)
		case 0x00:
			b.WriteString(`\00`)
		case '(':
			b.WriteString(`\28`)
		case ')':
			b.WriteString(`\29`)
		case '*':
			b.WriteString(`\2a`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
