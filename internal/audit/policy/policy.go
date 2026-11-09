// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package policy resolves the effective audit policy for one session
// from the cluster default and the initiating user's override.
package policy

import (
	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// WarnVSCodeBypass is surfaced when downloads are denied but VS Code's
// forwarded channel still offers a way around the restriction.
const WarnVSCodeBypass = "download is denied but local port forwarding is allowed: VS Code can bypass the download restriction"

// Effective is a fully resolved policy. The zero value allows everything
// and fails open.
type Effective struct {
	DenyDownload, DenyUpload            bool
	MaxFileSize                         int64 // bytes; 0 = unlimited
	DenyLocalForward, DenyRemoteForward bool
	RecordForwardContent                bool
	DenyAgentForward, DenyX11           bool
	FailClosed                          bool
}

// Resolve merges field by field: a field set in user wins, otherwise the
// default's value applies, otherwise Allow / Open / unlimited.
func Resolve(def, user *devpodv1alpha1.AuditPolicy) Effective {
	if def == nil {
		def = &devpodv1alpha1.AuditPolicy{}
	}
	if user == nil {
		user = &devpodv1alpha1.AuditPolicy{}
	}
	dfw, ufw := def.Forwarding, user.Forwarding
	if dfw == nil {
		dfw = &devpodv1alpha1.ForwardingPolicy{}
	}
	if ufw == nil {
		ufw = &devpodv1alpha1.ForwardingPolicy{}
	}

	var e Effective
	e.DenyDownload = denied(user.Download, def.Download)
	e.DenyUpload = denied(user.Upload, def.Upload)
	e.DenyAgentForward = denied(user.AgentForwarding, def.AgentForwarding)
	e.DenyX11 = denied(user.X11Forwarding, def.X11Forwarding)
	e.DenyLocalForward = denied(ufw.Local, dfw.Local)
	e.DenyRemoteForward = denied(ufw.Remote, dfw.Remote)

	switch {
	case ufw.RecordContent != nil:
		e.RecordForwardContent = *ufw.RecordContent
	case dfw.RecordContent != nil:
		e.RecordForwardContent = *dfw.RecordContent
	}
	switch {
	case user.MaxFileSize != nil:
		e.MaxFileSize = user.MaxFileSize.Value()
	case def.MaxFileSize != nil:
		e.MaxFileSize = def.MaxFileSize.Value()
	}
	switch {
	case user.FailMode != nil:
		e.FailClosed = *user.FailMode == devpodv1alpha1.FailClosed
	case def.FailMode != nil:
		e.FailClosed = *def.FailMode == devpodv1alpha1.FailClosed
	}
	return e
}

func denied(user, def *devpodv1alpha1.PolicyAction) bool {
	if user != nil {
		return *user == devpodv1alpha1.PolicyDeny
	}
	return def != nil && *def == devpodv1alpha1.PolicyDeny
}

// HasDeny reports whether any content rule (download, upload, size) is
// active — the rules a broken decoder can no longer enforce.
func (e Effective) HasDeny() bool {
	return e.DenyDownload || e.DenyUpload || e.MaxFileSize > 0
}

// Warnings lists configuration combinations that do not do what an
// operator probably expects.
func (e Effective) Warnings() []string {
	var w []string
	if e.DenyDownload && !e.DenyLocalForward {
		w = append(w, WarnVSCodeBypass)
	}
	return w
}
