// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command audit-fixture records a protocol conversation as an
// uncompressed rawlog stream (channel 1) for decoder tests. It runs CMD
// as the "backend": its own stdin is forwarded to CMD (recorded as
// DataIn, client→backend) and CMD's stdout is forwarded to its own
// stdout (recorded as DataOut, backend→client). Used by
// hack/gen-audit-fixtures.sh; see there for how real sftp / scp / lrzsz
// clients are wired to it.
//
//	audit-fixture -o OUT.raw [-pty TERM] [-request 'subsystem:sftp'|'exec:CMD'|'shell'] -- CMD ARGS...
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
)

type recorder struct {
	mu  sync.Mutex
	out *os.File
	buf []byte
}

func (r *recorder) write(kind rawlog.Kind, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = rawlog.Append(r.buf[:0], rawlog.Record{TS: time.Now(), Channel: 1, Kind: kind, Payload: payload})
	if _, err := r.out.Write(r.buf); err != nil {
		fmt.Fprintln(os.Stderr, "audit-fixture:", err)
		os.Exit(1)
	}
}

func (r *recorder) request(dir uint8, typ string, payload []byte) {
	r.write(rawlog.KindRequest, rawlog.Encode(rawlog.Request{Dir: dir, Type: typ, WantReply: true, OK: true, Payload: payload}))
}

func main() {
	out := flag.String("o", "", "output rawlog file (required)")
	pty := flag.String("pty", "", "record a pty-req with this TERM before the start request")
	request := flag.String("request", "shell", "start request: 'subsystem:NAME', 'exec:CMD' or 'shell'")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: audit-fixture -o OUT.raw [-pty TERM] [-request 'subsystem:sftp'|'exec:CMD'|'shell'] -- CMD ARGS...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit-fixture:", err)
		os.Exit(1)
	}
	defer f.Close()
	rec := &recorder{out: f}

	rec.write(rawlog.KindChannelOpen, rawlog.Encode(rawlog.ChannelOpen{Type: "session"}))
	if *pty != "" {
		rec.request(0, "pty-req", ssh.Marshal(struct {
			Term             string
			Cols, Rows, W, H uint32
			Modes            string
		}{*pty, 80, 24, 0, 0, ""}))
	}
	switch {
	case strings.HasPrefix(*request, "subsystem:"):
		rec.request(0, "subsystem", ssh.Marshal(struct{ S string }{strings.TrimPrefix(*request, "subsystem:")}))
	case strings.HasPrefix(*request, "exec:"):
		rec.request(0, "exec", ssh.Marshal(struct{ S string }{strings.TrimPrefix(*request, "exec:")}))
	default:
		rec.request(0, "shell", nil)
	}

	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit-fixture:", err)
		os.Exit(1)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit-fixture:", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "audit-fixture:", err)
		os.Exit(1)
	}

	var in, outBytes int64
	go func() {
		pump(os.Stdin, stdin, func(p []byte) { in += int64(len(p)); rec.write(rawlog.KindDataIn, p) })
		_ = stdin.Close()
	}()
	done := make(chan struct{})
	go func() {
		pump(stdout, os.Stdout, func(p []byte) { outBytes += int64(len(p)); rec.write(rawlog.KindDataOut, p) })
		close(done)
	}()
	<-done
	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	rec.request(1, "exit-status", ssh.Marshal(struct{ S uint32 }{uint32(code)}))
	rec.write(rawlog.KindChannelClose, rawlog.Encode(rawlog.ChannelClose{BytesIn: in, BytesOut: outBytes}))
	os.Exit(code)
}

func pump(src io.Reader, dst io.Writer, record func([]byte)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			record(chunk)
			if _, werr := dst.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
