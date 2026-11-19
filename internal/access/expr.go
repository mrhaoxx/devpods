// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

// A rule's condition (design 2026-11-19 §5): tests of a cluster, a
// machine, a user or a group, combined with and, or, not and parentheses.
//
//	cluster in (st2, a5)
//	cluster = st and machine != st5 and (group = ops or user = star)
//	everyone and not group = interns
//
// An item is a name ("*", "?" and "[...]" globs allowed) or, for a
// machine, a label key=value; items with other characters, or named like
// a keyword, are quoted ("..."; \" and \\ escaped). The empty condition is
// true, as is everyone.

// Field is what a test is about.
type Field uint8

const (
	FieldCluster Field = iota
	FieldMachine
	FieldUser
	FieldGroup
)

var fieldNames = [...]string{"cluster", "machine", "user", "group"}

func (f Field) String() string { return fieldNames[f] }

func fieldOf(name string) (Field, bool) {
	for i, n := range fieldNames {
		if n == name {
			return Field(i), true
		}
	}
	return 0, false
}

type exprKind uint8

const (
	kindTrue exprKind = iota
	kindTest
	kindNot
	kindAnd
	kindOr
)

// Expr is a parsed condition.
type Expr struct {
	kind  exprKind
	kids  []*Expr // not: one; and, or: two or more
	field Field   // a test's
	items []string
	list  bool // "in (...)" rather than "= x"
}

// ParseError is where a condition is wrong (Offset counts characters —
// runes — from 0) and why.
type ParseError struct {
	Offset int
	Msg    string
}

func (e *ParseError) Error() string { return fmt.Sprintf("第 %d 个字符：%s", e.Offset+1, e.Msg) }

// keywords may not be unquoted items.
var keywords = map[string]bool{"and": true, "or": true, "not": true, "in": true, "everyone": true}

// Parse reads a condition; the error is a *ParseError.
func Parse(src string) (*Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	if p.peek().kind == tEOF {
		return &Expr{kind: kindTrue}, nil
	}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, &ParseError{t.off, "多余的 " + t.show()}
	}
	return e, nil
}

// Items: the items of every test of f, in order.
func (e *Expr) Items(f Field) []string {
	var out []string
	var walk func(*Expr)
	walk = func(x *Expr) {
		if x.kind == kindTest && x.field == f {
			out = append(out, x.items...)
		}
		for _, k := range x.kids {
			walk(k)
		}
	}
	walk(e)
	return out
}

// HasGlob: an item with glob characters.
func HasGlob(item string) bool { return strings.ContainsAny(item, "*?[") }

// QuoteItem writes s as an item: bare when it may be, else quoted.
func QuoteItem(s string) string {
	bare := s != "" && !keywords[s]
	for _, r := range s {
		if !wordRune(r) {
			bare = false
		}
	}
	if bare {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

/* ---- evaluation ---- */

// Tri is a condition's value where not everything is known: Unknown is
// "it depends on what is not known" (Kleene's logic).
type Tri int8

const (
	False Tri = iota
	Unknown
	True
)

func (t Tri) String() string { return [...]string{"false", "unknown", "true"}[t] }

// Facts is what is known when a condition is judged; a nil field is not
// known (a machine to join later; any user, when counting machines).
// Groups are User's.
type Facts struct {
	Cluster *string
	Node    *Node
	User    *string
	Groups  map[string]bool
}

// Eval judges the condition on f.
func (e *Expr) Eval(f Facts) Tri {
	switch e.kind {
	case kindTrue:
		return True
	case kindNot:
		return True - e.kids[0].Eval(f)
	case kindAnd:
		v := True
		for _, k := range e.kids {
			v = min(v, k.Eval(f))
		}
		return v
	case kindOr:
		v := False
		for _, k := range e.kids {
			v = max(v, k.Eval(f))
		}
		return v
	}
	for _, item := range e.items {
		switch v := e.test(item, f); v {
		case True, Unknown:
			return v // Unknown: the field is not known, for every item alike
		}
	}
	return False
}

// test: one item on f.
func (e *Expr) test(item string, f Facts) Tri {
	known := func(ok bool, match bool) Tri {
		switch {
		case !ok:
			return Unknown
		case match:
			return True
		}
		return False
	}
	switch e.field {
	case FieldCluster:
		return known(f.Cluster != nil, f.Cluster != nil && globMatch(item, *f.Cluster))
	case FieldMachine:
		return known(f.Node != nil, f.Node != nil && MachineMatches([]string{item}, *f.Node))
	case FieldUser:
		return known(f.User != nil, f.User != nil && globMatch(item, *f.User))
	}
	in := false
	for g := range f.Groups {
		in = in || globMatch(item, g)
	}
	return known(f.User != nil, in)
}

func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

/* ---- lexing ---- */

type tokKind uint8

const (
	tEOF tokKind = iota
	tWord
	tString
	tLParen
	tRParen
	tComma
	tNotEq
)

type token struct {
	kind tokKind
	text string
	off  int
}

func (t token) show() string {
	switch t.kind {
	case tString:
		return QuoteItem(t.text)
	case tLParen:
		return "("
	case tRParen:
		return ")"
	case tComma:
		return ","
	case tNotEq:
		return "!="
	}
	return t.text
}

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-/*?[]:=", r)
}

func lex(src string) ([]token, error) {
	rs := []rune(src)
	var toks []token
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(':
			toks = append(toks, token{tLParen, "", i})
			i++
		case r == ')':
			toks = append(toks, token{tRParen, "", i})
			i++
		case r == ',':
			toks = append(toks, token{tComma, "", i})
			i++
		case r == '!' && i+1 < len(rs) && rs[i+1] == '=':
			toks = append(toks, token{tNotEq, "", i})
			i += 2
		case r == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' {
					if j+1 >= len(rs) || (rs[j+1] != '"' && rs[j+1] != '\\') {
						return nil, &ParseError{j, `引号里只能用 \" 和 \\ 转义`}
					}
					j++
				}
				b.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, &ParseError{i, "引号没有结束"}
			}
			toks = append(toks, token{tString, b.String(), i})
			i = j + 1
		case strings.ContainsRune("，（）＝！", r):
			return nil, &ParseError{i, fmt.Sprintf("请用半角字符，不要用全角的 %c", r)}
		case wordRune(r):
			j := i
			for j < len(rs) && wordRune(rs[j]) {
				j++
			}
			toks = append(toks, token{tWord, string(rs[i:j]), i})
			i = j
		default:
			return nil, &ParseError{i, fmt.Sprintf("不认识的字符 %c", r)}
		}
	}
	return append(toks, token{tEOF, "", len(rs)}), nil
}

/* ---- parsing: or > and > not > atom ---- */

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) isWord(w string) bool { t := p.peek(); return t.kind == tWord && t.text == w }

// chain parses sub (op sub)*: one, or an and/or of them all.
func (p *parser) chain(op string, kind exprKind, sub func() (*Expr, error)) (*Expr, error) {
	first, err := sub()
	if err != nil {
		return nil, err
	}
	kids := []*Expr{first}
	for p.isWord(op) {
		p.next()
		k, err := sub()
		if err != nil {
			return nil, err
		}
		kids = append(kids, k)
	}
	if len(kids) == 1 {
		return first, nil
	}
	return &Expr{kind: kind, kids: kids}, nil
}

func (p *parser) or() (*Expr, error) { return p.chain("or", kindOr, p.and) }

func (p *parser) and() (*Expr, error) { return p.chain("and", kindAnd, p.not) }

func (p *parser) not() (*Expr, error) {
	if !p.isWord("not") {
		return p.atom()
	}
	p.next()
	k, err := p.not()
	if err != nil {
		return nil, err
	}
	return &Expr{kind: kindNot, kids: []*Expr{k}}, nil
}

func (p *parser) atom() (*Expr, error) {
	t := p.peek()
	switch {
	case t.kind == tLParen:
		p.next()
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if c := p.peek(); c.kind != tRParen {
			return nil, &ParseError{c.off, "缺少 )"}
		}
		p.next()
		return e, nil
	case t.kind == tWord && t.text == "everyone":
		p.next()
		return &Expr{kind: kindTrue}, nil
	case t.kind == tWord:
		return p.test()
	}
	return nil, &ParseError{t.off, "缺少条件（如 cluster = st）"}
}

// test: field op value. The first = right after a field name is the
// operator: cluster=st2, machine=gpu=h100.
func (p *parser) test() (*Expr, error) {
	t := p.next()
	name, rest, eq := strings.Cut(t.text, "=")
	field, ok := fieldOf(name)
	if !ok {
		return nil, &ParseError{t.off, fmt.Sprintf("未知的字段 %s（可用 cluster、machine、user、group）", t.text)}
	}
	var op string
	var val *token // a value cut off the same word
	if eq {
		op = "="
		if rest != "" {
			val = &token{tWord, rest, t.off + len([]rune(name)) + 1}
		}
	} else {
		o := p.peek()
		switch {
		case o.kind == tNotEq:
			p.next()
			op = "!="
		case o.kind == tWord && strings.HasPrefix(o.text, "="):
			p.next()
			op = "="
			if o.text != "=" {
				val = &token{tWord, o.text[1:], o.off + 1}
			}
		case o.kind == tWord && o.text == "in":
			p.next()
			op = "in"
		case o.kind == tWord && o.text == "not":
			p.next()
			if !p.isWord("in") {
				return nil, &ParseError{p.peek().off, "not 后面要跟 in"}
			}
			p.next()
			op = "not in"
		default:
			return nil, &ParseError{o.off, "缺少运算符（= != in not in）"}
		}
	}
	e := &Expr{kind: kindTest, field: field}
	if op == "=" || op == "!=" {
		item, err := p.item(op, val)
		if err != nil {
			return nil, err
		}
		e.items = []string{item}
	} else {
		if l := p.peek(); l.kind != tLParen {
			return nil, &ParseError{l.off, op + " 后面要一个括号列表，如 (a, b)"}
		}
		p.next()
		e.list = true
		if r := p.peek(); r.kind == tRParen {
			return nil, &ParseError{r.off, "列表是空的"}
		}
		for {
			item, err := p.item(op, nil)
			if err != nil {
				return nil, err
			}
			e.items = append(e.items, item)
			s := p.next()
			if s.kind == tRParen {
				break
			}
			if s.kind != tComma {
				return nil, &ParseError{s.off, "列表里用逗号分开，以 ) 结束"}
			}
		}
	}
	if op == "!=" || op == "not in" {
		return &Expr{kind: kindNot, kids: []*Expr{e}}, nil
	}
	return e, nil
}

// item: one value — val if the operator's word carried it, else the next token.
func (p *parser) item(op string, val *token) (string, error) {
	t := val
	if t == nil {
		n := p.peek()
		if n.kind != tWord && n.kind != tString {
			return "", &ParseError{n.off, op + " 后面缺少值"}
		}
		p.next()
		t = &n
	}
	if t.kind == tWord && strings.HasPrefix(t.text, "=") {
		return "", &ParseError{t.off, "多了一个 =：比较用一个 =，不是 ==（值本身以 = 开头要加引号）"}
	}
	if t.kind == tWord && keywords[t.text] {
		return "", &ParseError{t.off, t.text + " 是关键字，作为值要加引号：\"" + t.text + "\""}
	}
	return t.text, nil
}
