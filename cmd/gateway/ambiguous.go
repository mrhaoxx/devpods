// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ambiguousMessage tells someone whose key was accepted how to say
// which of several DevPods of one name they mean.
func ambiguousMessage(logins []string) string {
	var b strings.Builder
	b.WriteString("Several clusters have a DevPod of this name. Say which one, with the cluster after the name:\r\n\r\n")
	for _, l := range logins {
		b.WriteString("  ssh " + l + "@<this address>\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

// refuseWith answers a connection that will not be served: the first
// session gets msg on its error stream and a failing exit status, and
// the connection is closed. Anything else the client opens is refused.
func refuseWith(conn ssh.Conn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, msg string) {
	go ssh.DiscardRequests(reqs)
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return
		case nc, ok := <-chans:
			if !ok {
				return
			}
			if nc.ChannelType() != "session" {
				_ = nc.Reject(ssh.Prohibited, "say which cluster first")
				continue
			}
			ch, creqs, err := nc.Accept()
			if err != nil {
				return
			}
			// the message goes out once the client has asked for something
			// to run (a shell, a command): before that it may not be read
			done := make(chan struct{})
			go func() {
				defer close(done)
				for r := range creqs {
					started := r.Type == "shell" || r.Type == "exec" || r.Type == "subsystem"
					if r.WantReply {
						_ = r.Reply(r.Type == "pty-req" || r.Type == "env" || started, nil)
					}
					if started {
						return
					}
				}
			}()
			select {
			case <-done:
			case <-timer.C:
			}
			_, _ = ch.Stderr().Write([]byte(msg))
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, 1)
			_, _ = ch.SendRequest("exit-status", false, status)
			_ = ch.Close()
			_ = conn.Close()
			return
		}
	}
}
