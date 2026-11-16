// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// DialTimeout is the default backend-dial timeout. Exported so cmd/gateway
// can size its dial context consistently.
const DialTimeout = 5 * time.Second

// Dialer opens SSH connections to backend (per-DevPod) sshd instances.
type Dialer struct {
	signer ssh.Signer
	// Net, when set, is how a DevPod's address is reached: through the
	// agent of its cluster (a gateway of several clusters has no route to
	// a Pod of its own). Nil: a plain TCP connection.
	Net func(ctx context.Context, t Target) (net.Conn, error)
}

// Target is where a connection to a DevPod goes and what it is for. The
// cluster's agent checks it against what it knows of that DevPod.
type Target struct {
	Cluster   string
	Addr      string // the DevPod's address and a port
	Purpose   string // PurposeSSH or PurposeHTTP
	DevPodUID string
	User      string // who the SSH session is for
}

// What a connection to a DevPod is for; the cluster's agent holds each
// to its own rule (sshd's port for SSH, any other port for HTTP).
const (
	PurposeSSH  = "ssh"
	PurposeHTTP = "http"
)

// NewDialer returns a Dialer that authenticates to the backend using
// the given gateway internal signing key.
func NewDialer(internalKey ssh.Signer) *Dialer {
	return &Dialer{signer: internalKey}
}

// Dial opens a TCP connection to addr and completes an SSH client
// handshake. The returned channels and request channel must be drained
// or routed by the caller (typically the proxy loop). The host key is
// not verified — the backend is reached over a cluster-internal Pod IP
// which is itself the trust anchor.
func (d *Dialer) Dial(ctx context.Context, addr string) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	return d.DialIn(ctx, Target{Addr: addr})
}

// DialIn is Dial to the DevPod t names, for the user it names.
func (d *Dialer) DialIn(ctx context.Context, t Target) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	addr := t.Addr
	t.Purpose = PurposeSSH
	var tcp net.Conn
	var err error
	if d.Net != nil {
		tcp, err = d.Net(ctx, t)
	} else {
		dialer := &net.Dialer{Timeout: DialTimeout}
		tcp, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return d.Handshake(tcp, addr, "root")
}

// Handshake completes the SSH client handshake on conn, made elsewhere,
// as user, with the gateway's internal key: a DevPod's sshd (as root), or
// the console's CLI (as the user the gateway signed in). The host key is
// not checked: how conn was reached is the trust anchor. conn is closed
// when the handshake fails.
func (d *Dialer) Handshake(conn net.Conn, addr, user string) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         DialTimeout,
		ClientVersion:   "SSH-2.0-devpod-gateway",
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("ssh handshake to %s: %w", addr, err)
	}
	return c, chans, reqs, nil
}
