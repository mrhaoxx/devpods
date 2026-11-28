// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The stdin gate opens on the attacher's first byte, and only then; with
// nobody there it gives up after its wait.
func TestGateStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	start := time.Now()
	if gateStdin(r, 150*time.Millisecond) {
		t.Fatal("opened with nobody there")
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("gave up before its wait")
	}
	r2, w2, _ := os.Pipe()
	defer r2.Close()
	go func() { _, _ = w2.Write([]byte{0, 'x'}) }()
	if !gateStdin(r2, 5*time.Second) {
		t.Fatal("did not open on the sentinel")
	}
	rest := make([]byte, 1)
	if n, _ := r2.Read(rest); n != 1 || rest[0] != 'x' {
		t.Fatalf("the sentinel took more than itself: %q", rest[:n])
	}
	w2.Close()
}

// The terminal gate opens once the terminal has a size (the attacher's
// first resize).
func TestGateTTY(t *testing.T) {
	m, s, err := openPTY()
	if err != nil {
		t.Skip("no pseudo-terminals here:", err)
	}
	defer m.Close()
	defer s.Close()
	if gateTTY(s, 150*time.Millisecond) {
		t.Fatal("opened at size 0×0")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = setWinsize(m, &unix.Winsize{Row: 30, Col: 100})
	}()
	if !gateTTY(s, 5*time.Second) {
		t.Fatal("did not open at a size")
	}
}

// A command on a terminal of its own, relayed to the container's: it sees
// a terminal of the container's size and what it writes comes out, all of
// it, before the relay ends.
func TestPTYRelay(t *testing.T) {
	outerM, outerS, err := openPTY() // the container's terminal: the attacher at its master
	if err != nil {
		t.Skip("no pseudo-terminals here:", err)
	}
	defer outerM.Close()
	defer outerS.Close()
	if err := setWinsize(outerM, &unix.Winsize{Row: 30, Col: 100}); err != nil {
		t.Fatal(err)
	}
	m, s, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "stty size; test -t 0 && echo on-a-tty; for i in 1 2 3; do echo line $i; done")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	stop := ptyRelay(outerS, outerS, outerS, m)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	stop()
	var got bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&got, outerM); close(done) }()
	time.Sleep(300 * time.Millisecond)
	_ = outerS.Close()
	_ = outerM.SetReadDeadline(time.Now())
	<-done
	out := strings.ReplaceAll(got.String(), "\r", "")
	for _, want := range []string{"30 100", "on-a-tty", "line 1", "line 3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("relayed %q, missing %q", out, want)
		}
	}
}
