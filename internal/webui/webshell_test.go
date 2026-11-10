// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// fakeSSHGateway stands in for the gateway: it accepts only the proxy
// key, records the login and terminal sizes, and runs an echo "shell"
// that exits with status 3 on "exit".
type fakeSSHGateway struct {
	addr string
	mu   sync.Mutex
	// logins seen, the pty size requested and window changes received
	logins  []string
	pty     [2]uint32
	resized [][2]uint32
}

func (g *fakeSSHGateway) snapshot() ([]string, [2]uint32, [][2]uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.logins...), g.pty, append([][2]uint32(nil), g.resized...)
}

func startFakeSSHGateway(t *testing.T, proxy ssh.PublicKey) *fakeSSHGateway {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), proxy.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("not the proxy key")
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	g := &fakeSSHGateway{addr: ln.Addr().String()}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go g.serve(nc, cfg)
		}
	}()
	return g
}

func (g *fakeSSHGateway) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	g.mu.Lock()
	g.logins = append(g.logins, sc.User())
	g.mu.Unlock()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range creqs {
				switch req.Type {
				case "pty-req":
					var p struct {
						Term             string
						Cols, Rows, W, H uint32
						Modes            string
					}
					_ = ssh.Unmarshal(req.Payload, &p)
					g.mu.Lock()
					g.pty = [2]uint32{p.Cols, p.Rows}
					g.mu.Unlock()
				case "window-change":
					var p struct{ Cols, Rows, W, H uint32 }
					_ = ssh.Unmarshal(req.Payload, &p)
					g.mu.Lock()
					g.resized = append(g.resized, [2]uint32{p.Cols, p.Rows})
					g.mu.Unlock()
				}
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			}
		}()
		go func() {
			defer ch.Close()
			buf := make([]byte, 1024)
			for {
				n, err := ch.Read(buf)
				if err != nil {
					return
				}
				if strings.TrimSpace(string(buf[:n])) == "exit" {
					_, _ = ch.Write([]byte("bye"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{3}))
					return
				}
				_, _ = ch.Write(append([]byte("echo:"), buf[:n]...))
			}
		}()
	}
}

func TestWebshell(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	gw := startFakeSSHGateway(t, signer.PublicKey())
	shell, err := webui.NewWebshell(pem.EncodeToMemory(block), gw.addr, "", 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(shell.PublicKey(), "ssh-ed25519 ") {
		t.Fatalf("public key %q", shell.PublicKey())
	}
	s.Webshell = shell

	alice, bob := forge(sm, "ws-alice", false), forge(sm, "ws-bob", false)
	createFor(t, s, alice, "ws-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	key := types.NamespacedName{Name: "ws-alice-dev1", Namespace: "devpods"}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/devpods/ws-alice-dev1/shell?cols=100&rows=30"
	dial := func(c *http.Cookie, origin string) (*websocket.Conn, *http.Response, error) {
		h := http.Header{"Cookie": {c.String()}}
		if origin != "" {
			h.Set("Origin", origin)
		}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return websocket.Dial(dctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
	}
	status := func(c *http.Cookie, origin string) int {
		t.Helper()
		conn, resp, err := dial(c, origin)
		if err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return http.StatusSwitchingProtocols
		}
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}
		return resp.StatusCode
	}

	if got := status(alice, ""); got != http.StatusConflict { // not running yet
		t.Fatalf("stopped DevPod: %d", got)
	}
	var dp devpodv1alpha1.DevPod
	if err := s.Reader.Get(ctx, key, &dp); err != nil {
		t.Fatal(err)
	}
	dp.Spec.Collaborators = []string{"ws-bob"}
	if err := k8sClient.Update(ctx, &dp); err != nil {
		t.Fatal(err)
	}
	dp.Status.Phase = devpodv1alpha1.DevPodRunning
	if err := k8sClient.Status().Update(ctx, &dp); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for status(bob, "") != http.StatusSwitchingProtocols { // the cache catches up
		if time.Now().After(deadline) {
			t.Fatal("DevPod never became usable")
		}
		time.Sleep(100 * time.Millisecond)
	}

	read := func(c *websocket.Conn) string {
		t.Helper()
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		typ, data, err := c.Read(rctx)
		if err != nil || typ != websocket.MessageBinary {
			t.Fatalf("read: %v %v", typ, err)
		}
		return string(data)
	}

	t.Run("owner gets a shell through the gateway as themselves", func(t *testing.T) {
		c, _, err := dial(alice, "")
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		if err := c.Write(ctx, websocket.MessageBinary, []byte("hi")); err != nil {
			t.Fatal(err)
		}
		if got := read(c); got != "echo:hi" {
			t.Fatalf("got %q", got)
		}
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			logins, pty, resized := gw.snapshot()
			if len(resized) > 0 {
				if logins[len(logins)-1] != "ws-alice+dev1" || pty != [2]uint32{100, 30} || resized[0] != [2]uint32{120, 40} {
					t.Fatalf("login %v pty %v resized %v", logins, pty, resized)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the resize never reached the gateway")
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = c.Write(ctx, websocket.MessageBinary, []byte("exit\n"))
		if got := read(c); got != "bye" {
			t.Fatalf("last output %q", got)
		}
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, _, err = c.Read(rctx)
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusNormalClosure || !strings.Contains(ce.Reason, "status 3") {
			t.Fatalf("close: %v", err)
		}
	})
	t.Run("a collaborator names the DevPod in full", func(t *testing.T) {
		c, _, err := dial(bob, "")
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		_ = c.Write(ctx, websocket.MessageBinary, []byte("x"))
		read(c)
		if logins, _, _ := gw.snapshot(); logins[len(logins)-1] != "ws-bob+ws-alice-dev1" {
			t.Fatalf("logins %v", logins)
		}
	})
	t.Run("probe reports without opening a shell", func(t *testing.T) {
		before, _, _ := gw.snapshot()
		for user, want := range map[string]int{"ws-alice": http.StatusNoContent, "ws-eve": http.StatusNotFound} {
			req, _ := http.NewRequest("GET", srv.URL+"/api/devpods/ws-alice-dev1/shell?probe=1", nil)
			req.AddCookie(forge(sm, user, false))
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != want {
				t.Fatalf("probe as %s: %d", user, resp.StatusCode)
			}
		}
		if after, _, _ := gw.snapshot(); len(after) != len(before) {
			t.Fatalf("probe reached the gateway: %v", after)
		}
	})
	t.Run("who may open one", func(t *testing.T) {
		if got := status(forge(sm, "ws-eve", false), ""); got != http.StatusNotFound {
			t.Fatalf("stranger: %d", got)
		}
		// the gateway lets only the owner and collaborators in, admins included
		if got := status(forge(sm, "ws-root", true), ""); got != http.StatusForbidden {
			t.Fatalf("admin who is no collaborator: %d", got)
		}
		s.Origin = "https://ui.example"
		if got := status(alice, "https://evil.example"); got != http.StatusForbidden {
			t.Fatalf("cross-origin: %d", got)
		}
		if got := status(alice, ""); got != http.StatusForbidden {
			t.Fatalf("no origin: %d", got)
		}
		s.Origin = ""
		s.Webshell = nil
		if got := status(alice, ""); got != http.StatusNotFound {
			t.Fatalf("disabled: %d", got)
		}
		s.Webshell = shell
	})
	t.Run("at most two shells per user", func(t *testing.T) {
		var held []*websocket.Conn
		for i := 0; i < 2; i++ {
			c, _, err := dial(alice, "")
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, c)
		}
		if got := status(alice, ""); got != http.StatusTooManyRequests {
			t.Fatalf("third shell: %d", got)
		}
		if got := status(bob, ""); got != http.StatusSwitchingProtocols {
			t.Fatalf("another user: %d", got)
		}
		for _, c := range held {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
		deadline := time.Now().Add(5 * time.Second)
		for status(alice, "") != http.StatusSwitchingProtocols { // slots come back
			if time.Now().After(deadline) {
				t.Fatal("slots were never released")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}
