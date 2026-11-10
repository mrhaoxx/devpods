// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
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
	// CommandTimeout bounds one command (default 60 s).
	CommandTimeout time.Duration
	Log            *slog.Logger

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
			go ssh.DiscardRequests(reqs)
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
				exit(ch, s.run(ctx, m, p.Command, out, errw, running, last))
				return
			}
			if !pty { // `ssh devpod@gateway` without a terminal
				Help(out, "")
				exit(ch, ExitOK)
				return
			}
			exit(ch, s.interactive(ctx, ch, m, running, last))
			return
		default: // subsystem (sftp), x11-req, auth-agent-req@openssh.com, …
			_ = req.Reply(false, nil)
		}
	}
}

// run executes one command line and logs it.
func (s *Server) run(ctx context.Context, m Meta, line string, out, errw io.Writer, running *atomic.Int32, last *atomic.Int64) int {
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
		code = Run(&Env{Ctx: cctx, User: m.User, API: s.API, Out: out, Err: errw}, argv)
		cancel()
	}
	name := ""
	if len(argv) > 0 {
		name = argv[0]
	}
	s.Log.Info("cli_command", "user", m.User, "command", name, "args", strings.Join(argv, " "), "exit", code,
		"duration_ms", time.Since(start).Milliseconds(), "remote", m.Remote, "auth_path", m.AuthKind, "proxy_alias", m.Alias)
	return code
}

// interactive is the prompt a terminal gets from a bare `ssh devpod@gateway`.
func (s *Server) interactive(ctx context.Context, ch ssh.Channel, m Meta, running *atomic.Int32, last *atomic.Int64) int {
	t := term.NewTerminal(ch, "devpod> ")
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
		code = s.run(ctx, m, line, t, t, running, last)
	}
}
