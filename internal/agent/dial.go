// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// What a gateway's connection to a DevPod is for.
const (
	ForSSH  = "ssh"
	ForHTTP = "http"
)

// DialPurpose is the purpose of a stream that asks for a connection to
// the DevPod of that uid, at addr, for purpose; user is who the SSH
// session is for.
func DialPurpose(addr, purpose, uid, user string) []byte {
	b, err := json.Marshal(Purpose{Kind: "dial", Addr: addr, For: purpose, UID: uid, User: user})
	if err != nil {
		panic(err)
	}
	return b
}

// DialAllowed is the rule a gateway's request is held to, by what the
// agent itself knows of the cluster right now — not by what the gateway
// or the console believe:
//
//   - the DevPod of that uid is in the namespace and running, and the
//     address is its own;
//   - for SSH the port is its sshd, and the user is its owner or one of
//     its collaborators;
//   - for HTTP the port is any but its sshd.
//
// Nothing else in the cluster — no other workload, no node, not the API,
// not another DevPod that has since taken the address — can be reached
// through the agent, whoever asks.
//
// A DevPod on the host network has its node's address: for HTTP that is
// any port of the node (as it is for a gateway in the cluster); for SSH
// it is still only the DevPod's own sshd port.
func DialAllowed(ctx context.Context, r client.Reader, namespace string, p Purpose) error {
	if p.For != ForSSH && p.For != ForHTTP {
		return fmt.Errorf("unknown purpose %q", p.For)
	}
	if p.UID == "" {
		return errors.New("which DevPod is not said")
	}
	host, port, err := net.SplitHostPort(p.Addr)
	if err != nil {
		return errors.New("not an address")
	}
	ip := net.ParseIP(host)
	want, err := strconv.Atoi(port)
	if ip == nil || err != nil || want < 1 || want > 65535 {
		return errors.New("not an address")
	}
	var list devpodv1alpha1.DevPodList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("cannot tell: %w", err)
	}
	for i := range list.Items {
		dp := &list.Items[i]
		if string(dp.UID) != p.UID {
			continue
		}
		if dp.Status.Phase != devpodv1alpha1.DevPodRunning || dp.Status.Endpoint == "" {
			return errors.New("the DevPod is not running")
		}
		h, sp, err := net.SplitHostPort(dp.Status.Endpoint)
		sshPort, perr := strconv.Atoi(sp)
		if err != nil || perr != nil || !ip.Equal(net.ParseIP(h)) {
			return errors.New("not the DevPod's address")
		}
		switch p.For {
		case ForSSH:
			if want != sshPort {
				return errors.New("not the DevPod's sshd")
			}
			if p.User != dp.Spec.Owner && !slices.Contains(dp.Spec.Collaborators, p.User) {
				return errors.New("not a DevPod this user may log in to")
			}
		case ForHTTP:
			if want == sshPort {
				return errors.New("the DevPod's sshd is not an HTTP service")
			}
		}
		return nil
	}
	return errors.New("not a DevPod of this cluster")
}

// dialAnswer is the one line the agent says before a dial stream becomes
// the connection: whether it is there, and the agent's end of it as the
// DevPod sees it (a shell's peer there; the gateway records it with the
// session, so the shell's commands can be told apart by it).
type dialAnswer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Local string `json:"local,omitempty"`
}

func answerDial(conn net.Conn, err error, local string) {
	a := dialAnswer{OK: err == nil, Local: local}
	if err != nil {
		a.Error = err.Error()
	}
	b, _ := json.Marshal(a)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = conn.Write(append(b, '\n'))
	_ = conn.SetWriteDeadline(time.Time{})
}

// AwaitDial reads the agent's answer on a dial stream: without an error,
// what follows is the connection to the DevPod, returned with the local
// address the DevPod sees (an agent that does not say it: the stream's
// own). Not a byte more is read.
func AwaitDial(conn net.Conn) (net.Conn, error) {
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	var line []byte
	one := make([]byte, 1)
	for len(line) < 1024 {
		if _, err := io.ReadFull(conn, one); err != nil {
			return nil, fmt.Errorf("the cluster's agent did not answer: %w", err)
		}
		if one[0] == '\n' {
			var a dialAnswer
			if err := json.Unmarshal(line, &a); err != nil {
				return nil, errors.New("the cluster's agent answered something else")
			}
			if !a.OK {
				return nil, errors.New("the cluster's agent refused: " + a.Error)
			}
			if a.Local == "" {
				return conn, nil
			}
			return localConn{Conn: conn, local: tcpAddr(a.Local)}, nil
		}
		line = append(line, one[0])
	}
	return nil, errors.New("the cluster's agent answered something else")
}

// tcpAddr is an address as the agent wrote it.
type tcpAddr string

func (a tcpAddr) Network() string { return "tcp" }
func (a tcpAddr) String() string  { return string(a) }

// localConn is a dial stream that says the agent's local address.
type localConn struct {
	net.Conn
	local net.Addr
}

func (c localConn) LocalAddr() net.Addr { return c.local }

// serveDial connects a gateway's stream to a DevPod's address, if the
// rule allows it.
func (a *Agent) serveDial(ctx context.Context, p Purpose, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if a.o.DevPods == nil {
		answerDial(conn, errors.New("this agent makes no connections to DevPods"), "")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := DialAllowed(cctx, a.o.DevPods, a.o.Namespace, p)
	var target net.Conn
	if err == nil {
		target, err = (&net.Dialer{}).DialContext(cctx, "tcp", p.Addr)
	}
	cancel()
	local := ""
	if err == nil {
		local = target.LocalAddr().String()
	}
	answerDial(conn, err, local)
	if err != nil {
		return
	}
	defer func() { _ = target.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, target); done <- struct{}{} }()
	select { // one direction ending ends the connection: a stream has no half-close
	case <-done:
	case <-ctx.Done():
	}
}
