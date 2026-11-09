// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/webui/leader"
)

// telemetryFlags configure the phase-2 live data layer (spec §5.10).
type telemetryFlags struct {
	ingestListen    string
	ingestKeyFile   string
	leaderNamespace string
	ingestService   string
}

func addTelemetryFlags(fs *flag.FlagSet) *telemetryFlags {
	tf := &telemetryFlags{}
	fs.StringVar(&tf.ingestListen, "ingest-listen", fmt.Sprintf(":%d", render.IngestPort), "telemetry ingest + internal live listener")
	fs.StringVar(&tf.ingestKeyFile, "ingest-key-file", "", "shared telemetry ingest HMAC key; empty = telemetry off")
	fs.StringVar(&tf.leaderNamespace, "leader-election-namespace", "devpod-system", "namespace of the webui leader Lease and the ingest EndpointSlice")
	fs.StringVar(&tf.ingestService, "ingest-service", "devpod-webui-ingest", "selectorless Service the leader publishes itself behind")
	return tf
}

func (tf *telemetryFlags) enabled() bool { return tf.ingestKeyFile != "" }

func (tf *telemetryFlags) key() ([]byte, error) {
	b, err := os.ReadFile(tf.ingestKeyFile)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) < 32 {
		return nil, fmt.Errorf("ingest key %s: need >= 32 bytes", tf.ingestKeyFile)
	}
	return b, nil
}

// newElector builds this replica's elector; POD_NAME / POD_IP come from
// the downward API.
func (tf *telemetryFlags) newElector(cfg *rest.Config) (*leader.Elector, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	id := os.Getenv("POD_NAME")
	if id == "" {
		id, _ = os.Hostname()
	}
	return &leader.Elector{Client: cs, Namespace: tf.leaderNamespace, Name: "devpod-webui-leader", Identity: id}, nil
}

// publishSelf points the ingest Service at this pod when it leads.
func (tf *telemetryFlags) publishSelf(ctx context.Context, c client.Client) {
	name, ip := os.Getenv("POD_NAME"), os.Getenv("POD_IP")
	if name == "" || ip == "" {
		slog.Error("telemetry leader: POD_NAME / POD_IP unset; ingest endpoint not published")
		return
	}
	if err := leader.PublishIngestEndpoint(ctx, c, tf.leaderNamespace, tf.ingestService, name, ip, render.IngestPort); err != nil {
		slog.Error("telemetry leader: publish ingest endpoint", "err", err)
		return
	}
	slog.Info("telemetry leader: ingest endpoint published", "pod", name, "ip", ip)
}
