// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package gatewaycli is the DevPod command line served over SSH
// (ssh devpod@gateway <command>): the gateway authenticates the caller
// by SSH key and runs each command as a call to the console's REST API
// on that user's behalf, so the CLI, the console and API tokens obey the
// same rules.
package gatewaycli

import (
	"errors"
	"strings"
)

// Split breaks a command line into words the way a shell would for
// plain words and quotes ('…' literal, "…" with \" and \\, \x outside
// quotes). Nothing is expanded: no variables, globs or substitutions.
func Split(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false // a word is open (so '' yields an empty word)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated ' quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			in = true
		case c == '"':
			in = true
			i++
			for ; ; i++ {
				if i >= len(s) {
					return nil, errors.New(`unterminated " quote`)
				}
				if s[i] == '"' {
					break
				}
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("trailing backslash")
			}
			i++
			cur.WriteByte(s[i])
			in = true
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}
