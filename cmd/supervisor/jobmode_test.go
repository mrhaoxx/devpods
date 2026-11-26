// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
)

func startChild(t *testing.T, name, script string) *child {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &child{name: name, cmd: cmd}
}

// In a job the command is the point: sshd ending does not end it.
func TestJobModeSurvivesSshd(t *testing.T) {
	children := []*child{startChild(t, "sshd", "exit 1"), startChild(t, "user", "sleep 0.5; exit 7")}
	var stopping atomic.Bool
	if code := waitForChildren(children, &stopping, true, true); code != 7 {
		t.Fatalf("exit %d, want the command's 7", code)
	}
}

// Elsewhere sshd ending ends the supervisor, as before.
func TestNotJobModeEndsWithSshd(t *testing.T) {
	children := []*child{startChild(t, "sshd", "exit 1"), startChild(t, "user", "sleep 5; exit 7")}
	var stopping atomic.Bool
	if code := waitForChildren(children, &stopping, true, false); code != 1 {
		t.Fatalf("exit %d, want sshd's 1", code)
	}
}
