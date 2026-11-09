// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package agent is the supervisor-side telemetry agent (spec §5–§6).
package agent

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
)

// unlimitedV1 is the smallest value cgroup v1 uses to mean "no limit".
const unlimitedV1 = uint64(1) << 62

// Cgroup reads the container's own cgroup (v2 unified or v1 split).
type Cgroup struct {
	v2                                  bool
	cpu, cpuacct, cpuset, mem, io, pids string
}

// CgroupSample is one container-level sample (spec §6.1). Rates are
// zero on the first sample (no previous counters).
type CgroupSample struct {
	CPUCores      float64
	CPULimit      float64 // 0 = unlimited
	ThrottledUsec uint64
	Cpuset        string
	MemWorkingSet uint64
	MemAnon       uint64
	MemFile       uint64
	MemShmem      uint64
	MemLimit      uint64 // 0 = unlimited
	OOMKills      uint64
	IOReadBps     float64
	IOWriteBps    float64
	IOReadIops    float64
	IOWriteIops   float64
	Pids          uint64

	at                       time.Time
	usageUsec                uint64
	ioR, ioW, ioRops, ioWops uint64
}

// OpenCgroup detects the layout under root (normally /sys/fs/cgroup).
func OpenCgroup(root string) (*Cgroup, error) {
	if exists(filepath.Join(root, "cgroup.controllers")) {
		return &Cgroup{v2: true, cpu: root, cpuacct: root, cpuset: root, mem: root, io: root, pids: root}, nil
	}
	pick := func(names ...string) string {
		for _, n := range names {
			if p := filepath.Join(root, n); exists(p) {
				return p
			}
		}
		return ""
	}
	c := &Cgroup{
		cpu:     pick("cpu,cpuacct", "cpu", "cpuacct,cpu"),
		cpuacct: pick("cpu,cpuacct", "cpuacct", "cpuacct,cpu"),
		cpuset:  pick("cpuset"),
		mem:     pick("memory"),
		io:      pick("blkio"),
		pids:    pick("pids"),
	}
	if c.cpu == "" && c.mem == "" {
		return nil, errors.New("no cgroup v1 or v2 hierarchy under " + root)
	}
	return c, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// read returns a file's trimmed content; a missing directory or file is
// "absent" (empty string), never an error (spec §11.2).
// readBufs: cgroup files (memory.stat is the largest, ~2 KiB) are read
// several times a second.
var readBufs = sync.Pool{New: func() any { b := make([]byte, 16<<10); return &b }}

func read(dir, name string) string {
	if dir == "" {
		return ""
	}
	bp := readBufs.Get().(*[]byte)
	defer readBufs.Put(bp)
	b, ok := procfs.ReadSmall(filepath.Join(dir, name), *bp)
	if !ok {
		return ""
	}
	return string(bytes.TrimSpace(b))
}

func readUint(dir, name string) uint64 {
	v, _ := strconv.ParseUint(read(dir, name), 10, 64)
	return v
}

// keyed parses "key value" lines.
func keyed(dir, name string) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(read(dir, name)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				out[f[0]] = v
			}
		}
	}
	return out
}

// Sample reads the cgroup now; prev (may be nil) supplies the counters
// rates are computed from.
func (c *Cgroup) Sample(prev *CgroupSample, now time.Time) (CgroupSample, error) {
	s := CgroupSample{at: now}
	if c.v2 {
		c.sampleV2(&s)
	} else {
		c.sampleV1(&s)
	}
	s.Pids = readUint(c.pids, "pids.current")
	if prev != nil && !prev.at.IsZero() {
		if dt := now.Sub(prev.at).Seconds(); dt > 0 {
			rate := func(cur, old uint64) float64 {
				if cur < old {
					return 0 // counter reset
				}
				return float64(cur-old) / dt
			}
			s.CPUCores = rate(s.usageUsec, prev.usageUsec) / 1e6
			s.IOReadBps = rate(s.ioR, prev.ioR)
			s.IOWriteBps = rate(s.ioW, prev.ioW)
			s.IOReadIops = rate(s.ioRops, prev.ioRops)
			s.IOWriteIops = rate(s.ioWops, prev.ioWops)
		}
	}
	return s, nil
}

func (c *Cgroup) sampleV2(s *CgroupSample) {
	st := keyed(c.cpu, "cpu.stat")
	s.usageUsec, s.ThrottledUsec = st["usage_usec"], st["throttled_usec"]
	if f := strings.Fields(read(c.cpu, "cpu.max")); len(f) == 2 && f[0] != "max" {
		q, _ := strconv.ParseFloat(f[0], 64)
		p, _ := strconv.ParseFloat(f[1], 64)
		if p > 0 {
			s.CPULimit = q / p
		}
	}
	s.Cpuset = read(c.cpuset, "cpuset.cpus.effective")
	cur := readUint(c.mem, "memory.current")
	if m := read(c.mem, "memory.max"); m != "max" {
		s.MemLimit, _ = strconv.ParseUint(m, 10, 64)
	}
	ms := keyed(c.mem, "memory.stat")
	s.MemAnon, s.MemFile, s.MemShmem = ms["anon"], ms["file"], ms["shmem"]
	s.MemWorkingSet = sub(cur, ms["inactive_file"])
	s.OOMKills = keyed(c.mem, "memory.events")["oom_kill"]
	sc := bufio.NewScanner(strings.NewReader(read(c.io, "io.stat")))
	for sc.Scan() {
		for _, kv := range strings.Fields(sc.Text())[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, _ := strconv.ParseUint(v, 10, 64)
			switch k {
			case "rbytes":
				s.ioR += n
			case "wbytes":
				s.ioW += n
			case "rios":
				s.ioRops += n
			case "wios":
				s.ioWops += n
			}
		}
	}
}

func (c *Cgroup) sampleV1(s *CgroupSample) {
	s.usageUsec = readUint(c.cpuacct, "cpuacct.usage") / 1000
	s.ThrottledUsec = keyed(c.cpu, "cpu.stat")["throttled_time"] / 1000
	if q, err := strconv.ParseFloat(read(c.cpu, "cpu.cfs_quota_us"), 64); err == nil && q > 0 {
		if p, _ := strconv.ParseFloat(read(c.cpu, "cpu.cfs_period_us"), 64); p > 0 {
			s.CPULimit = q / p
		}
	}
	s.Cpuset = read(c.cpuset, "cpuset.effective_cpus")
	if s.Cpuset == "" {
		s.Cpuset = read(c.cpuset, "cpuset.cpus")
	}
	if l := readUint(c.mem, "memory.limit_in_bytes"); l < unlimitedV1 {
		s.MemLimit = l
	}
	ms := keyed(c.mem, "memory.stat")
	s.MemAnon, s.MemFile, s.MemShmem = ms["rss"], ms["cache"], ms["shmem"]
	s.MemWorkingSet = sub(readUint(c.mem, "memory.usage_in_bytes"), ms["total_inactive_file"])
	s.OOMKills = keyed(c.mem, "memory.oom_control")["oom_kill"]
	sumOp := func(name string) (r, w uint64) {
		sc := bufio.NewScanner(strings.NewReader(read(c.io, name)))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) != 3 {
				continue
			}
			n, _ := strconv.ParseUint(f[2], 10, 64)
			switch f[1] {
			case "Read":
				r += n
			case "Write":
				w += n
			}
		}
		return
	}
	s.ioR, s.ioW = sumOp("blkio.throttle.io_service_bytes")
	s.ioRops, s.ioWops = sumOp("blkio.throttle.io_serviced")
}

func sub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
