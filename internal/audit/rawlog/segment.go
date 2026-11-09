// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package rawlog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Magic starts every decompressed segment, so any single segment can be
// identified and decoded on its own.
const Magic = "DPRAW1\n"

// SegmentConfig parameterises a SegmentWriter.
type SegmentConfig struct {
	MaxBytes int64         // seal once this many uncompressed bytes were written
	MaxAge   time.Duration // seal on Tick once the segment is this old
	// Create opens the destination of segment seq (1-based).
	Create func(seq uint32) (io.WriteCloser, error)
	// OnSeal is called after segment seq was flushed and closed.
	OnSeal func(seq uint32, first, last time.Time, rawBytes int64) error
	Now    func() time.Time
}

// SegmentWriter writes records into a rolling series of zstd segments.
// It is not safe for concurrent use.
type SegmentWriter struct {
	cfg SegmentConfig

	seq      uint32
	dst      io.WriteCloser
	enc      *zstd.Encoder
	started  time.Time
	first    time.Time
	last     time.Time
	rawBytes int64
	buf      []byte
}

// NewSegmentWriter returns a writer; the first segment is created lazily.
func NewSegmentWriter(cfg SegmentConfig) *SegmentWriter {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SegmentWriter{cfg: cfg}
}

func (w *SegmentWriter) open() error {
	w.seq++
	dst, err := w.cfg.Create(w.seq)
	if err != nil {
		return err
	}
	enc, err := getEncoder(dst)
	if err != nil {
		_ = dst.Close()
		return err
	}
	if _, err := enc.Write([]byte(Magic)); err != nil {
		_ = enc.Close()
		encoders.Put(enc)
		_ = dst.Close()
		return err
	}
	w.dst, w.enc = dst, enc
	w.started = w.cfg.Now()
	w.rawBytes = 0
	w.first, w.last = time.Time{}, time.Time{}
	return nil
}

// Write appends one record, sealing the segment afterwards when it has
// reached MaxBytes.
func (w *SegmentWriter) Write(r Record) error {
	if w.enc == nil {
		if err := w.open(); err != nil {
			return err
		}
	}
	w.buf = Append(w.buf[:0], r)
	if _, err := w.enc.Write(w.buf); err != nil {
		return err
	}
	if w.first.IsZero() {
		w.first = r.TS
	}
	w.last = r.TS
	w.rawBytes += int64(len(w.buf))
	if w.cfg.MaxBytes > 0 && w.rawBytes >= w.cfg.MaxBytes {
		return w.seal()
	}
	return nil
}

// Tick seals a non-empty segment that has reached MaxAge.
func (w *SegmentWriter) Tick() error {
	if w.enc == nil || w.rawBytes == 0 {
		return nil
	}
	if w.cfg.MaxAge > 0 && w.cfg.Now().Sub(w.started) >= w.cfg.MaxAge {
		return w.seal()
	}
	return nil
}

// Close seals the current segment, if any.
func (w *SegmentWriter) Close() error {
	if w.enc == nil {
		return nil
	}
	return w.seal()
}

func (w *SegmentWriter) seal() error {
	encErr := w.enc.Close()
	dstErr := w.dst.Close()
	encoders.Put(w.enc)
	seq, first, last, raw := w.seq, w.first, w.last, w.rawBytes
	w.enc, w.dst = nil, nil
	if err := errors.Join(encErr, dstErr); err != nil {
		return err
	}
	if w.cfg.OnSeal != nil {
		return w.cfg.OnSeal(seq, first, last, raw)
	}
	return nil
}

// encoders recycles zstd encoders across segments and sessions. A
// gateway keeps one open segment per active session, so the encoder is
// sized for hundreds of them: synchronous (no per-encoder goroutine
// fan-out), fastest level and a 256 KiB window (under 1 MiB of heap per
// open segment instead of ~10 MiB with the defaults; terminal output
// still compresses well).
var encoders sync.Pool

// EncoderWindow is the zstd window of raw segments.
const EncoderWindow = 256 << 10

func getEncoder(dst io.Writer) (*zstd.Encoder, error) {
	if enc, ok := encoders.Get().(*zstd.Encoder); ok {
		enc.Reset(dst)
		return enc, nil
	}
	return zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithWindowSize(EncoderWindow), zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true))
}

// OpenSegment decompresses one segment and returns a reader positioned
// after the magic header.
func OpenSegment(r io.Reader) (*Reader, error) {
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(dec.IOReadCloser(), 64<<10)
	head := make([]byte, len(Magic))
	if _, err := io.ReadFull(br, head); err != nil {
		dec.Close()
		return nil, fmt.Errorf("rawlog: read segment header: %w", err)
	}
	if string(head) != Magic {
		dec.Close()
		return nil, errors.New("rawlog: not a segment (bad magic)")
	}
	return NewReader(br), nil
}

// Iterator yields records until io.EOF.
type Iterator interface {
	Next() (Record, error)
}

// Concat iterates the records of n segments in order, opening each with
// open and closing it once exhausted.
func Concat(n int, open func(i int) (io.ReadCloser, error)) Iterator {
	return &concat{n: n, open: open}
}

type concat struct {
	n    int
	i    int
	open func(i int) (io.ReadCloser, error)
	cur  *Reader
	rc   io.ReadCloser
}

func (c *concat) Next() (Record, error) {
	for {
		if c.cur == nil {
			if c.i >= c.n {
				return Record{}, io.EOF
			}
			rc, err := c.open(c.i)
			if err != nil {
				return Record{}, err
			}
			rd, err := OpenSegment(rc)
			if err != nil {
				_ = rc.Close()
				return Record{}, err
			}
			c.rc, c.cur = rc, rd
			c.i++
		}
		r, err := c.cur.Next()
		if err == io.EOF {
			_ = c.rc.Close()
			c.cur, c.rc = nil, nil
			continue
		}
		return r, err
	}
}
