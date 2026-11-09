// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadConfig_DefaultsAndOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "telemetry.json")
	if err := os.WriteFile(p, []byte(`{"topN":5,"hostNetwork":true,"gpu":false,"ingestURL":"http://x/ingest"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.TopN != 5 || !c.HostNetwork || c.GPU || c.IngestURL != "http://x/ingest" {
		t.Fatalf("overrides lost: %+v", c)
	}
	if c.MetricsInterval != time.Second || c.ProcScanInterval != 500*time.Millisecond || c.SocketMapInterval != 5*time.Second ||
		c.BufferBytes != 33554432 || !c.RDMA || !c.ProcessTracking || !c.Hooks {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestLoadConfig_Missing(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json")); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("want ErrNoConfig, got %v", err)
	}
}

func TestConfig_MarshalRoundTrip(t *testing.T) {
	c := DefaultConfig()
	c.IngestURL, c.TopN, c.HostNetwork, c.MetricsInterval = "http://y", 9, true, 2*time.Second
	c.PVCMounts = []string{"/root"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"ingestURL", "metricsIntervalMs", "procScanIntervalMs", "socketMapIntervalMs", "topN", "gpu", "rdma", "processTracking", "hooks", "hostNetwork", "bufferBytes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing json key %q in %s", k, b)
		}
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, c) {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}
