// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"testing"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

func TestInfo_OSReleaseKernelLimits(t *testing.T) {
	root := writeTree(t, map[string]string{
		"etc/os-release":            "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\nID=ubuntu\n",
		"proc/sys/kernel/osrelease": "6.8.0-45-generic\n",
	})
	cgRoot := writeTree(t, map[string]string{"cgroup.controllers": "cpu", "cpu.max": "800000 100000", "cpuset.cpus.effective": "0-7", "memory.max": "34359738368"})
	cg, _ := OpenCgroup(cgRoot)
	gpus := []wire.GPU{{Index: 0, UUID: "GPU-a", Name: "A100", MemTotal: 81920}}
	devs := []RDMADev{{IBDev: "mlx5_0", Ports: []int{1}, Scope: "vf_exclusive"}}
	in := CollectInfo(root, cg, gpus, devs, map[string]string{"gpu": "ok", "rdma": "disabled: boom"})
	if in.OS != "Ubuntu 24.04 LTS" || in.Kernel != "6.8.0-45-generic" || in.CPULimit != 8 || in.Cpuset != "0-7" || in.MemLimit != 34359738368 {
		t.Fatalf("%+v", in)
	}
	if len(in.GPUs) != 1 || in.GPUs[0].UUID != "GPU-a" || len(in.RDMA) != 1 || in.RDMA[0].Scope != "vf_exclusive" || in.Collectors["rdma"] != "disabled: boom" || in.AgentVersion == "" {
		t.Fatalf("%+v", in)
	}
}

func TestSnapshot_ListenPortsAndTop(t *testing.T) {
	root := writeTree(t, map[string]string{
		"net/tcp": "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
			"   0: 00000000:22B8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 777 1\n" +
			"   1: 0100007F:1F90 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 778 1\n",
	})
	procs := map[procfs.PID]*procfs.Proc{}
	cpu := map[procfs.PID]float64{}
	for i := 1; i <= 25; i++ {
		id := procfs.PID{Pid: i, Start: uint64(i)}
		st := "S"
		if i == 25 {
			st = "Z"
		}
		procs[id] = &procfs.Proc{ID: id, Exe: fmt.Sprintf("/bin/p%d", i), RSS: uint64(i) << 20, Threads: 2, State: st}
		cpu[id] = float64(i) / 10
	}
	sm := procfs.NewSocketMap(map[uint64]procfs.PID{777: {Pid: 30, Start: 30}})
	s := CollectSnapshot(SnapshotInput{ProcRoot: root, Procs: procs, CPU: cpu, Sockets: sm})
	if len(s.Listen) != 1 || s.Listen[0].Port != 8888 || s.Listen[0].Pid != 30 {
		t.Fatalf("listen: %+v", s.Listen)
	}
	if len(s.TopCPU) != 20 || s.TopCPU[0].Pid != 25 || len(s.TopRSS) != 20 || s.TopRSS[0].Pid != 25 {
		t.Fatalf("top: %d %d", len(s.TopCPU), len(s.TopRSS))
	}
	if s.Procs != 25 || s.Threads != 50 || s.Zombies != 1 {
		t.Fatalf("counts: %+v", s)
	}
}
