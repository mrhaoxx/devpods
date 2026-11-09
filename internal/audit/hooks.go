// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package audit records SSH sessions at the gateway: every channel's
// raw stream goes to a local spool and on to S3, decoders turn SFTP /
// scp / ZMODEM into file events with content, and optional policies are
// enforced inline. The gateway's proxy talks to it through SessionHooks.
package audit

import (
	"github.com/mrhaoxx/devpod/internal/audit/decode"
)

// Dir is the direction a message travels.
type Dir = decode.Dir

const (
	ClientToBackend = decode.ClientToBackend
	BackendToClient = decode.BackendToClient
)

// Result is what to do with one chunk of channel data.
type Result = decode.Result

// User-visible rejection texts (spec §7).
const (
	MsgForwardDenied    = "blocked by DevPod audit policy: port forwarding disabled"
	MsgAgentDenied      = "blocked by DevPod audit policy: agent forwarding disabled"
	MsgX11Denied        = "blocked by DevPod audit policy: X11 forwarding disabled"
	MsgAuditUnavailable = "audit storage unavailable (fail-closed policy)"
)

// DenyError rejects a channel open or request; Msg is shown to the user.
type DenyError struct{ Msg string }

func (e *DenyError) Error() string { return e.Msg }

// SessionHooks observes and filters one proxied SSH connection.
type SessionHooks interface {
	// OpenChannel is consulted before a channel is opened on the far
	// side; an error rejects it (administratively prohibited).
	OpenChannel(dir Dir, chType string, extra []byte) (ChannelHooks, error)
	// GlobalRequest is consulted before a global request is forwarded;
	// an error replies false without forwarding.
	GlobalRequest(dir Dir, typ string, payload []byte) error
	// Done is closed when the session must be torn down.
	Done() <-chan struct{}
	// Close ends the session.
	Close(reason string, bytesIn, bytesOut int64)
}

// ChannelHooks observes and filters one channel.
type ChannelHooks interface {
	// Request is consulted before a channel request is forwarded; an
	// error replies false without forwarding.
	Request(dir Dir, typ string, wantReply bool, payload []byte) error
	// RequestResult reports the far side's answer to the last request.
	RequestResult(dir Dir, typ string, ok bool)
	// Data filters one chunk travelling in dir.
	Data(dir Dir, stderr bool, p []byte) Result
	// Close ends the channel.
	Close()
}

// Noop returns hooks that record nothing and allow everything.
func Noop() SessionHooks { return noopSession{} }

type noopSession struct{}

func (noopSession) OpenChannel(Dir, string, []byte) (ChannelHooks, error) {
	return noopChannel{}, nil
}
func (noopSession) GlobalRequest(Dir, string, []byte) error { return nil }
func (noopSession) Done() <-chan struct{}                   { return nil }
func (noopSession) Close(string, int64, int64)              {}

type noopChannel struct{}

func (noopChannel) Request(Dir, string, bool, []byte) error { return nil }
func (noopChannel) RequestResult(Dir, string, bool)         {}
func (noopChannel) Data(_ Dir, _ bool, p []byte) Result     { return Result{Pass: p} }
func (noopChannel) Close()                                  {}
