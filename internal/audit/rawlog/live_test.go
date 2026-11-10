// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package rawlog

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }

// Flush makes everything written so far decodable from the bytes on
// disk while the segment stays open (the live-view hand-over point).
func TestSegmentFlushMakesRecordsReadable(t *testing.T) {
	var buf bytes.Buffer
	w := NewSegmentWriter(SegmentConfig{Create: func(uint32) (io.WriteCloser, error) { return nopWC{&buf}, nil }})
	t0 := time.Unix(1000, 0)
	for i, s := range []string{"hello ", "world"} {
		if err := w.Write(Record{TS: t0.Add(time.Duration(i) * time.Second), Channel: 1, Kind: KindDataOut, Payload: []byte(s)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSegment(bytes.NewReader(append([]byte(nil), buf.Bytes()...)))
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for {
		rec, err := r.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("unexpected error after %q: %v", got, err)
			}
			break
		}
		got += string(rec.Payload)
	}
	if got != "hello world" {
		t.Fatalf("read %q after Flush", got)
	}
	if err := w.Flush(); err != nil { // flushing an idle or closed writer is fine
		t.Fatal(err)
	}
	_ = w.Close()
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestTermSize(t *testing.T) {
	req := func(typ string, payload []byte) Record {
		return Record{Kind: KindRequest, Payload: Encode(Request{Type: typ, OK: true, Payload: payload})}
	}
	if c, r, ok := TermSize(req("pty-req", ptyPayload("xterm", 120, 40))); !ok || c != 120 || r != 40 {
		t.Fatalf("pty-req: %d %d %v", c, r, ok)
	}
	if c, r, ok := TermSize(req("window-change", ssh.Marshal(struct{ C, R, W, H uint32 }{100, 30, 0, 0}))); !ok || c != 100 || r != 30 {
		t.Fatalf("window-change: %d %d %v", c, r, ok)
	}
	if _, _, ok := TermSize(req("shell", nil)); ok {
		t.Fatal("shell is not a size")
	}
	if _, _, ok := TermSize(Record{Kind: KindDataOut, Payload: []byte("x")}); ok {
		t.Fatal("data is not a size")
	}
}
