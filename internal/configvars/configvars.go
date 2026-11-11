// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package configvars finds and fills the variables of a saved DevPod
// config (config center spec §3):
//
//	${VAR}            a variable that must be given
//	${VAR:-default}   one with a default, which ends at the first };
//	                  an empty value takes the default too
//	$${               a literal ${
//
// A name is [A-Z][A-Z0-9_]*; any other ${...} — a shell's ${home} — is
// text. The text is read once, left to right: what a value brings in is
// never read as a variable.
package configvars

import (
	"fmt"
	"sort"
	"strings"
)

// The variables with a meaning of their own.
const (
	Owner = "OWNER" // the user launching; filled by the server
	Name  = "NAME"  // the DevPod's full name
	Node  = "NODE"  // the node it is pinned to
	Index = "INDEX" // its number in a batch, from 1; filled by the server
)

// MaxValueLen bounds one value, in bytes.
const MaxValueLen = 1024

// Var is one variable of a config.
type Var struct {
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	// Required: the person launching has to give it — no default, and
	// not one the server fills.
	Required bool   `json:"required"`
	Default  string `json:"default,omitempty"`
}

// MissingError: variables without a default that were not given.
type MissingError struct{ Names []string }

func (e *MissingError) Error() string {
	return "missing variables: " + strings.Join(e.Names, ", ")
}

// UnknownError: values given for variables the config does not have.
type UnknownError struct{ Names []string }

func (e *UnknownError) Error() string {
	return "unknown variables: " + strings.Join(e.Names, ", ")
}

// ValueError: a value that cannot be put into the text.
type ValueError struct{ Name, Reason string }

func (e *ValueError) Error() string { return fmt.Sprintf("variable %s: %s", e.Name, e.Reason) }

// ref is one ${...} of the text.
type ref struct {
	name       string
	def        string
	hasDefault bool
}

func nameEnd(s string, i int) int {
	if i >= len(s) || s[i] < 'A' || s[i] > 'Z' {
		return i
	}
	for i < len(s) && (s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9' || s[i] == '_') {
		i++
	}
	return i
}

// scan walks content once. text gets every stretch that is not a
// variable (with $${ already turned into ${); variable gets each ${VAR}.
func scan(content string, text func(string), variable func(ref)) {
	start := 0 // of the pending literal stretch
	for i := 0; i < len(content); {
		if content[i] != '$' {
			i++
			continue
		}
		rest := content[i:]
		if strings.HasPrefix(rest, "$${") {
			text(content[start:i] + "${")
			i += 3
			start = i
			continue
		}
		if !strings.HasPrefix(rest, "${") {
			i++
			continue
		}
		end := nameEnd(content, i+2)
		if end == i+2 || end >= len(content) {
			i++
			continue
		}
		r := ref{name: content[i+2 : end]}
		next := end
		switch {
		case content[end] == '}':
			next = end + 1
		case strings.HasPrefix(content[end:], ":-"):
			close := strings.IndexByte(content[end:], '}')
			if close < 0 {
				i++
				continue
			}
			r.def, r.hasDefault = content[end+2:end+close], true
			next = end + close + 1
		default:
			i++
			continue
		}
		text(content[start:i])
		variable(r)
		i, start = next, next
	}
	text(content[start:])
}

func builtin(name string) bool {
	return name == Owner || name == Name || name == Node || name == Index
}

// collect lists the variables in order of first appearance; a variable
// has a default as soon as one of its appearances carries one, and the
// first such default is the one used everywhere.
func collect(content string) ([]string, map[string]*ref) {
	var order []string
	byName := map[string]*ref{}
	scan(content, func(string) {}, func(r ref) {
		have, ok := byName[r.name]
		if !ok {
			order = append(order, r.name)
			byName[r.name] = &r
			return
		}
		if r.hasDefault && !have.hasDefault {
			have.def, have.hasDefault = r.def, true
		}
	})
	return order, byName
}

// Parse lists the variables of content in order of first appearance.
func Parse(content string) []Var {
	order, byName := collect(content)
	out := make([]Var, 0, len(order))
	for _, n := range order {
		r := byName[n]
		out = append(out, Var{
			Name:     n,
			Builtin:  builtin(n),
			Required: !r.hasDefault && n != Owner && n != Index,
			Default:  r.def,
		})
	}
	return out
}

// Uses reports whether content has the variable name.
func Uses(content, name string) bool {
	_, byName := collect(content)
	_, ok := byName[name]
	return ok
}

func checkValue(name, v string) error {
	if len(v) > MaxValueLen {
		return &ValueError{Name: name, Reason: fmt.Sprintf("longer than %d bytes", MaxValueLen)}
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return &ValueError{Name: name, Reason: "line breaks and control characters are not allowed"}
		}
	}
	return nil
}

// Render fills the variables of content from values. Every variable
// without a default must be given, the server-filled ones included.
func Render(content string, values map[string]string) (string, error) {
	order, byName := collect(content)
	var unknown []string
	for n := range values {
		if _, ok := byName[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", &UnknownError{Names: unknown}
	}
	var missing []string
	for _, n := range order {
		v, given := values[n]
		if !given {
			if !byName[n].hasDefault {
				missing = append(missing, n)
			}
			continue
		}
		if err := checkValue(n, v); err != nil {
			return "", err
		}
	}
	if len(missing) > 0 {
		return "", &MissingError{Names: missing}
	}
	var b strings.Builder
	scan(content, func(s string) { b.WriteString(s) }, func(r ref) {
		// as in a shell, :- gives the default for an empty value too
		if v, given := values[r.name]; given && (v != "" || !byName[r.name].hasDefault) {
			b.WriteString(v)
			return
		}
		b.WriteString(byName[r.name].def)
	})
	return b.String(), nil
}
