// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package sshmsg

import (
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestParsePtyReq(t *testing.T) {
	p := ssh.Marshal(struct {
		Term             string
		Cols, Rows, W, H uint32
		Modes            string
	}{"xterm-256color", 120, 40, 0, 0, ""})
	got, err := ParsePtyReq(p)
	if err != nil || got != (PtyReq{Term: "xterm-256color", Cols: 120, Rows: 40}) {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestParseStringPayloads(t *testing.T) {
	one := func(s string) []byte { return ssh.Marshal(struct{ S string }{s}) }
	if got, err := ParseExec(one("scp -t /tmp")); err != nil || got != "scp -t /tmp" {
		t.Fatalf("exec: %q %v", got, err)
	}
	if got, err := ParseSubsystem(one("sftp")); err != nil || got != "sftp" {
		t.Fatalf("subsystem: %q %v", got, err)
	}
}

func TestParseWindowChange(t *testing.T) {
	p := ssh.Marshal(struct{ Cols, Rows, W, H uint32 }{100, 30, 0, 0})
	c, r, err := ParseWindowChange(p)
	if err != nil || c != 100 || r != 30 {
		t.Fatalf("got %d %d %v", c, r, err)
	}
}

func TestParseExitStatusAndSignal(t *testing.T) {
	st, err := ParseExitStatus(ssh.Marshal(struct{ S uint32 }{3}))
	if err != nil || st != 3 {
		t.Fatalf("status %d %v", st, err)
	}
	sig, err := ParseExitSignal(ssh.Marshal(struct {
		Sig       string
		Core      bool
		Msg, Lang string
	}{"TERM", false, "", ""}))
	if err != nil || sig != "TERM" {
		t.Fatalf("signal %q %v", sig, err)
	}
}

func TestParseEnv(t *testing.T) {
	n, v, err := ParseEnv(ssh.Marshal(struct{ N, V string }{"LANG", "C.UTF-8"}))
	if err != nil || n != "LANG" || v != "C.UTF-8" {
		t.Fatalf("got %q=%q %v", n, v, err)
	}
}

func TestParseTCPIP(t *testing.T) {
	extra := ssh.Marshal(struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}{"127.0.0.1", 8888, "10.0.0.5", 51000})
	got, err := ParseTCPIPExtra(extra)
	if err != nil || got != (TCPIP{Host: "127.0.0.1", Port: 8888, OrigHost: "10.0.0.5", OrigPort: 51000}) {
		t.Fatalf("got %+v %v", got, err)
	}
	h, port, err := ParseTCPIPForward(ssh.Marshal(struct {
		Host string
		Port uint32
	}{"0.0.0.0", 9000}))
	if err != nil || h != "0.0.0.0" || port != 9000 {
		t.Fatalf("forward %q %d %v", h, port, err)
	}
}

func TestParse_Malformed(t *testing.T) {
	bad := []byte{0x00}
	if _, err := ParsePtyReq(bad); err == nil {
		t.Error("pty-req")
	}
	if _, err := ParseExec(bad); err == nil {
		t.Error("exec")
	}
	if _, err := ParseSubsystem(bad); err == nil {
		t.Error("subsystem")
	}
	if _, _, err := ParseWindowChange(bad); err == nil {
		t.Error("window-change")
	}
	if _, err := ParseExitStatus(bad); err == nil {
		t.Error("exit-status")
	}
	if _, err := ParseExitSignal(bad); err == nil {
		t.Error("exit-signal")
	}
	if _, _, err := ParseEnv(bad); err == nil {
		t.Error("env")
	}
	if _, err := ParseTCPIPExtra(bad); err == nil {
		t.Error("tcpip extra")
	}
	if _, _, err := ParseTCPIPForward(bad); err == nil {
		t.Error("tcpip-forward")
	}
}
