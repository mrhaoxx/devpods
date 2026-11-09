// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package rawlog

import (
	"bytes"
	"io"
	"testing"
	"time"
)

type memSegs struct {
	bufs   map[uint32]*bytes.Buffer
	sealed []uint32
	raw    map[uint32]int64
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

func newMemSegs() *memSegs {
	return &memSegs{bufs: map[uint32]*bytes.Buffer{}, raw: map[uint32]int64{}}
}

func (m *memSegs) cfg(maxBytes int64, maxAge time.Duration, now func() time.Time) SegmentConfig {
	return SegmentConfig{
		MaxBytes: maxBytes,
		MaxAge:   maxAge,
		Now:      now,
		Create: func(seq uint32) (io.WriteCloser, error) {
			b := &bytes.Buffer{}
			m.bufs[seq] = b
			return nopCloser{b}, nil
		},
		OnSeal: func(seq uint32, first, last time.Time, rawBytes int64) error {
			m.sealed = append(m.sealed, seq)
			m.raw[seq] = rawBytes
			return nil
		},
	}
}

func bigRecord(n int, ch uint32) Record {
	return Record{TS: time.Unix(1, 0), Channel: ch, Kind: KindDataOut, Payload: bytes.Repeat([]byte("x"), n)}
}

func TestSegment_RotatesAtMaxBytes(t *testing.T) {
	m := newMemSegs()
	w := NewSegmentWriter(m.cfg(8<<20, time.Hour, time.Now))
	for i := 0; i < 3; i++ {
		if err := w.Write(bigRecord(5<<20, uint32(i))); err != nil {
			t.Fatal(err)
		}
		if i == 0 && len(m.sealed) != 0 {
			t.Fatalf("sealed too early: %v", m.sealed)
		}
		if i == 1 && (len(m.sealed) != 1 || m.sealed[0] != 1) {
			t.Fatalf("after 10MiB want seq 1 sealed, got %v", m.sealed)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if len(m.sealed) != 2 || m.sealed[1] != 2 {
		t.Fatalf("want [1 2], got %v", m.sealed)
	}
	if m.raw[1] < 10<<20 {
		t.Fatalf("seq 1 raw bytes = %d", m.raw[1])
	}
}

func TestSegment_RotatesOnAgeViaTick(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newMemSegs()
	w := NewSegmentWriter(m.cfg(8<<20, 60*time.Second, func() time.Time { return now }))
	if err := w.Tick(); err != nil || len(m.sealed) != 0 {
		t.Fatalf("empty writer must not seal: %v %v", err, m.sealed)
	}
	if err := w.Write(bigRecord(10, 1)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Second)
	_ = w.Tick()
	if len(m.sealed) != 0 {
		t.Fatal("sealed before MaxAge")
	}
	now = now.Add(2 * time.Second)
	if err := w.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(m.sealed) != 1 {
		t.Fatalf("want seal after 61s, got %v", m.sealed)
	}
}

func TestSegment_EachDecodesIndependently(t *testing.T) {
	m := newMemSegs()
	w := NewSegmentWriter(m.cfg(8<<20, time.Hour, time.Now))
	for i := 0; i < 3; i++ {
		_ = w.Write(bigRecord(5<<20, uint32(i)))
	}
	_ = w.Close()
	rd, err := OpenSegment(bytes.NewReader(m.bufs[2].Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	r, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if r.Channel != 2 {
		t.Fatalf("segment 2 starts with channel %d, want 2", r.Channel)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if _, err := OpenSegment(bytes.NewReader([]byte("not zstd"))); err == nil {
		t.Fatal("garbage must not open")
	}
}

func TestConcat_PreservesOrder(t *testing.T) {
	m := newMemSegs()
	w := NewSegmentWriter(m.cfg(1, time.Hour, time.Now)) // seal after every record
	for i := 0; i < 4; i++ {
		_ = w.Write(Record{TS: time.Unix(int64(i), 0), Channel: uint32(i), Kind: KindDataIn, Payload: []byte{byte(i)}})
	}
	_ = w.Close()
	closed := 0
	it := Concat(len(m.sealed), func(i int) (io.ReadCloser, error) {
		return closeCounter{bytes.NewReader(m.bufs[m.sealed[i]].Bytes()), &closed}, nil
	})
	for i := 0; i < 4; i++ {
		r, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		if r.Channel != uint32(i) {
			t.Fatalf("record %d has channel %d", i, r.Channel)
		}
	}
	if _, err := it.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if closed != 4 {
		t.Fatalf("closed %d segments, want 4", closed)
	}
}

type closeCounter struct {
	io.Reader
	n *int
}

func (c closeCounter) Close() error { *c.n++; return nil }
