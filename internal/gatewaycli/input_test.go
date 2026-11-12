// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// A config piped in before the command reads it (`ssh gw config save x
// < big.yaml`) is kept whole up to the largest config, so the size
// check sees all of it instead of a silently cut copy.
func TestInputKeepsAWholeConfig(t *testing.T) {
	want := bytes.Repeat([]byte("a: b\n"), (maxConfigContent+5<<10)/5)
	in := newInput(bytes.NewReader(want), false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		in.mu.Lock()
		ended := in.err != nil
		in.mu.Unlock()
		if ended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("input never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := io.ReadAll(in)
	if len(got) != len(want) {
		t.Fatalf("kept %d of %d bytes", len(got), len(want))
	}
}
