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
}

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
	dialer := &net.Dialer{Timeout: DialTimeout}
	tcp, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         DialTimeout,
		ClientVersion:   "SSH-2.0-devpod-gateway",
	}
	conn, chans, reqs, err := ssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		_ = tcp.Close()
		return nil, nil, nil, fmt.Errorf("ssh handshake to %s: %w", addr, err)
	}
	return conn, chans, reqs, nil
}
