// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/mrhaoxx/devpod/internal/webui"
)

// queueFlags: the job queue (design 2026-11-26), which the console keeps
// (Job, JobPriorityRule, in its own cluster) and schedules.
type queueFlags struct {
	enabled                bool
	defaultLimit, maxLimit time.Duration
}

func addQueueFlags(fs *flag.FlagSet) *queueFlags {
	f := &queueFlags{}
	fs.BoolVar(&f.enabled, "queue", false, "enable the job queue (needs the Job and JobPriorityRule CRDs in the console's cluster; clusters take jobs when their agent says \"jobs\")")
	fs.DurationVar(&f.defaultLimit, "job-default-time-limit", 24*time.Hour, "a job's time limit when it says none")
	fs.DurationVar(&f.maxLimit, "job-max-time-limit", 72*time.Hour, "the longest time limit a job may ask (a Cluster's spec.jobMaxTimeLimit may say less)")
	return f
}

// build returns nil when the queue is off.
func (f *queueFlags) build() (*webui.QueueRules, error) {
	if !f.enabled {
		return nil, nil
	}
	if f.defaultLimit <= 0 || f.maxLimit < f.defaultLimit {
		return nil, fmt.Errorf("invalid job time limits: default %s, max %s", f.defaultLimit, f.maxLimit)
	}
	return &webui.QueueRules{DefaultTimeLimit: f.defaultLimit, MaxTimeLimit: f.maxLimit, GangTimeout: 5 * time.Minute,
		PreemptGrace: 2 * time.Minute, Tick: 5 * time.Second, KeepEnded: 30 * 24 * time.Hour}, nil
}
