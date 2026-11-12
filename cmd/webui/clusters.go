// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// clusterFlags: this console as the one console of several clusters.
type clusterFlags struct {
	enabled   *bool
	instance  *string
	selector  *string
	namespace *string
}

func addClusterFlags(fs *flag.FlagSet) *clusterFlags {
	host, _ := os.Hostname()
	return &clusterFlags{
		enabled: fs.Bool("clusters", false,
			"manage other clusters: their agents connect to this console at /agent/connect"),
		instance: fs.String("instance", envOr("POD_NAME", host),
			"this console instance's name among its replicas (default: $POD_NAME, else the hostname)"),
		selector: fs.String("instance-selector", "",
			"label selector of the console's Pods, e.g. app.kubernetes.io/component=webui; agents connect to every one of them. Empty: this instance alone"),
		namespace: fs.String("instance-namespace", envOr("POD_NAMESPACE", "devpod-system"),
			"namespace of the console's Pods (default: $POD_NAMESPACE)"),
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// build returns the registry, or nil when the console manages only the
// cluster it runs in.
func (f *clusterFlags) build(ctx context.Context, hub client.Client, reader client.Reader, scheme *runtime.Scheme, devpodNamespace string) (*clusters.Registry, error) {
	if !*f.enabled {
		return nil, nil
	}
	if *f.instance == "" {
		return nil, fmt.Errorf("--clusters: --instance is empty")
	}
	instances := func() []string { return []string{*f.instance} }
	if *f.selector != "" {
		sel, err := labels.Parse(*f.selector)
		if err != nil {
			return nil, fmt.Errorf("--instance-selector: %w", err)
		}
		instances = podNames(reader, *f.namespace, sel, *f.instance)
	}
	warm, byObject := clusterCache(devpodNamespace)
	r := clusters.New(clusters.Options{
		Hub: hub, Reader: reader, Instance: *f.instance, Instances: instances,
		Scheme: scheme, Indexes: webui.RegisterIndexes, Warm: warm, ByObject: byObject,
	})
	go r.Run(ctx)
	return r, nil
}

// clusterCache is what the console keeps of a cluster: the kinds whose
// informers start with the cluster's clients (there to read when it is
// away), and the namespace each namespaced kind is read in. The agent may
// list a namespaced kind in the DevPod namespace only: one read across
// namespaces would be refused, and the cache would never sync.
func clusterCache(devpodNamespace string) ([]client.Object, map[client.Object]cache.ByObject) {
	inNS := cache.ByObject{Namespaces: map[string]cache.Config{devpodNamespace: {}}}
	warm := []client.Object{
		&devpodv1alpha1.DevPod{}, &corev1.Pod{}, &devpodv1alpha1.DevPodSnapshot{}, // in the DevPod namespace
		&corev1.Node{}, &devpodv1alpha1.Reservation{}, // cluster-scoped
	}
	byObject := map[client.Object]cache.ByObject{
		&devpodv1alpha1.DevPod{}: inNS, &corev1.Pod{}: inNS, &corev1.Event{}: inNS, &devpodv1alpha1.DevPodSnapshot{}: inNS,
	}
	return warm, byObject
}

// podNames lists the console's running Pods, asked at most every few
// seconds. This instance is always among them; when the list cannot be
// read, the last one known stands.
func podNames(reader client.Reader, ns string, sel labels.Selector, self string) func() []string {
	var (
		mu   sync.Mutex
		last = []string{self}
		at   time.Time
	)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < 5*time.Second {
			return last
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
			return last
		}
		names := []string{self}
		for _, p := range pods.Items {
			if p.Name != self && p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
				names = append(names, p.Name)
			}
		}
		slices.Sort(names)
		last, at = names, time.Now()
		return last
	}
}
