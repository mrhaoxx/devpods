// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// show prints a condition as a prefix form: (and A B), (or A B), (not A),
// (cluster = st), (user in alice,bob), true.
func show(e *Expr) string {
	switch e.kind {
	case kindTrue:
		return "true"
	case kindNot:
		return "(not " + show(e.kids[0]) + ")"
	case kindAnd, kindOr:
		op := "and"
		if e.kind == kindOr {
			op = "or"
		}
		parts := []string{op}
		for _, k := range e.kids {
			parts = append(parts, show(k))
		}
		return "(" + strings.Join(parts, " ") + ")"
	}
	op := "="
	if e.list {
		op = "in"
	}
	return "(" + e.field.String() + " " + op + " " + strings.Join(e.items, ",") + ")"
}

func parsed(t *testing.T, src string) string {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return show(e)
}

func parseErr(t *testing.T, src string) *ParseError {
	t.Helper()
	_, err := Parse(src)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("Parse(%q): want a *ParseError, got %v", src, err)
	}
	return pe
}

func TestParsePrecedence(t *testing.T) {
	for src, want := range map[string]string{
		"cluster = a or cluster = b and not cluster = c": "(or (cluster = a) (and (cluster = b) (not (cluster = c))))",
		"(cluster = a or cluster = b) and cluster = c":   "(and (or (cluster = a) (cluster = b)) (cluster = c))",
		"not not user = x":                                           "(not (not (user = x)))",
		"cluster = a and machine = b and user = c":                   "(and (cluster = a) (machine = b) (user = c))",
		"cluster = st and not machine = st5 or user in (alice, bob)": "(or (and (cluster = st) (not (machine = st5))) (user in alice,bob))",
	} {
		if got := parsed(t, src); got != want {
			t.Errorf("%q:\n got  %s\n want %s", src, got, want)
		}
	}
}

func TestParseOperators(t *testing.T) {
	if a, b := parsed(t, "cluster != st"), parsed(t, "not cluster = st"); a != b {
		t.Errorf("!=: %s vs %s", a, b)
	}
	if a, b := parsed(t, "machine not in (a)"), parsed(t, "not machine in (a)"); a != b {
		t.Errorf("not in: %s vs %s", a, b)
	}
	if got := parsed(t, "group in (ops)"); got != "(group in ops)" {
		t.Errorf("a list of one: %s", got)
	}
}

func TestParseNoSpaces(t *testing.T) {
	if a, b := parsed(t, "cluster=st2"), parsed(t, "cluster = st2"); a != b {
		t.Errorf("cluster=st2: %s vs %s", a, b)
	}
	if got := parsed(t, "machine=gpu=h100"); got != "(machine = gpu=h100)" {
		t.Errorf("machine=gpu=h100: %s", got)
	}
	if got := parsed(t, "cluster!=st2"); got != "(not (cluster = st2))" {
		t.Errorf("cluster!=st2: %s", got)
	}
}

func TestParseLabelItems(t *testing.T) {
	if got := parsed(t, "machine = nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"); got != "(machine = nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3)" {
		t.Errorf("a label with dots and a slash: %s", got)
	}
	if got := parsed(t, "machine in (st1-*, gpu=B300)"); got != "(machine in st1-*,gpu=B300)" {
		t.Errorf("two items: %s", got)
	}
}

func TestParseQuotes(t *testing.T) {
	for src, want := range map[string]string{
		`cluster = "and"`:    "(cluster = and)",
		`user = "a\"b"`:      `(user = a"b)`,
		`user = "x\\y"`:      `(user = x\y)`,
		`user in ("a b", c)`: "(user in a b,c)",
	} {
		if got := parsed(t, src); got != want {
			t.Errorf("%s: %s, want %s", src, got, want)
		}
	}
	if pe := parseErr(t, "cluster = and"); pe.Offset != 10 {
		t.Errorf("a keyword as a value: %+v", pe)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, src := range []string{"", "  ", "everyone"} {
		if got := parsed(t, src); got != "true" {
			t.Errorf("%q: %s", src, got)
		}
	}
	if got := parsed(t, "everyone and not group = interns"); got != "(and true (not (group = interns)))" {
		t.Errorf("everyone and not: %s", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct {
		src    string
		offset int
		says   string
	}{
		{"cluster =", 9, "缺少值"},
		{"cluster in st", 11, "括号列表"},
		{"cluster = st and", 16, "缺少条件"},
		{"(cluster = st", 13, "缺少 )"},
		{"node = a", 0, "未知的字段"},
		{"cluster = st st2", 13, "多余"},
		{"Cluster = st", 0, "未知的字段"},
	} {
		pe := parseErr(t, c.src)
		if pe.Offset != c.offset || !strings.Contains(pe.Msg, c.says) {
			t.Errorf("%q: %+v, want offset %d saying %q", c.src, pe, c.offset, c.says)
		}
	}
	if got := parseErr(t, "node = a").Error(); !strings.HasPrefix(got, "第 1 个字符：") {
		t.Errorf("Error(): %q", got)
	}
}

func TestParseFullWidth(t *testing.T) {
	if pe := parseErr(t, "cluster in (st，yc)"); pe.Offset != 14 || !strings.Contains(pe.Msg, "半角") {
		t.Errorf("a full-width comma: %+v", pe)
	}
	if pe := parseErr(t, "cluster in （st）"); pe.Offset != 11 || !strings.Contains(pe.Msg, "半角") {
		t.Errorf("a full-width parenthesis: %+v", pe)
	}
}

func TestItemsAndQuote(t *testing.T) {
	e, err := Parse("group = a or not group in (b, c*) or user = d")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Items(FieldGroup); !slices.Equal(got, []string{"a", "b", "c*"}) {
		t.Errorf("Items(group): %v", got)
	}
	if !HasGlob("c*") || HasGlob("c") {
		t.Error("HasGlob")
	}
	for s, want := range map[string]string{"st2": "st2", "and": `"and"`, "a b": `"a b"`, `a"b`: `"a\"b"`, `a\*b`: `"a\\*b"`, "": `""`} {
		if got := QuoteItem(s); got != want {
			t.Errorf("QuoteItem(%q) = %s, want %s", s, got, want)
		}
		// what QuoteItem writes reads back as the same item
		if e, err := Parse("user = " + QuoteItem(s)); err != nil || e.items[0] != s {
			t.Errorf("round trip %q: %v", s, err)
		}
	}
}
