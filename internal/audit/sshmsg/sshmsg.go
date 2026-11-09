// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package sshmsg parses the RFC 4254 payloads of channel requests,
// global requests and channel-open extra data that the audit layer
// inspects.
package sshmsg

import (
	"golang.org/x/crypto/ssh"
)

// PtyReq is the interesting part of a "pty-req" payload.
type PtyReq struct {
	Term       string
	Cols, Rows uint32
}

// ParsePtyReq parses a "pty-req" request payload.
func ParsePtyReq(p []byte) (PtyReq, error) {
	var m struct {
		Term             string
		Cols, Rows, W, H uint32
		Modes            string
	}
	if err := ssh.Unmarshal(p, &m); err != nil {
		return PtyReq{}, err
	}
	return PtyReq{Term: m.Term, Cols: m.Cols, Rows: m.Rows}, nil
}

func parseString(p []byte) (string, error) {
	var m struct{ S string }
	if err := ssh.Unmarshal(p, &m); err != nil {
		return "", err
	}
	return m.S, nil
}

// ParseExec parses an "exec" request payload (the command line).
func ParseExec(p []byte) (string, error) { return parseString(p) }

// ParseSubsystem parses a "subsystem" request payload (the name).
func ParseSubsystem(p []byte) (string, error) { return parseString(p) }

// ParseWindowChange parses a "window-change" request payload.
func ParseWindowChange(p []byte) (cols, rows uint32, err error) {
	var m struct{ Cols, Rows, W, H uint32 }
	if err := ssh.Unmarshal(p, &m); err != nil {
		return 0, 0, err
	}
	return m.Cols, m.Rows, nil
}

// ParseExitStatus parses an "exit-status" request payload.
func ParseExitStatus(p []byte) (uint32, error) {
	var m struct{ S uint32 }
	if err := ssh.Unmarshal(p, &m); err != nil {
		return 0, err
	}
	return m.S, nil
}

// ParseExitSignal parses an "exit-signal" request payload (signal name).
func ParseExitSignal(p []byte) (string, error) {
	var m struct {
		Signal    string
		Core      bool
		Msg, Lang string
	}
	if err := ssh.Unmarshal(p, &m); err != nil {
		return "", err
	}
	return m.Signal, nil
}

// ParseEnv parses an "env" request payload.
func ParseEnv(p []byte) (name, value string, err error) {
	var m struct{ N, V string }
	if err := ssh.Unmarshal(p, &m); err != nil {
		return "", "", err
	}
	return m.N, m.V, nil
}

// TCPIP is the extra data of "direct-tcpip" and "forwarded-tcpip"
// channel opens (both share one layout).
type TCPIP struct {
	Host     string
	Port     uint32
	OrigHost string
	OrigPort uint32
}

// ParseTCPIPExtra parses direct-tcpip / forwarded-tcpip extra data.
func ParseTCPIPExtra(extra []byte) (TCPIP, error) {
	var m TCPIP
	if err := ssh.Unmarshal(extra, &m); err != nil {
		return TCPIP{}, err
	}
	return m, nil
}

// ParseTCPIPForward parses a "tcpip-forward" global request payload.
func ParseTCPIPForward(p []byte) (host string, port uint32, err error) {
	var m struct {
		Host string
		Port uint32
	}
	if err := ssh.Unmarshal(p, &m); err != nil {
		return "", 0, err
	}
	return m.Host, m.Port, nil
}
