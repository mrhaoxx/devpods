// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"testing"
)

// The agent is sequential; on a 104-core node without a CPU limit the Go
// default (GOMAXPROCS = cores) made every GC cycle fan out over 104 Ps
// and cost more than the sampling itself (spec §14 item 4).
func TestTelemetryChildEnv(t *testing.T) {
	t.Setenv("GOMAXPROCS", "64") // the container's own setting does not leak through
	env := newTelemetrySup(42, 77).env
	for _, want := range []string{"GOMEMLIMIT=24MiB", "GOMAXPROCS=2", "SUPERVISOR_SSHD_PID=42", "SUPERVISOR_WORKLOAD_PID=77"} {
		if !slices.Contains(env, want) {
			t.Errorf("child env lacks %s", want)
		}
	}
	if slices.Contains(env, "GOMAXPROCS=64") {
		t.Error("the container's GOMAXPROCS must be replaced, not kept beside ours")
	}
}
