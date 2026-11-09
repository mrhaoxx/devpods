// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package rawlog

import (
	"io"
	"math/rand"
	"runtime"
	"testing"
	"time"
)

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

// A gateway holds one open segment per active session (hundreds per
// pod). Each must stay well under 1 MiB of heap once it has seen a
// realistic amount of output, or the pod is OOM-killed under load.
func TestSegment_OpenWriterHeapBounded(t *testing.T) {
	const writers, perWriter = 24, 2 << 20
	rng := rand.New(rand.NewSource(1))
	chunk := make([]byte, 4<<10)
	for i := range chunk {
		chunk[i] = byte(' ' + rng.Intn(95)) // printable, mildly compressible
	}
	cfg := SegmentConfig{MaxBytes: 64 << 20, Create: func(uint32) (io.WriteCloser, error) { return discardCloser{}, nil }}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ws := make([]*SegmentWriter, writers)
	for i := range ws {
		ws[i] = NewSegmentWriter(cfg)
		for n := 0; n < perWriter; n += len(chunk) {
			if err := ws[i].Write(Record{TS: time.Unix(1, 0), Kind: KindDataOut, Payload: chunk}); err != nil {
				t.Fatal(err)
			}
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	per := float64(after.HeapAlloc-before.HeapAlloc) / writers / (1 << 20)
	for _, w := range ws {
		_ = w.Close()
	}
	t.Logf("heap per open segment: %.2f MiB", per)
	if per > 1 {
		t.Fatalf("open segment holds %.2f MiB of heap, want < 1 MiB", per)
	}
}
