// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package config is the supervisor telemetry configuration (spec §10.3).
// It has no dependencies so the controller can render it into the
// host-key Secret without linking the agent.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Paths inside the user container (the host-key Secret is mounted at
// /etc/devpod).
const (
	ConfigPath = "/etc/devpod/telemetry.json"
	TokenPath  = "/etc/devpod/ingest-token"
)

// ErrNoConfig means telemetry is not configured for this DevPod.
var ErrNoConfig = errors.New("telemetry config not found")

// Config controls the telemetry agent.
type Config struct {
	IngestURL         string
	MetricsInterval   time.Duration
	ProcScanInterval  time.Duration
	SocketMapInterval time.Duration
	TopN              int
	GPU               bool
	RDMA              bool
	ProcessTracking   bool
	Hooks             bool
	HostNetwork       bool
	BufferBytes       int64
}

// DefaultConfig is spec §10.3's defaults.
func DefaultConfig() Config {
	return Config{
		MetricsInterval:   time.Second,
		ProcScanInterval:  500 * time.Millisecond,
		SocketMapInterval: 5 * time.Second,
		TopN:              20,
		GPU:               true,
		RDMA:              true,
		ProcessTracking:   true,
		Hooks:             true,
		BufferBytes:       32 << 20,
	}
}

// file is the on-disk form; pointers distinguish "absent" from zero.
type file struct {
	IngestURL           string `json:"ingestURL"`
	MetricsIntervalMs   *int64 `json:"metricsIntervalMs,omitempty"`
	ProcScanIntervalMs  *int64 `json:"procScanIntervalMs,omitempty"`
	SocketMapIntervalMs *int64 `json:"socketMapIntervalMs,omitempty"`
	TopN                *int   `json:"topN,omitempty"`
	GPU                 *bool  `json:"gpu,omitempty"`
	RDMA                *bool  `json:"rdma,omitempty"`
	ProcessTracking     *bool  `json:"processTracking,omitempty"`
	Hooks               *bool  `json:"hooks,omitempty"`
	HostNetwork         *bool  `json:"hostNetwork,omitempty"`
	BufferBytes         *int64 `json:"bufferBytes,omitempty"`
}

// MarshalJSON writes every field explicitly.
func (c Config) MarshalJSON() ([]byte, error) {
	ms := func(d time.Duration) *int64 { v := d.Milliseconds(); return &v }
	return json.Marshal(file{
		IngestURL:           c.IngestURL,
		MetricsIntervalMs:   ms(c.MetricsInterval),
		ProcScanIntervalMs:  ms(c.ProcScanInterval),
		SocketMapIntervalMs: ms(c.SocketMapInterval),
		TopN:                &c.TopN,
		GPU:                 &c.GPU,
		RDMA:                &c.RDMA,
		ProcessTracking:     &c.ProcessTracking,
		Hooks:               &c.Hooks,
		HostNetwork:         &c.HostNetwork,
		BufferBytes:         &c.BufferBytes,
	})
}

// UnmarshalJSON starts from DefaultConfig and applies present fields.
func (c *Config) UnmarshalJSON(b []byte) error {
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	out := DefaultConfig()
	out.IngestURL = f.IngestURL
	dur := func(p *int64, d *time.Duration) {
		if p != nil && *p > 0 {
			*d = time.Duration(*p) * time.Millisecond
		}
	}
	dur(f.MetricsIntervalMs, &out.MetricsInterval)
	dur(f.ProcScanIntervalMs, &out.ProcScanInterval)
	dur(f.SocketMapIntervalMs, &out.SocketMapInterval)
	if f.TopN != nil && *f.TopN > 0 {
		out.TopN = *f.TopN
	}
	for _, b := range []struct {
		p *bool
		d *bool
	}{{f.GPU, &out.GPU}, {f.RDMA, &out.RDMA}, {f.ProcessTracking, &out.ProcessTracking}, {f.Hooks, &out.Hooks}, {f.HostNetwork, &out.HostNetwork}} {
		if b.p != nil {
			*b.d = *b.p
		}
	}
	if f.BufferBytes != nil && *f.BufferBytes > 0 {
		out.BufferBytes = *f.BufferBytes
	}
	*c = out
	return nil
}

// LoadConfig reads path; a missing file is ErrNoConfig.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, ErrNoConfig
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
