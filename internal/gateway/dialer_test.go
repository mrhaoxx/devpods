// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

// fakeBackend starts an in-process ssh server that accepts only the given
// gateway pubkey. Returns the listener address and a cancel func.
func fakeBackend(t *testing.T, allowedPub ssh.PublicKey) (string, func()) {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

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
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _, _, _ = ssh.NewServerConn(c, cfg)
				// no-op: we only care about the handshake succeeding
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestDialer_HappyPath(t *testing.T) {
	_, gwPriv, _ := ed25519.GenerateKey(rand.Reader)
	gwSigner, _ := ssh.NewSignerFromKey(gwPriv)

	addr, stop := fakeBackend(t, gwSigner.PublicKey())
	defer stop()

	d := gateway.NewDialer(gwSigner)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, chans, reqs, err := d.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	go func() {
		for ch := range chans {
			_ = ch.Reject(ssh.Prohibited, "test")
		}
	}()
}

func TestDialer_TimeoutOnNoListener(t *testing.T) {
	_, gwPriv, _ := ed25519.GenerateKey(rand.Reader)
	gwSigner, _ := ssh.NewSignerFromKey(gwPriv)

	d := gateway.NewDialer(gwSigner)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// 127.0.0.1 with an unused port: should fail fast (TCP RST).
	_, _, _, err := d.Dial(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error dialing dead port")
	}
}
