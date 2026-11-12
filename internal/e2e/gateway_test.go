// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// writeKey makes an Ed25519 key, writes it as an OpenSSH private key and
// returns its signer.
func writeKey(t *testing.T, path string) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// fakeSSHD stands in for the sshd of a DevPod: it lets in the gateway's
// internal key and nobody else, and answers any command with who it is.
func fakeSSHD(t *testing.T, internal ssh.PublicKey, name string) string {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	host, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), internal.Marshal()) {
			return nil, errors.New("not the gateway")
		}
		return nil, nil
	}}
	cfg.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "session" {
						_ = nc.Reject(ssh.UnknownChannelType, "sessions only")
						continue
					}
					ch, creqs, err := nc.Accept()
					if err != nil {
						return
					}
					go func() {
						for r := range creqs {
							if r.Type != "exec" {
								if r.WantReply {
									_ = r.Reply(r.Type == "env" || r.Type == "pty-req", nil)
								}
								continue
							}
							_ = r.Reply(true, nil)
							fmt.Fprintf(ch, "this is %s\n", name)
							_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
							_ = ch.Close()
							return
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// sshRun logs in to the gateway as login with key and runs a command;
// stdout, stderr and the error are what a user would get.
func sshRun(addr, login string, key ssh.Signer) (string, string, error) {
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: login, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second,
	})
	if err != nil {
		return "", "", err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", "", err
	}
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	done := make(chan error, 1)
	go func() { done <- sess.Run("whoami") }()
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		err = context.DeadlineExceeded
	}
	return strings.TrimSpace(out.String()), errb.String(), err
}
