// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/audit"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

// backendLog records what reached the backend.
type backendLog struct {
	mu       sync.Mutex
	channels int
	requests []string
}

func (b *backendLog) add(ch bool, req string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch {
		b.channels++
	}
	if req != "" {
		b.requests = append(b.requests, req)
	}
}

func (b *backendLog) snapshot() (int, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.channels, append([]string(nil), b.requests...)
}

// loggingBackend is an echo sshd that records channels and requests.
func loggingBackend(t *testing.T, allowed ssh.PublicKey, log *backendLog) string {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(k.Marshal(), allowed.Marshal()) {
			return &ssh.Permissions{}, nil
		}
		return nil, errors.New("no")
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			tc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(tc, cfg)
				if err != nil {
					return
				}
				go func() {
					for r := range reqs {
						log.add(false, "global:"+r.Type)
						_ = r.Reply(true, nil)
					}
				}()
				for nc := range chans {
					log.add(true, "")
					ch, in, err := nc.Accept()
					if err != nil {
						return
					}
					go func() {
						for r := range in {
							log.add(false, r.Type)
							_ = r.Reply(true, nil)
						}
					}()
					go func() { _, _ = io.Copy(ch, ch); _ = ch.Close() }()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// startProxy wires client → in-process front sshd → Proxy(hooks) → backend.
func startProxy(t *testing.T, hooks audit.SessionHooks) (*ssh.Client, *backendLog, <-chan error) {
	t.Helper()
	_, gwPriv, _ := ed25519.GenerateKey(rand.Reader)
	gwSigner, _ := ssh.NewSignerFromKey(gwPriv)
	log := &backendLog{}
	backendAddr := loggingBackend(t, gwSigner.PublicKey(), log)

	_, frontPriv, _ := ed25519.GenerateKey(rand.Reader)
	frontSigner, _ := ssh.NewSignerFromKey(frontPriv)
	frontCfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return &ssh.Permissions{}, nil
	}}
	frontCfg.AddHostKey(frontSigner)
	frontLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = frontLn.Close() })

	proxyDone := make(chan error, 1)
	go func() {
		tc, err := frontLn.Accept()
		if err != nil {
			return
		}
		defer tc.Close()
		srv, srvChans, srvReqs, err := ssh.NewServerConn(tc, frontCfg)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cli, cliChans, cliReqs, err := gateway.NewDialer(gwSigner).Dial(ctx, backendAddr)
		if err != nil {
			srv.Close()
			return
		}
		proxyDone <- gateway.Proxy(srv, srvChans, srvReqs, cli, cliChans, cliReqs, &gateway.ProxyStats{}, hooks)
	}()

	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	client, err := ssh.Dial("tcp", frontLn.Addr().String(), &ssh.ClientConfig{
		User: "alice+hello", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientSigner)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, log, proxyDone
}

type fakeHooks struct {
	openErr    error
	denyReq    map[string]bool
	denyGlobal map[string]bool
	filter     func(audit.Dir, []byte) audit.Result
	done       chan struct{}
}

func (h *fakeHooks) OpenChannel(audit.Dir, string, []byte) (audit.ChannelHooks, error) {
	if h.openErr != nil {
		return nil, h.openErr
	}
	return &fakeChan{h: h}, nil
}

func (h *fakeHooks) GlobalRequest(_ audit.Dir, typ string, _ []byte) error {
	if h.denyGlobal[typ] {
		return &audit.DenyError{Msg: "denied"}
	}
	return nil
}

func (h *fakeHooks) Done() <-chan struct{}        { return h.done }
func (h *fakeHooks) Close(string, int64, int64) {}

type fakeChan struct{ h *fakeHooks }

func (c *fakeChan) Request(_ audit.Dir, typ string, _ bool, _ []byte) error {
	if c.h.denyReq[typ] {
		return &audit.DenyError{Msg: "denied"}
	}
	return nil
}
func (c *fakeChan) RequestResult(audit.Dir, string, bool) {}
func (c *fakeChan) Data(d audit.Dir, _ bool, p []byte) audit.Result {
	if c.h.filter != nil {
		return c.h.filter(d, p)
	}
	return audit.Result{Pass: p}
}
func (c *fakeChan) Close() {}

func TestProxy_RejectsChannelOpen(t *testing.T) {
	client, log, _ := startProxy(t, &fakeHooks{openErr: &audit.DenyError{Msg: "nope"}})
	_, err := client.NewSession()
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.Prohibited || oce.Message != "nope" {
		t.Fatalf("got %v", err)
	}
	if n, _ := log.snapshot(); n != 0 {
		t.Fatalf("backend saw %d channels", n)
	}
}

func TestProxy_RequestDenied(t *testing.T) {
	client, log, _ := startProxy(t, &fakeHooks{denyReq: map[string]bool{"exec": true}})
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("ls"); err == nil {
		t.Fatal("denied exec must fail")
	}
	time.Sleep(100 * time.Millisecond)
	if _, reqs := log.snapshot(); len(reqs) != 0 {
		t.Fatalf("backend received %v", reqs)
	}
}

func TestProxy_DataFilterPassAndReply(t *testing.T) {
	h := &fakeHooks{filter: func(d audit.Dir, p []byte) audit.Result {
		if d == audit.ClientToBackend && bytes.Contains(p, []byte("secret")) {
			return audit.Result{Pass: bytes.ReplaceAll(p, []byte("secret"), nil), Reply: []byte("NO")}
		}
		return audit.Result{Pass: p}
	}}
	client, _, _ := startProxy(t, h)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	_, _ = stdin.Write([]byte("secret\n"))
	got := make([]byte, 3)
	if _, err := io.ReadFull(stdout, got); err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "NO") || !strings.Contains(s, "\n") || strings.Contains(s, "secret") {
		t.Fatalf("client read %q", s)
	}
}

func TestProxy_GlobalRequestDenied(t *testing.T) {
	client, log, _ := startProxy(t, &fakeHooks{denyGlobal: map[string]bool{"blocked@test": true}})
	ok, _, err := client.SendRequest("blocked@test", true, nil)
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	ok, _, _ = client.SendRequest("allowed@test", true, nil)
	if !ok {
		t.Fatal("other global requests must still be forwarded")
	}
	if _, reqs := log.snapshot(); len(reqs) != 1 || reqs[0] != "global:allowed@test" {
		t.Fatalf("backend saw %v", reqs)
	}
}

func TestProxy_DoneClosesConnection(t *testing.T) {
	h := &fakeHooks{done: make(chan struct{})}
	client, _, proxyDone := startProxy(t, h)
	if _, err := client.NewSession(); err != nil {
		t.Fatal(err)
	}
	close(h.done)
	select {
	case <-proxyDone:
	case <-time.After(time.Second):
		t.Fatal("Proxy did not return after Done")
	}
}
