// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Server serves CLI connections: SSH connections that logged in as the
// CLI user instead of into a DevPod.
type Server struct {
	API *API
	// MaxConns caps concurrent CLI connections on this gateway (default 64).
	MaxConns int
	// IdleTimeout closes a connection with no command running (default 5 min).
	IdleTimeout time.Duration
	// CommandTimeout bounds one command (default 60 s). Following a
	// snapshot runs on its own clock (FollowBudget, FollowIdle).
	CommandTimeout time.Duration
	// FollowBudget / FollowIdle: how long a snapshot is followed at most
	// (default 60 min) and how long without any event (default 60 s).
	FollowBudget, FollowIdle time.Duration
	Log                      *slog.Logger

	once sync.Once
	sem  chan struct{}
}

// Meta describes the caller for the command log.
type Meta struct {
	User     string
	Remote   string
	AuthKind string // direct | trusted_proxy
	Alias    string
}

const maxChannels = 8 // sessions open at once on one connection

func (s *Server) init() {
	s.once.Do(func() {
		if s.MaxConns <= 0 {
			s.MaxConns = 64
		}
		if s.IdleTimeout <= 0 {
			s.IdleTimeout = 5 * time.Minute
		}
		if s.CommandTimeout <= 0 {
			s.CommandTimeout = 60 * time.Second
		}
		if s.Log == nil {
			s.Log = slog.Default()
		}
		s.sem = make(chan struct{}, s.MaxConns)
	})
}

// Serve runs one authenticated CLI connection until it ends. Only
// session channels are served: no forwarding, no subsystems.
func (s *Server) Serve(ctx context.Context, conn ssh.Conn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, m Meta) {
	s.init()
	go ssh.DiscardRequests(reqs) // tcpip-forward and friends are refused
	busy := false
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		busy = true
	}

	var running atomic.Int32
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // idle watchdog
		t := time.NewTicker(s.IdleTimeout / 4)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-t.C:
				if running.Load() == 0 && time.Since(time.Unix(0, last.Load())) > s.IdleTimeout {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	slots := make(chan struct{}, maxChannels)
	var wg sync.WaitGroup
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.Prohibited, "the DevPod CLI only runs commands (no forwarding)")
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = nc.Reject(ssh.ResourceShortage, "too many sessions on this connection")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			<-slots
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.session(ctx, ch, chReqs, m, busy, &running, &last)
		}()
	}
	wg.Wait()
}

// crlf turns \n into \r\n for a client that asked for a terminal.
type crlf struct{ w io.Writer }

func (c crlf) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func exit(ch ssh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
	_ = ch.Close()
}

func (s *Server) session(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, m Meta, busy bool, running *atomic.Int32, last *atomic.Int64) {
	defer ch.Close()
	pty := false
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			pty = true
			_ = req.Reply(true, nil)
		case "env", "window-change":
			_ = req.Reply(true, nil) // accepted and ignored
		case "exec", "shell":
			_ = req.Reply(true, nil)
			in := newInput(ch, pty)
			go func() { // later requests: a signal interrupts; the channel closing hangs up
				for r := range reqs {
					if r.Type == "signal" {
						in.fire()
					}
					if r.WantReply {
						_ = r.Reply(false, nil)
					}
				}
				in.hangup()
			}()
			if busy {
				fmt.Fprint(ch.Stderr(), "error: too many DevPod CLI sessions on this gateway; try again in a moment\r\n")
				exit(ch, ExitAPI)
				return
			}
			var out, errw io.Writer = ch, ch.Stderr()
			if pty {
				out, errw = crlf{ch}, crlf{ch.Stderr()}
			}
			if req.Type == "exec" {
				var p struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &p)
				exit(ch, s.run(ctx, m, p.Command, out, errw, in, running, last))
				return
			}
			if !pty { // `ssh devpod@gateway` without a terminal
				Help(out, "")
				exit(ch, ExitOK)
				return
			}
			exit(ch, s.interactive(ctx, ch, in, m, running, last))
			return
		default: // subsystem (sftp), x11-req, auth-agent-req@openssh.com, …
			_ = req.Reply(false, nil)
		}
	}
}

// run executes one command line and logs it.
func (s *Server) run(ctx context.Context, m Meta, line string, out, errw io.Writer, in *input, running *atomic.Int32, last *atomic.Int64) int {
	running.Add(1)
	start := time.Now()
	defer func() {
		running.Add(-1)
		last.Store(time.Now().UnixNano())
	}()
	argv, err := Split(line)
	code := ExitUsage
	if err != nil {
		fmt.Fprintf(errw, "error: %s\n", err)
	} else {
		cctx, cancel := context.WithTimeout(ctx, s.CommandTimeout)
		env := &Env{Ctx: cctx, User: m.User, API: s.API, Out: out, Err: errw, Session: ctx, Interrupt: in.begin(), TTY: in.tty,
			StdinLine: func() (string, error) { return in.line(nil) }, FollowBudget: s.FollowBudget, FollowIdle: s.FollowIdle}
		if in.tty {
			env.ReadLine = func(prompt string, echo bool) (string, error) {
				_, _ = io.WriteString(out, prompt)
				var echoTo io.Writer
				if echo {
					echoTo = out
				}
				l, err := in.line(echoTo)
				_, _ = io.WriteString(out, "\n")
				return l, err
			}
		}
		code = Run(env, argv)
		in.end()
		cancel()
	}
	name := ""
	if len(argv) > 0 {
		name = argv[0]
	}
	s.Log.Info("cli_command", "user", m.User, "command", name, "args", strings.Join(redactArgs(argv), " "), "exit", code,
		"duration_ms", time.Since(start).Milliseconds(), "remote", m.Remote, "auth_path", m.AuthKind, "proxy_alias", m.Alias)
	return code
}

// interactive is the prompt a terminal gets from a bare `ssh devpod@gateway`.
func (s *Server) interactive(ctx context.Context, ch ssh.Channel, in *input, m Meta, running *atomic.Int32, last *atomic.Int64) int {
	t := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{in, ch}, "devpod> ")
	fmt.Fprintf(t, "DevPod CLI — signed in as %s. Type help for commands, exit to leave.\n", m.User)
	code := ExitOK
	for {
		line, err := t.ReadLine()
		if err != nil { // EOF (^D), or the connection went away
			return code
		}
		last.Store(time.Now().UnixNano())
		switch strings.TrimSpace(line) {
		case "":
			continue
		case "exit", "quit", "logout":
			return code
		}
		code = s.run(ctx, m, line, t, t, in, running, last)
	}
}

// secretFlag matches flags whose value must not reach the command log.
// No command takes a password as an argument, but people try.
var secretFlag = regexp.MustCompile(`(?i)^-+[a-z-]*(pass|secret|token|credential)[a-z-]*`)

// redactArgs hides the value of any password-like flag (--password x,
// --password=x) before a command line is logged.
func redactArgs(argv []string) []string {
	out := make([]string, len(argv))
	hide := false
	for i, w := range argv {
		switch {
		case hide:
			out[i], hide = "<redacted>", false
		case secretFlag.MatchString(w):
			if name, _, inline := strings.Cut(w, "="); inline {
				out[i] = name + "=<redacted>"
			} else {
				out[i], hide = w, !strings.HasSuffix(strings.ToLower(w), "-stdin")
			}
		default:
			out[i] = w
		}
	}
	return out
}

var errInterrupted = errors.New("interrupted")

const maxInput = 64 << 10 // unread input kept per session

// input is a session's standard input and its interrupts. One goroutine
// reads the channel for the whole session, so Ctrl-C is seen while a
// command runs: on a terminal, a ^C byte arriving during a command
// interrupts it instead of being queued. An SSH "signal" request
// interrupts too, and the channel closing (hangup) interrupts every
// command from then on. End of input alone is not an interrupt
// (`ssh -n`, an agent without stdin).
type input struct {
	tty bool

	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	err     error         // input ended
	running bool          // a command is running
	intr    chan struct{} // the running command's interrupt
	fired   bool
	gone    bool
}

func newInput(src io.Reader, tty bool) *input {
	in := &input{tty: tty}
	in.cond = sync.NewCond(&in.mu)
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := src.Read(tmp)
			in.mu.Lock()
			for _, b := range tmp[:n] {
				if in.tty && in.running && b == 3 {
					in.fireLocked()
					continue
				}
				if len(in.buf) < maxInput {
					in.buf = append(in.buf, b)
				}
			}
			if err != nil {
				in.err = err
			}
			in.cond.Broadcast()
			in.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return in
}

func (in *input) fireLocked() {
	if in.running && !in.fired {
		in.fired = true
		close(in.intr)
		in.cond.Broadcast()
	}
}

// fire interrupts the running command, if any.
func (in *input) fire() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.fireLocked()
}

// hangup: the channel is gone; this and every later command is interrupted.
func (in *input) hangup() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.gone = true
	in.fireLocked()
}

// begin marks a command as running and returns its interrupt channel.
func (in *input) begin() <-chan struct{} {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.running, in.fired, in.intr = true, false, make(chan struct{})
	if in.gone {
		in.fireLocked()
	}
	return in.intr
}

func (in *input) end() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.running = false
}

// Read hands queued input to the prompt's line editor.
func (in *input) Read(p []byte) (int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for len(in.buf) == 0 {
		if in.err != nil {
			return 0, in.err
		}
		in.cond.Wait()
	}
	n := copy(p, in.buf)
	in.buf = in.buf[n:]
	return n, nil
}

// line reads one line for a running command (a prompt's answer, or the
// first line of standard input), echoing what is typed to echo when it
// is not nil. An interrupt aborts it.
func (in *input) line(echo io.Writer) (string, error) {
	var out []byte
	in.mu.Lock()
	defer in.mu.Unlock()
	for {
		for len(in.buf) == 0 {
			if in.fired {
				return "", errInterrupted
			}
			if in.err != nil {
				if len(out) > 0 {
					return string(out), nil
				}
				return "", in.err
			}
			in.cond.Wait()
		}
		b := in.buf[0]
		in.buf = in.buf[1:]
		switch b {
		case '\r', '\n':
			if b == '\r' && len(in.buf) > 0 && in.buf[0] == '\n' {
				in.buf = in.buf[1:]
			}
			return string(out), nil
		case 0x7f, 0x08: // backspace
			if len(out) > 0 {
				out = out[:len(out)-1]
				if echo != nil {
					_, _ = io.WriteString(echo, "\b \b")
				}
			}
		case 4: // ^D on an empty line: no answer
			if len(out) == 0 {
				return "", io.EOF
			}
		default:
			if b < 0x20 {
				continue
			}
			out = append(out, b)
			if echo != nil {
				_, _ = echo.Write([]byte{b})
			}
		}
	}
}
