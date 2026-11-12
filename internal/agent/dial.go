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

// DialPurpose is the purpose of a stream that asks for a connection to a
// DevPod's address.
func DialPurpose(addr, purpose string) []byte {
	b, err := json.Marshal(Purpose{Kind: "dial", Addr: addr, For: purpose})
	if err != nil {
		panic(err)
	}
	return b
}

// DialAllowed is the rule a gateway's request is held to: the address is
// that of a running DevPod of the namespace; for SSH the port is that
// DevPod's sshd, for HTTP any port but that one. Nothing else in the
// cluster — no other workload, no node, not the API — can be reached
// through the agent, whoever asks.
//
// A DevPod on the host network has its node's address: for HTTP that is
// any port of the node (as it is for a gateway in the cluster); for SSH
// it is still only the DevPod's own sshd port.
func DialAllowed(ctx context.Context, r client.Reader, namespace, addr, purpose string) error {
	if purpose != ForSSH && purpose != ForHTTP {
		return fmt.Errorf("unknown purpose %q", purpose)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("not an address")
	}
	ip := net.ParseIP(host)
	if n, err := strconv.Atoi(port); ip == nil || err != nil || n < 1 || n > 65535 {
		return errors.New("not an address")
	}
	var list devpodv1alpha1.DevPodList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("cannot tell: %w", err)
	}
	for i := range list.Items {
		dp := &list.Items[i]
		if dp.Status.Phase != devpodv1alpha1.DevPodRunning || dp.Status.Endpoint == "" {
			continue
		}
		h, sshPort, err := net.SplitHostPort(dp.Status.Endpoint)
		if err != nil || !ip.Equal(net.ParseIP(h)) {
			continue
		}
		switch {
		case purpose == ForSSH && port == sshPort:
			return nil
		case purpose == ForHTTP && port != sshPort:
			return nil
		}
	}
	return errors.New("not a DevPod's address for " + purpose)
}

// dialAnswer is the one line the agent says before a dial stream becomes
// the connection: whether it is there.
type dialAnswer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func answerDial(conn net.Conn, err error) {
	a := dialAnswer{OK: err == nil}
	if err != nil {
		a.Error = err.Error()
	}
	b, _ := json.Marshal(a)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = conn.Write(append(b, '\n'))
	_ = conn.SetWriteDeadline(time.Time{})
}

// AwaitDial reads the agent's answer on a dial stream: nil means what
// follows is the connection to the DevPod. Not a byte more is read.
func AwaitDial(conn net.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	var line []byte
	one := make([]byte, 1)
	for len(line) < 1024 {
		if _, err := io.ReadFull(conn, one); err != nil {
			return fmt.Errorf("the cluster's agent did not answer: %w", err)
		}
		if one[0] == '\n' {
			var a dialAnswer
			if err := json.Unmarshal(line, &a); err != nil {
				return errors.New("the cluster's agent answered something else")
			}
			if !a.OK {
				return errors.New("the cluster's agent refused: " + a.Error)
			}
			return nil
		}
		line = append(line, one[0])
	}
	return errors.New("the cluster's agent answered something else")
}

// serveDial connects a gateway's stream to a DevPod's address, if the
// rule allows it.
func (a *Agent) serveDial(ctx context.Context, p Purpose, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if a.o.DevPods == nil {
		answerDial(conn, errors.New("this agent makes no connections to DevPods"))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := DialAllowed(cctx, a.o.DevPods, a.o.Namespace, p.Addr, p.For)
	var target net.Conn
	if err == nil {
		target, err = (&net.Dialer{}).DialContext(cctx, "tcp", p.Addr)
	}
	cancel()
	answerDial(conn, err)
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
