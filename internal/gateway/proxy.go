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

	"github.com/mrhaoxx/devpod/internal/audit"
)

// ProxyStats accumulates byte counts for one proxied SSH connection.
// Both fields are written by the per-channel data pumps in goroutines,
// so use atomics. Callers (cmd/gateway) read them once at session
// close.
type ProxyStats struct {
	BytesClientToBackend atomic.Int64
	BytesBackendToClient atomic.Int64
}

// Proxy bridges a client SSH connection (server-side from our POV) and
// a backend SSH connection (client-side from our POV). Runs until either
// side closes, or hooks.Done() fires; returns the close reason.
//
// hooks (nil = audit.Noop()) sees every channel open, request and data
// chunk before it is forwarded and may reject or rewrite it.
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
	hooks audit.SessionHooks,
) error {
	if hooks == nil {
		hooks = audit.Noop()
	}
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
			go proxyChannel(nc, backendConn, stats, hooks, audit.ClientToBackend)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for nc := range backendChans {
			go proxyChannel(nc, clientConn, nil, hooks, audit.BackendToClient)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for req := range clientReqs {
			if err := hooks.GlobalRequest(audit.ClientToBackend, req.Type, req.Payload); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
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
			if err := hooks.GlobalRequest(audit.BackendToClient, req.Type, req.Payload); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			ok, payload, _ := clientConn.SendRequest(req.Type, req.WantReply, req.Payload)
			_ = req.Reply(ok, payload)
		}
	}()

	stop := make(chan struct{})
	go func() {
		select {
		case <-hooks.Done(): // e.g. fail-closed audit storage is full
			_ = clientConn.Close()
			_ = backendConn.Close()
		case <-stop:
		}
	}()

	closeErr := make(chan error, 2)
	go func() { closeErr <- clientConn.Wait() }()
	go func() { closeErr <- backendConn.Wait() }()

	err := <-closeErr
	close(stop)
	_ = clientConn.Close()
	_ = backendConn.Close()
	wg.Wait()

	if err == io.EOF {
		return nil
	}
	return err
}

func opposite(d audit.Dir) audit.Dir {
	if d == audit.ClientToBackend {
		return audit.BackendToClient
	}
	return audit.ClientToBackend
}

// lockedWriter serialises writes to one channel side so that a unit
// injected by one pump (an audit Reply) never interleaves with a unit
// forwarded by the other.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.w.Write(p)
	return err
}

// proxyChannel opens the corresponding channel on remote, accepts the
// local channel, and pumps stdin/stdout/stderr + channel requests in
// both directions until both sides close. l2r is the direction of data
// flowing from the local (opening) side to remote.
//
// stats (optional) counts bytes assuming local=client / remote=backend:
// lch→rch writes bump BytesClientToBackend; rch→lch writes bump
// BytesBackendToClient. Pass nil to skip counting.
func proxyChannel(local ssh.NewChannel, remote ssh.Conn, stats *ProxyStats, hooks audit.SessionHooks, l2r audit.Dir) {
	r2l := opposite(l2r)
	ch, err := hooks.OpenChannel(l2r, local.ChannelType(), local.ExtraData())
	if err != nil {
		_ = local.Reject(ssh.Prohibited, err.Error())
		return
	}
	defer ch.Close()

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

	lw := &lockedWriter{w: lch}
	rw := &lockedWriter{w: rch}
	lerr := &lockedWriter{w: lch.Stderr()}
	// clientErr is the client's stderr on this channel: audit notices
	// and rejection reasons go there (OpenSSH prints it verbatim).
	clientErr := lerr
	if l2r == audit.BackendToClient {
		clientErr = &lockedWriter{w: rch.Stderr()}
	}
	closeBoth := func() {
		_ = lch.Close()
		_ = rch.Close()
	}

	// OpenNG-style close orchestration. Two signal channels gate the
	// closes so exit-status (and any other late channel-requests) gets
	// flushed before we tear the channel down.
	//
	//   stdup   = remote → local copy finished
	//   stddown = local → remote copy finished
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
	// dropped. Hold the local→remote CloseWrite until the start-program
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

	forwardReqs := func(in <-chan *ssh.Request, to ssh.Channel, dir audit.Dir, onStart func()) {
		for r := range in {
			if err := ch.Request(dir, r.Type, r.WantReply, r.Payload); err != nil {
				if dir == audit.ClientToBackend {
					_ = clientErr.write([]byte(err.Error() + "\n"))
				}
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			ok, _ := to.SendRequest(r.Type, r.WantReply, r.Payload)
			ch.RequestResult(dir, r.Type, ok)
			_ = r.Reply(ok, nil)
			switch r.Type {
			case "shell", "exec", "subsystem":
				if onStart != nil {
					onStart()
				}
			}
		}
	}

	go func() {
		forwardReqs(lreqs, rch, l2r, markStartProgSent)
		// If the client closed without ever asking us to start a program,
		// unblock the data pump so we don't leak.
		markStartProgSent()
		<-stddown
		_ = rch.Close()
	}()
	go func() {
		forwardReqs(rreqs, lch, r2l, nil)
		<-stdup
		_ = lch.Close()
	}()

	// pump reads src, lets the hooks filter each chunk, forwards Pass to
	// dst and writes Reply back to the sender.
	pump := func(src io.Reader, dst, back *lockedWriter, dir audit.Dir, stderr bool, count *atomic.Int64) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if count != nil {
					count.Add(int64(n))
				}
				res := ch.Data(dir, stderr, buf[:n])
				if werr := dst.write(res.Pass); werr != nil {
					return
				}
				_ = back.write(res.Reply)
				_ = clientErr.write(res.Notice)
				if res.CloseChannel {
					closeBoth()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}

	var toRemote, toLocal *atomic.Int64
	if stats != nil {
		toRemote, toLocal = &stats.BytesClientToBackend, &stats.BytesBackendToClient
	}

	// Stderr is unidirectional: remote → local.
	go pump(rch.Stderr(), lerr, rw, r2l, true, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pump(rch, lw, rw, r2l, false, toLocal)
		_ = lch.CloseWrite()
		close(stdup)
	}()
	go func() {
		defer wg.Done()
		pump(lch, rw, lw, l2r, false, toRemote)
		<-startProgSent
		_ = rch.CloseWrite()
		close(stddown)
	}()
	wg.Wait()
}
