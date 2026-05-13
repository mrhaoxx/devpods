// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

// ProxyStats accumulates byte counts for one proxied SSH connection.
// Both fields are written by the per-channel data pumps in goroutines,
// so use atomics. Callers (cmd/gateway) read them once at session
// close.
type ProxyStats struct {
	BytesClientToBackend atomic.Int64
	BytesBackendToClient atomic.Int64
}

// countingReader is an io.Reader that increments a *atomic.Int64 by the
// number of bytes Read'd.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

// Proxy bridges a client SSH connection (server-side from our POV) and
// a backend SSH connection (client-side from our POV). Runs until either
// side closes; returns the close reason.
//
// Ported from OpenNG/ngssh/proxy.go's HandleSSH: four goroutines fan
// channel-open events and global requests in both directions; per-channel
// pumping is in proxyChannel.
func Proxy(
	clientConn ssh.Conn,
	clientChans <-chan ssh.NewChannel,
	clientReqs <-chan *ssh.Request,
	backendConn ssh.Conn,
	backendChans <-chan ssh.NewChannel,
	backendReqs <-chan *ssh.Request,
	stats *ProxyStats,
) error {
	var wg sync.WaitGroup

	// proxyChannel's stats parameter assumes local=client / remote=backend
	// orientation (lch→rch is client→backend, rch→lch is backend→client).
	// Backend-initiated channels (rare; e.g. forwarded-tcpip from a remote
	// listener) flip that orientation, so we pass nil and skip counting
	// for them rather than mis-attribute bytes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for nc := range clientChans {
			go proxyChannel(nc, backendConn, stats)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for nc := range backendChans {
			go proxyChannel(nc, clientConn, nil)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for req := range clientReqs {
			ok, payload, _ := backendConn.SendRequest(req.Type, req.WantReply, req.Payload)
			_ = req.Reply(ok, payload)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for req := range backendReqs {
			if req.Type == "hostkeys-00@openssh.com" {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			ok, payload, _ := clientConn.SendRequest(req.Type, req.WantReply, req.Payload)
			_ = req.Reply(ok, payload)
		}
	}()

	closeErr := make(chan error, 2)
	go func() { closeErr <- clientConn.Wait() }()
	go func() { closeErr <- backendConn.Wait() }()

	err := <-closeErr
	_ = clientConn.Close()
	_ = backendConn.Close()
	wg.Wait()

	if err == io.EOF {
		return nil
	}
	return err
}

// proxyChannel opens the corresponding channel on remote, accepts the
// local channel, and pumps stdin/stdout/stderr + channel requests in
// both directions until both sides close.
//
// stats (optional) counts bytes assuming local=client / remote=backend:
// lch→rch writes bump BytesClientToBackend; rch→lch writes bump
// BytesBackendToClient. Pass nil to skip counting.
func proxyChannel(local ssh.NewChannel, remote ssh.Conn, stats *ProxyStats) {
	rch, rreqs, err := remote.OpenChannel(local.ChannelType(), local.ExtraData())
	if err != nil {
		if oce, ok := err.(*ssh.OpenChannelError); ok {
			_ = local.Reject(oce.Reason, oce.Message)
		} else {
			_ = local.Reject(ssh.ConnectionFailed, fmt.Sprintf("open channel: %v", err))
		}
		return
	}

	lch, lreqs, err := local.Accept()
	if err != nil {
		_ = rch.Close()
		return
	}

	// OpenNG-style close orchestration. Two signal channels gate the
	// closes so exit-status (and any other late channel-requests) gets
	// flushed before we tear the channel down.
	//
	//   stdup   = backend → client copy finished
	//   stddown = client → backend copy finished
	//
	// Each request-forwarder, after its incoming reqs channel drains
	// (peer sent channel-close), waits for the corresponding data pump
	// to finish, then closes the *opposite* channel. That ordering is
	// what avoids the chicken-and-egg: the peer's close is the trigger
	// that drains reqs, then we close our side, which lets the other
	// peer's reqs drain too.
	stdup := make(chan struct{})
	stddown := make(chan struct{})

	// For session channels, sshd ignores SSH_MSG_CHANNEL_EOF that arrives
	// before the "shell"/"exec"/"subsystem" request has been processed:
	// the spawned process's stdin pipe is created after the request, so
	// nothing closes it when an early EOF was already received and
	// dropped. Hold the client→backend CloseWrite until the start-program
	// request has been forwarded (its SendRequest with WantReply=true
	// blocks until backend has the shell running and replies "success").
	//
	// Closed unconditionally for non-session channels so direct-tcpip
	// etc. pump immediately.
	startProgSent := make(chan struct{})
	var startProgOnce sync.Once
	markStartProgSent := func() {
		startProgOnce.Do(func() { close(startProgSent) })
	}
	if local.ChannelType() != "session" {
		// Non-session channels (direct-tcpip etc.) don't have the early-EOF
		// race that motivates startProgSent; flip the gate immediately.
		// Goes through Once so the lreqs forwarder's drain-time call below
		// is a safe no-op.
		markStartProgSent()
	}

	go func() {
		for r := range lreqs {
			ok, _ := rch.SendRequest(r.Type, r.WantReply, r.Payload)
			_ = r.Reply(ok, nil)
			switch r.Type {
			case "shell", "exec", "subsystem":
				markStartProgSent()
			}
		}
		// If the client closed without ever asking us to start a program,
		// unblock the data pump so we don't leak.
		markStartProgSent()
		<-stddown
		_ = rch.Close()
	}()
	go func() {
		for r := range rreqs {
			ok, _ := lch.SendRequest(r.Type, r.WantReply, r.Payload)
			_ = r.Reply(ok, nil)
		}
		<-stdup
		_ = lch.Close()
	}()

	// Stderr is unidirectional: backend → client.
	go func() { _, _ = io.Copy(lch.Stderr(), rch.Stderr()) }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var src io.Reader = rch
		if stats != nil {
			src = &countingReader{r: rch, n: &stats.BytesBackendToClient}
		}
		_, _ = io.Copy(lch, src)
		_ = lch.CloseWrite()
		close(stdup)
	}()
	go func() {
		defer wg.Done()
		var src io.Reader = lch
		if stats != nil {
			src = &countingReader{r: lch, n: &stats.BytesClientToBackend}
		}
		_, _ = io.Copy(rch, src)
		<-startProgSent
		_ = rch.CloseWrite()
		close(stddown)
	}()
	wg.Wait()
}
