// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package rawlog

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

var update = flag.Bool("update", false, "rewrite golden files")

type sliceIter struct {
	recs []Record
	i    int
}

func (s *sliceIter) Next() (Record, error) {
	if s.i >= len(s.recs) {
		return Record{}, io.EOF
	}
	s.i++
	return s.recs[s.i-1], nil
}

type castBuilder struct {
	t0   time.Time
	recs []Record
}

func (b *castBuilder) at(ms int, ch uint32, k Kind, p []byte) {
	b.recs = append(b.recs, Record{TS: b.t0.Add(time.Duration(ms) * time.Millisecond), Channel: ch, Kind: k, Payload: p})
}

func (b *castBuilder) open(ms int, ch uint32) {
	b.at(ms, ch, KindChannelOpen, Encode(ChannelOpen{Type: "session"}))
}

func (b *castBuilder) req(ms int, ch uint32, typ string, payload []byte) {
	b.at(ms, ch, KindRequest, Encode(Request{Type: typ, WantReply: true, OK: true, Payload: payload}))
}

func ptyPayload(term string, cols, rows uint32) []byte {
	return ssh.Marshal(struct {
		Term             string
		Cols, Rows, W, H uint32
		Modes            string
	}{term, cols, rows, 0, 0, ""})
}

func castOf(t *testing.T, recs []Record, ch uint32) []string {
	t.Helper()
	var out bytes.Buffer
	if err := WriteCast(&out, &sliceIter{recs: recs}, ch); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line %d is not valid JSON: %q", i, l)
		}
	}
	return lines
}

func TestWriteCast_Golden(t *testing.T) {
	b := &castBuilder{t0: time.Unix(1794210460, 0)}
	b.open(0, 1)
	b.req(10, 1, "pty-req", ptyPayload("xterm-256color", 120, 40))
	b.req(20, 1, "shell", nil)
	b.at(500, 1, KindDataOut, []byte("hello\r\n"))
	b.at(1500, 1, KindDataIn, []byte("ls\r"))
	b.req(2000, 1, "window-change", ssh.Marshal(struct{ C, R, W, H uint32 }{100, 30, 0, 0}))
	b.at(2250, 1, KindDataErr, []byte("a b\r\n"))
	b.at(2300, 1, KindInjectToClient, []byte("\r\n[devpod] blocked\r\n"))

	var out bytes.Buffer
	if err := WriteCast(&out, &sliceIter{recs: b.recs}, 1); err != nil {
		t.Fatal(err)
	}
	golden := "testdata/cast/simple.cast"
	if *update {
		if err := os.MkdirAll("testdata/cast", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("cast differs from golden:\n%s\nwant:\n%s", out.Bytes(), want)
	}
}

func TestWriteCast_UTF8SplitAndInvalid(t *testing.T) {
	b := &castBuilder{t0: time.Unix(100, 0)}
	b.open(0, 1)
	b.at(1, 1, KindDataOut, []byte("\xe4\xbd"))
	b.at(2, 1, KindDataOut, []byte("\xa0"))
	b.at(3, 1, KindDataOut, []byte("\xff"))
	lines := castOf(t, b.recs, 1)
	var texts []string
	for _, l := range lines[1:] {
		var ev []any
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, ev[2].(string))
	}
	joined := strings.Join(texts, "")
	if joined != "你�" {
		t.Fatalf("got %q", joined)
	}
}

func TestWriteCast_OtherChannelsExcluded(t *testing.T) {
	b := &castBuilder{t0: time.Unix(100, 0)}
	b.open(0, 1)
	b.open(0, 2)
	b.at(1, 2, KindDataOut, []byte("other"))
	b.at(2, 1, KindDataOut, []byte("mine"))
	lines := castOf(t, b.recs, 1)
	if len(lines) != 2 || !strings.Contains(lines[1], "mine") {
		t.Fatalf("got %v", lines)
	}
}

func TestWriteCast_NoPtyDefaults80x24(t *testing.T) {
	b := &castBuilder{t0: time.Unix(100, 0)}
	b.open(0, 1)
	b.req(1, 1, "exec", ssh.Marshal(struct{ S string }{"ls"}))
	lines := castOf(t, b.recs, 1)
	var h map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatal(err)
	}
	if h["width"].(float64) != 80 || h["height"].(float64) != 24 || h["version"].(float64) != 2 {
		t.Fatalf("header %v", h)
	}
	if _, ok := h["env"]; ok {
		t.Fatalf("no TERM expected without pty: %v", h)
	}
}
