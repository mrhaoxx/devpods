// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Someone whose key was accepted, on a name two clusters have: a real
// client is told how to write it, on the error stream, and its command
// fails — with a shell or with a command, and nothing else is opened.
func TestRefuseWith_TellsARealClient(t *testing.T) {
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	host, _ := ssh.NewSignerFromKey(hostPriv)
	msg := ambiguousMessage([]string{"alice+x+a", "alice+x+b"})
	for _, how := range []string{"exec", "shell"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			cfg := &ssh.ServerConfig{NoClientAuth: true}
			cfg.AddHostKey(host)
			sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
			if err != nil {
				return
			}
			refuseWith(sc, chans, reqs, msg)
		}()
		client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "alice+x", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		// a forward is not served
		if _, err := client.Dial("tcp", "10.0.0.1:80"); err == nil {
			t.Fatalf("%s: a forwarded connection was opened", how)
		}
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("%s: session: %v", how, err)
		}
		var stderr bytes.Buffer
		sess.Stderr = &stderr
		if how == "exec" {
			err = sess.Run("hostname")
		} else {
			if err = sess.Shell(); err == nil {
				err = sess.Wait()
			}
		}
		var exit *ssh.ExitError
		if !errors.As(err, &exit) || exit.ExitStatus() != 1 {
			t.Fatalf("%s: ended with %v", how, err)
		}
		if out := stderr.String(); !strings.Contains(out, "ssh alice+x+a@") || !strings.Contains(out, "ssh alice+x+b@") {
			t.Fatalf("%s: the client was told %q", how, out)
		}
		_ = client.Close()
		_ = ln.Close()
	}
}
