// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

// echoBackend runs a tiny sshd that accepts the given client pubkey,
// expects a single "session" channel, and echoes everything the client
// writes back.
func echoBackend(t *testing.T, allowedPub ssh.PublicKey) (string, func()) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(allowedPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			tc, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					ch, in, err := newCh.Accept()
					if err != nil {
						return
					}
					go func() {
						for r := range in {
							_ = r.Reply(true, nil)
						}
					}()
					_, _ = io.Copy(ch, ch)
					_ = ch.Close()
				}
			}(tc)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestProxy_EchoEndToEnd(t *testing.T) {
	_, gwPriv, _ := ed25519.GenerateKey(rand.Reader)
	gwSigner, _ := ssh.NewSignerFromKey(gwPriv)

	backendAddr, stopBackend := echoBackend(t, gwSigner.PublicKey())
	defer stopBackend()

	_, frontHostPriv, _ := ed25519.GenerateKey(rand.Reader)
	frontHostSigner, _ := ssh.NewSignerFromKey(frontHostPriv)

	frontCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	frontCfg.AddHostKey(frontHostSigner)

	frontLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer frontLn.Close()

	dialer := gateway.NewDialer(gwSigner)

	statsCh := make(chan *gateway.ProxyStats, 1)

	go func() {
		for {
			tc, err := frontLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				srv, srvChans, srvReqs, err := ssh.NewServerConn(conn, frontCfg)
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cli, cliChans, cliReqs, err := dialer.Dial(ctx, backendAddr)
				if err != nil {
					srv.Close()
					return
				}
				stats := &gateway.ProxyStats{}
				_ = gateway.Proxy(srv, srvChans, srvReqs, cli, cliChans, cliReqs, stats)
				select {
				case statsCh <- stats:
				default:
				}
			}(tc)
		}
	}()

	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)

	tc, err := net.Dial("tcp", frontLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(tc, frontLn.Addr().String(), &ssh.ClientConfig{
		User:            "alice+hello",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cli := ssh.NewClient(conn, chans, reqs)
	defer cli.Close()

	session, err := cli.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	if err := session.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}

	_, _ = stdin.Write([]byte("hello\n"))
	_ = stdin.Close()

	buf := make([]byte, 6)
	if _, err := io.ReadFull(stdout, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "hello\n" {
		t.Errorf("got %q, want %q", string(buf), "hello\n")
	}

	// Close the client to force Proxy() to return so we can read stats.
	_ = cli.Close()

	select {
	case stats := <-statsCh:
		// "hello\n" was sent client→backend (6 bytes) and echoed back
		// (6 bytes). The byte counters track channel payload bytes, not
		// on-wire bytes, so we should see exactly 6/6.
		if stats.BytesClientToBackend.Load() < 6 {
			t.Errorf("BytesClientToBackend = %d, want >= 6", stats.BytesClientToBackend.Load())
		}
		if stats.BytesBackendToClient.Load() < 6 {
			t.Errorf("BytesBackendToClient = %d, want >= 6", stats.BytesBackendToClient.Load())
		}
	case <-time.After(5 * time.Second):
		t.Errorf("timeout waiting for proxy stats")
	}
}
