// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"crypto/rand"
	"syscall"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
)

// benchChannel opens one recorded session channel and runs reqs on it.
func benchChannel(b *testing.B, reqs func(ch ChannelHooks)) (ChannelHooks, *Recorder) {
	b.Helper()
	r, err := NewRecorder(Config{SpoolDir: b.TempDir(), SpoolMaxBytes: 8 << 30, Objects: &memObjects{}, Events: &memEvents{}, GatewayPod: "gw-0"})
	if err != nil {
		b.Fatal(err)
	}
	s := r.Start(SessionMeta{ID: NewSessionID(time.Now()), User: "alice", DevPod: "alice-dev", DevPodUID: "uid-1", Owner: "alice", OpenedAt: time.Now()}, policy.Effective{})
	ch, err := s.OpenChannel(ClientToBackend, "session", nil)
	if err != nil {
		b.Fatal(err)
	}
	reqs(ch)
	return ch, r
}

// benchFeed also reports process CPU per op: the zstd encoder may work
// on other goroutines, which ns/op alone would hide.
func benchFeed(b *testing.B, ch ChannelHooks, chunk []byte) {
	b.SetBytes(int64(len(chunk)))
	c0 := cpuTime()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch.Data(BackendToClient, false, chunk)
	}
	b.StopTimer()
	b.ReportMetric(float64(cpuTime()-c0)/float64(b.N), "cpu-ns/op")
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkChannel_TTYOutput: a pty shell printing text (`yes`) — raw
// recording plus the ZMODEM scanner.
func BenchmarkChannel_TTYOutput(b *testing.B) {
	ch, _ := benchChannel(b, func(ch ChannelHooks) {
		_ = ch.Request(ClientToBackend, "pty-req", true, ptyReq("xterm"))
		_ = ch.Request(ClientToBackend, "shell", true, nil)
	})
	// A pty hands the gateway at most a few KiB per read.
	benchFeed(b, ch, bytes.Repeat([]byte("y\r\n"), 4<<10/3))
}

// BenchmarkChannel_ExecRandom: incompressible exec output (raw recording only).
func BenchmarkChannel_ExecRandom(b *testing.B) {
	ch, _ := benchChannel(b, func(ch ChannelHooks) {
		_ = ch.Request(ClientToBackend, "exec", true, str("cat blob"))
	})
	chunk := make([]byte, 32<<10)
	_, _ = rand.Read(chunk)
	benchFeed(b, ch, chunk)
}

func ptyReq(term string) []byte {
	return append(append(str(term), u32(80)...), append(append(u32(24), u32(0)...), append(u32(0), str("")...)...)...)
}
