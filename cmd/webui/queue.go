// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
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

// podLogs reads a Pod's log: in the console's own cluster ("") through
// its own config, in another through that cluster's agent.
func podLogs(local *rest.Config, reg *clusters.Registry) func(ctx context.Context, cluster, ns, pod, container string, follow bool) (io.ReadCloser, error) {
	return func(ctx context.Context, cluster, ns, pod, container string, follow bool) (io.ReadCloser, error) {
		cfg := local
		if cluster != "" && reg != nil {
			cfg = reg.RESTConfig(cluster)
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		return cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: follow}).Stream(ctx)
	}
}
