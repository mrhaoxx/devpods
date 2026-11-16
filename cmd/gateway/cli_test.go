// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/gateway"
	"github.com/mrhaoxx/devpod/internal/gwlink"
)

func TestCLIFlags_Build(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)

	// off without an API URL, or with the user name cleared
	for _, f := range []cliFlags{{user: "devpod"}, {user: "", apiURL: "http://webui", keyFile: key}} {
		if d, err := f.build(nil); d != nil || err != nil {
			t.Fatalf("%+v: %v %v", f, d, err)
		}
	}
	// on: it must have a key
	for _, f := range []cliFlags{{user: "devpod", apiURL: "http://webui"}, {user: "devpod", apiURL: "http://webui", keyFile: empty}} {
		if _, err := f.build(nil); err == nil {
			t.Fatalf("%+v built without a key", f)
		}
	}
	d, err := (&cliFlags{user: "dp", apiURL: "http://webui/", keyFile: key, maxConns: 7}).build(nil)
	if err != nil || d.user != "dp" || d.srv.API.BaseURL != "http://webui" || d.srv.API.Key != "s3cret" || d.srv.MaxConns != 7 {
		t.Fatalf("built %+v %v", d, err)
	}
	// a gateway of several clusters hands the CLI to the console: no URL, no key
	h := &hub{}
	if d, err := (&cliFlags{user: "devpod"}).build(h); err != nil || d == nil || d.hub != h || d.srv != nil {
		t.Fatalf("with a hub: %+v %v", d, err)
	}
	if d, err := (&cliFlags{user: ""}).build(h); d != nil || err != nil {
		t.Fatalf("with a hub, the name cleared: %+v %v", d, err)
	}
}

// A gateway of several clusters hands a CLI login to the console: it
// opens the console's CLI endpoint (signed, naming the user), logs in
// there as that user with its internal key, and proxies the session.
func TestCLIHandedToTheConsole(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	internal, _ := ssh.NewSignerFromKey(priv)
	host := func() ssh.Signer {
		_, p, _ := ed25519.GenerateKey(rand.Reader)
		s, _ := ssh.NewSignerFromKey(p)
		return s
	}
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != gwlink.CLIPath || r.Header.Get(gwlink.HeaderSignature) == "" || r.URL.Query().Get("user") != "alice" {
			http.Error(w, "not a gateway's request for alice", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(internal.PublicKey().Marshal()) {
				return nil, ssh.ErrNoAuth
			}
			return &ssh.Permissions{}, nil
		}}
		cfg.AddHostKey(host())
		sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			ch, creqs, err := nc.Accept()
			if err != nil {
				continue
			}
			for req := range creqs {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				_, _ = fmt.Fprintf(ch, "%s ran %s\n", sc.User(), req.Payload[4:])
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = ch.Close()
				break
			}
		}
	}))
	defer console.Close()
	id, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	if err != nil {
		t.Fatal(err)
	}
	old := cli
	t.Cleanup(func() { cli = old })
	cli = &cliDeps{user: "devpod", hub: &hub{link: gwlink.NewClient(gwlink.Options{Console: console.URL, Gateway: "gw", Identity: id})}}

	// the gateway's side: a CLI login, signed in as alice
	gwCfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return &ssh.Permissions{Extensions: map[string]string{cliExtension: "1", "devpod.io/user": "alice", "devpod.io/cli-auth": "direct"}}, nil
	}}
	gwCfg.AddHostKey(host())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sc, chans, reqs, err := ssh.NewServerConn(c, gwCfg)
		if err != nil {
			return
		}
		serveCLI(ctx, sc, chans, reqs, gateway.NewDialer(internal), &auditDeps{})
	}()
	user, _ := ssh.NewSignerFromKey(priv) // any key: the gateway's own check is stubbed above
	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "alice+devpod", Auth: []ssh.AuthMethod{ssh.PublicKeys(user)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("whoami")
	if err != nil || string(out) != "alice ran whoami\n" {
		t.Fatalf("whoami: %v %q", err, out)
	}
}
