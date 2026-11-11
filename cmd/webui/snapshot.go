// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/mrhaoxx/devpod/internal/webui"
)

// snapshotFlags: saving a DevPod as an image (snapshot spec v2).
type snapshotFlags struct {
	enabled                   bool
	imageTemplate, pushSecret string
	maxPerUser                int
}

func addSnapshotFlags(fs *flag.FlagSet) *snapshotFlags {
	f := &snapshotFlags{}
	fs.BoolVar(&f.enabled, "snapshots", false, "enable saving a DevPod as an image (needs the controller's snapshot reconciler)")
	fs.StringVar(&f.imageTemplate, "snapshot-image-template", "", "default target image with {user}, {name} and {tag}, e.g. harbor.example.com/devpod-users/{user}/{name}:{tag}; empty = users always name the address and bring credentials")
	fs.StringVar(&f.pushSecret, "snapshot-push-secret", "", "dockerconfigjson Secret in the devpods namespace used to push to template addresses")
	fs.IntVar(&f.maxPerUser, "snapshot-max-per-user", 10, "succeeded snapshots a user may keep; 0 = unlimited")
	return f
}

// build returns nil when snapshots are off. An image template alone
// also turns the feature on.
func (f *snapshotFlags) build(cfg *rest.Config) (*webui.Snapshots, error) {
	if !f.enabled && f.imageTemplate == "" {
		return nil, nil
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return webui.NewSnapshots(f.imageTemplate, f.pushSecret, f.maxPerUser, jobLogs(cs))
}

// jobLogs follows the output of the snapshot Job's Pod (Job
// "snapshot-<name>", container "snapshot").
func jobLogs(cs kubernetes.Interface) func(context.Context, string, string) (io.ReadCloser, error) {
	return func(ctx context.Context, namespace, snapshot string) (io.ReadCloser, error) {
		pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=snapshot-" + snapshot})
		if err != nil {
			return nil, err
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodPending {
				continue // no log yet
			}
			return cs.CoreV1().Pods(namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: "snapshot", Follow: true}).Stream(ctx)
		}
		return nil, errors.New("the snapshot job has not started")
	}
}
