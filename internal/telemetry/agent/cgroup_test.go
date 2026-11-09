// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTree creates files (relative path → content) under a temp root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCgroup_V2(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cgroup.controllers":    "cpuset cpu io memory pids",
		"cpu.stat":              "usage_usec 1000000\nuser_usec 800000\nnr_throttled 3\nthrottled_usec 4000\n",
		"cpu.max":               "200000 100000\n",
		"cpuset.cpus.effective": "0-7\n",
		"memory.current":        "1000\n",
		"memory.max":            "max\n",
		"memory.stat":           "anon 600\nfile 300\nshmem 10\ninactive_file 200\n",
		"memory.events":         "low 0\nhigh 0\nmax 2\noom 1\noom_kill 1\n",
		"io.stat":               "8:0 rbytes=1000 wbytes=2000 rios=10 wios=20 dbytes=0 dios=0\n8:16 rbytes=1000 wbytes=0 rios=5 wios=0\n",
		"pids.current":          "37\n",
	})
	cg, err := OpenCgroup(root)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(100, 0)
	first, err := cg.Sample(nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, root, "cpu.stat", "usage_usec 1500000\nthrottled_usec 4000\n")
	writeTreeFile(t, root, "io.stat", "8:0 rbytes=3000 wbytes=2000 rios=30 wios=20\n8:16 rbytes=1000 wbytes=0 rios=5 wios=0\n")
	s, err := cg.Sample(&first, t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUCores != 0.5 || s.CPULimit != 2 || s.Cpuset != "0-7" || s.ThrottledUsec != 4000 {
		t.Fatalf("cpu: %+v", s)
	}
	if s.MemWorkingSet != 800 || s.MemAnon != 600 || s.MemFile != 300 || s.MemShmem != 10 || s.MemLimit != 0 || s.OOMKills != 1 {
		t.Fatalf("mem: %+v", s)
	}
	if s.IOReadBps != 2000 || s.IOWriteBps != 0 || s.IOReadIops != 20 || s.Pids != 37 {
		t.Fatalf("io/pids: %+v", s)
	}
	if first.CPUCores != 0 {
		t.Fatalf("first sample has no rate: %v", first.CPUCores)
	}
}

func writeTreeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCgroup_V1AndMissingFiles(t *testing.T) {
	v1 := writeTree(t, map[string]string{
		"cpu,cpuacct/cpuacct.usage":     "2000000000\n", // ns
		"cpu,cpuacct/cpu.cfs_quota_us":  "-1\n",
		"cpu,cpuacct/cpu.cfs_period_us": "100000\n",
		"cpu,cpuacct/cpu.stat":          "nr_periods 0\nnr_throttled 0\nthrottled_time 5000000\n",
		"memory/memory.usage_in_bytes":  "5000\n",
		"memory/memory.limit_in_bytes":  "9223372036854771712\n",
		"memory/memory.stat":            "rss 3000\ncache 1500\nshmem 0\ntotal_inactive_file 1000\n",
		"cpuset/cpuset.effective_cpus":  "2-3\n",
		"pids/pids.current":             "9\n",
	})
	cg, err := OpenCgroup(v1)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(100, 0)
	first, err := cg.Sample(nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, v1, "cpu,cpuacct/cpuacct.usage", "3000000000\n")
	s, err := cg.Sample(&first, t0.Add(time.Second))
	if err != nil {
		t.Fatalf("v1 without blkio must not fail: %v", err)
	}
	if s.CPUCores != 1 || s.CPULimit != 0 || s.ThrottledUsec != 5000 || s.Cpuset != "2-3" {
		t.Fatalf("v1 cpu: %+v", s)
	}
	if s.MemWorkingSet != 4000 || s.MemLimit != 0 || s.MemAnon != 3000 || s.MemFile != 1500 || s.IOReadBps != 0 || s.Pids != 9 {
		t.Fatalf("v1 mem/io: %+v", s)
	}

	v2 := writeTree(t, map[string]string{
		"cgroup.controllers": "cpu memory",
		"cpu.stat":           "usage_usec 10\n",
		"cpu.max":            "max 100000\n",
		"memory.current":     "100\n",
		"memory.max":         "1048576\n",
	})
	cg2, err := OpenCgroup(v2)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := cg2.Sample(nil, t0)
	if err != nil {
		t.Fatalf("v2 without io.stat / memory.events / memory.stat must not fail: %v", err)
	}
	if s2.CPULimit != 0 || s2.MemLimit != 1048576 || s2.MemWorkingSet != 100 || s2.OOMKills != 0 {
		t.Fatalf("v2 partial: %+v", s2)
	}
}
