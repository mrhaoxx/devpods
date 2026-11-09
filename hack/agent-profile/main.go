// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command agent-profile runs the telemetry agent inside a container for
// -for, writing a CPU profile (audit-telemetry spec §14 item 4). Run it
// next to the supervisor's own agent; it posts under a separate boot id.
//
//	SUPERVISOR_SSHD_PID=… agent-profile -for 60s -cpuprofile /tmp/agent.prof
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
)

func main() {
	d := flag.Duration("for", time.Minute, "how long to run")
	out := flag.String("cpuprofile", "/tmp/agent.prof", "CPU profile output")
	mem := flag.String("memprofile", "", "allocation profile output (optional)")
	flag.Parse()
	cfg, err := config.LoadConfig(config.ConfigPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *d)
	defer cancel()
	_ = agent.Run(ctx, cfg, agent.DefaultEnv())
	pprof.StopCPUProfile()
	_ = f.Close()
	if *mem != "" {
		if mf, err := os.Create(*mem); err == nil {
			_ = pprof.Lookup("allocs").WriteTo(mf, 0)
			_ = mf.Close()
		}
	}
}
