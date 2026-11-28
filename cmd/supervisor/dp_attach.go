// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/term"
)

// dp attaches to the ranks of a job it waits for (srun's way, plat101's
// scheme): told so by the console (a frame {"a": …}), it opens the
// console's attach (/dp/v1/attach) for each rank — rank 0's terminal
// alone for --pty — and is the job's terminal, or its stdin and output,
// itself. The ranks wait for it (their supervisor's gate), so nothing
// they write is missed.

// dpIO: what a dp call reads and writes, and whether it may attach.
type dpIO struct {
	in       io.Reader // the caller's stdin: rank 0's
	out, err io.Writer
	tty      bool   // in and out are this process's terminal, fd
	fd       int    // the terminal (0, dp's stdin)
	term     string // its kind (TERM): a --pty job's terminal's
	attach   bool   // the console's attach answered the probe
}

// attachTries, attachRetry: how often a rank not running yet is tried.
var (
	attachTries = 20
	attachRetry = 500 * time.Millisecond
)

// attachTo: what the console tells dp to attach to (gatewaycli.AttachTo).
type attachTo struct {
	Job     string `json:"job"`
	Cluster string `json:"cluster"`
	Nodes   int    `json:"nodes"`
	TTY     bool   `json:"tty"`
}

// attachable: argv submits a job the caller stays with — srun, or a job
// submit without -d or --dry-run.
func attachable(argv []string) bool {
	if len(argv) > 0 && argv[0] == "srun" {
		return true
	}
	if len(argv) < 2 || (argv[0] != "job" && argv[0] != "jobs") || argv[1] != "submit" {
		return false
	}
	for _, a := range argv[2:] {
		switch a {
		case "--":
			return true
		case "-d", "--detach", "--dry-run":
			return false
		}
	}
	return true
}

// probeAttach: the console's attach is reachable from here (an agent of
// before passes no WebSocket on — dp then follows the output instead).
func probeAttach(base, uid, token string) bool {
	req, err := http.NewRequest(http.MethodGet, base+"/dp/v1/attach?probe=1", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-DevPod-UID", uid)
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent
}

// attacher attaches to the ranks the console says, one job at a time.
type attacher struct {
	base, uid, token string
	io               dpIO
	wg               sync.WaitGroup
	// held: what the console says (the job's state) while a terminal is
	// raw, said once it is not — it would land in the middle of the screen
	mu    sync.Mutex
	raw   bool
	held  [][2]string // stream, text
	stdin *stdinRouter
}

// say writes the console's text on stream ("o", "e"), or holds it while
// the terminal is raw.
func (a *attacher) say(stream, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.raw {
		a.held = append(a.held, [2]string{stream, text})
		return
	}
	a.write(stream, text)
}

func (a *attacher) write(stream, text string) {
	if stream == "o" {
		_, _ = io.WriteString(a.io.out, text)
	} else {
		_, _ = io.WriteString(a.io.err, text)
	}
}

// setRaw marks the terminal raw (or not: what was held is said).
func (a *attacher) setRaw(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.raw = on
	if !on {
		for _, h := range a.held {
			a.write(h[0], h[1])
		}
		a.held = nil
	}
}

// start attaches to what to says, in the background.
func (a *attacher) start(to attachTo) {
	if to.TTY {
		a.wg.Add(1)
		go func() { defer a.wg.Done(); a.attachTTY(to) }()
		return
	}
	for r := 0; r < max(to.Nodes, 1); r++ {
		a.wg.Add(1)
		go func() { defer a.wg.Done(); a.attachRank(to, r) }()
	}
}

// wait waits (at most limit) for the attaches to end.
func (a *attacher) wait(limit time.Duration) {
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
	}
	a.setRaw(false)
}

// dial opens the console's attach to rank of to, again while it is not
// running yet.
func (a *attacher) dial(ctx context.Context, to attachTo, rank int) (*websocket.Conn, error) {
	u := "ws" + strings.TrimPrefix(a.base, "http") + "/dp/v1/attach?job=" + url.QueryEscape(to.Job) + "&rank=" + strconv.Itoa(rank)
	hdr := http.Header{"Authorization": {"Bearer " + a.token}, "X-DevPod-UID": {a.uid}}
	var last error
	for try := 0; try < attachTries; try++ {
		c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
		if err == nil {
			c.SetReadLimit(1 << 20)
			return c, nil
		}
		last = err
		if resp != nil {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			last = fmt.Errorf("%s (%d)", strings.TrimSpace(string(msg)), resp.StatusCode)
			if resp.StatusCode != http.StatusConflict {
				break // only "not running yet" is worth another try
			}
		}
		time.Sleep(attachRetry)
	}
	return nil, last
}

// attachRank attaches to rank of to without a terminal: its gate's
// sentinel, the caller's stdin for rank 0 (the end of it at once for the
// others), and what it writes — marked by rank when there are several.
// Not attachable, the rank is followed instead.
func (a *attacher) attachRank(to attachTo, rank int) {
	ctx := context.Background()
	mark := ""
	if to.Nodes > 1 {
		mark = strconv.Itoa(rank) + ": "
	}
	out, errw := newLineMarker(a.io.out, mark), newLineMarker(a.io.err, mark)
	defer out.Flush()
	defer errw.Flush()
	c, err := a.dial(ctx, to, rank)
	if err != nil {
		fmt.Fprintf(a.io.err, "dp: cannot attach to rank %d (%v): following its output instead\n", rank, err)
		a.follow(to, rank, out, errw)
		return
	}
	defer func() { _ = c.CloseNow() }()
	_ = c.Write(ctx, websocket.MessageBinary, []byte{0}) // the gate opens
	if rank == 0 && a.stdin != nil {
		a.stdin.attach(wsSink{ctx, c})
	} else {
		wsSink{ctx, c}.eof()
	}
	readFrames(ctx, c, out, errw)
}

// attachTTY attaches to rank 0's terminal: this terminal raw while it
// lasts, its size followed.
func (a *attacher) attachTTY(to attachTo) {
	ctx := context.Background()
	c, err := a.dial(ctx, to, 0)
	if err != nil {
		fmt.Fprintf(a.io.err, "dp: cannot attach to the job's terminal: %v\n", err)
		return
	}
	defer func() { _ = c.CloseNow() }()
	if fd := a.io.fd; a.io.tty {
		if st, err := term.MakeRaw(fd); err == nil {
			a.setRaw(true)
			defer func() { _ = term.Restore(fd, st); a.setRaw(false) }()
		}
		first := true
		sendSize := func() {
			w, h, err := term.GetSize(fd)
			if err != nil || w <= 0 || h <= 0 {
				if !first {
					return
				}
				w, h = 80, 24 // a terminal of no size: the gate opens all the same
			}
			first = false
			b, _ := json.Marshal(map[string]any{"type": "resize", "cols": w, "rows": h})
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		sendSize() // the gate opens
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				sendSize()
			}
		}()
	} else {
		// no terminal here (a test): a size all the same, for the gate
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":80,"rows":24}`))
	}
	if a.stdin != nil {
		a.stdin.attach(wsSink{ctx, c})
	}
	readFrames(ctx, c, a.io.out, a.io.out)
}

// stdinSink: a rank 0 the caller's stdin goes to.
type stdinSink interface {
	data(p []byte) error
	eof()
}

// wsSink: a rank 0 attached over c.
type wsSink struct {
	ctx context.Context
	c   *websocket.Conn
}

func (w wsSink) data(p []byte) error { return w.c.Write(w.ctx, websocket.MessageBinary, p) }
func (w wsSink) eof()                { _ = w.c.Write(w.ctx, websocket.MessageText, []byte(`{"type":"eof"}`)) }

// stdinRouter reads the caller's stdin, once, for the rank 0 attached now
// — a requeued job's next attempt takes it over — and tells each rank 0
// that comes after its end that it has ended.
type stdinRouter struct {
	in   io.Reader
	once sync.Once
	mu   sync.Mutex
	to   stdinSink
	done bool
}

// attach makes k the one stdin goes to.
func (s *stdinRouter) attach(k stdinSink) {
	s.mu.Lock()
	s.to = k
	done := s.done
	s.mu.Unlock()
	if done {
		k.eof()
		return
	}
	s.once.Do(func() { go s.pump() })
}

func (s *stdinRouter) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.in.Read(buf)
		s.mu.Lock()
		to := s.to
		s.done = err != nil
		s.mu.Unlock()
		if n > 0 {
			_ = to.data(buf[:n]) // one gone meanwhile: lost with it
		}
		if err != nil {
			to.eof()
			return
		}
	}
}

// readFrames writes what the rank writes (1 stdout, 2 stderr) until it ends.
func readFrames(ctx context.Context, c *websocket.Conn, out, errw io.Writer) {
	for {
		_, msg, err := c.Read(ctx)
		if err != nil || len(msg) == 0 {
			if err != nil {
				return
			}
			continue
		}
		switch msg[0] {
		case 1:
			_, _ = out.Write(msg[1:])
		case 2:
			_, _ = errw.Write(msg[1:])
		}
	}
}

// follow follows rank's output through the console (job logs -f), when
// it cannot be attached to.
func (a *attacher) follow(to attachTo, rank int, out, errw io.Writer) {
	ref := to.Job
	if to.Cluster != "" {
		ref = to.Cluster + "/" + to.Job
	}
	dpCall(a.base, a.uid, a.token, []string{"job", "logs", "-f", "--rank", strconv.Itoa(rank), ref}, "/", nil, nil, dpIO{out: out, err: errw})
}

// lineMarker writes whole lines, each with mark in front ("" writes as
// it comes).
type lineMarker struct {
	mu   sync.Mutex
	w    io.Writer
	mark string
	part []byte
}

func newLineMarker(w io.Writer, mark string) *lineMarker { return &lineMarker{w: w, mark: mark} }

func (l *lineMarker) Write(p []byte) (int, error) {
	if l.mark == "" {
		return l.w.Write(p)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.part = append(l.part, p...)
	for {
		i := bytes.IndexByte(l.part, '\n')
		if i < 0 {
			break
		}
		if _, err := fmt.Fprintf(l.w, "%s%s\n", l.mark, l.part[:i]); err != nil {
			return 0, err
		}
		l.part = l.part[i+1:]
	}
	return len(p), nil
}

// Flush writes what is left of a line.
func (l *lineMarker) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.part) > 0 {
		fmt.Fprintf(l.w, "%s%s\n", l.mark, l.part)
		l.part = nil
	}
}
