// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"
)

// Container-side telemetry rows (spec §8.2, migration 0002).

type ContainerMetricRow struct {
	TS            time.Time `ch:"ts"`
	DevPodUID     string    `ch:"devpod_uid"`
	BootID        string    `ch:"boot_id"`
	CPU           float32   `ch:"cpu"`
	CPULimit      float32   `ch:"cpu_limit"`
	ThrottledUsec uint64    `ch:"throttled_usec"`
	MemWS         uint64    `ch:"mem_ws"`
	MemAnon       uint64    `ch:"mem_anon"`
	MemFile       uint64    `ch:"mem_file"`
	MemShmem      uint64    `ch:"mem_shmem"`
	MemLimit      uint64    `ch:"mem_limit"`
	OOMKills      uint64    `ch:"oom_kills"`
	IOR           float32   `ch:"io_r"`
	IOW           float32   `ch:"io_w"`
	IORIops       float32   `ch:"io_r_iops"`
	IOWIops       float32   `ch:"io_w_iops"`
	NetRx         float32   `ch:"net_rx"`
	NetTx         float32   `ch:"net_tx"`
	NetTCPRx      float32   `ch:"net_tcp_rx"`
	NetTCPTx      float32   `ch:"net_tcp_tx"`
	NetOtherRx    float32   `ch:"net_other_rx"`
	NetOtherTx    float32   `ch:"net_other_tx"`
	NetRxPkts     float32   `ch:"net_rx_pkts"`
	NetTxPkts     float32   `ch:"net_tx_pkts"`
	NetErrs       float32   `ch:"net_errs"`
	NetDrops      float32   `ch:"net_drops"`
	NetKind       string    `ch:"net_kind"`
	Pids          uint32    `ch:"pids"`
}

type GPUMetricRow struct {
	TS        time.Time `ch:"ts"`
	DevPodUID string    `ch:"devpod_uid"`
	GPUUUID   string    `ch:"gpu_uuid"`
	Index     uint8     `ch:"idx"`
	Name      string    `ch:"name"`
	Util      float32   `ch:"util"`
	MemUsed   float32   `ch:"mem_used"`
	MemTotal  float32   `ch:"mem_total"`
	Power     float32   `ch:"power"`
	Temp      float32   `ch:"temp"`
	SMClock   float32   `ch:"sm_clock"`
	ProcCount uint16    `ch:"proc_count"`
	Shared    uint8     `ch:"shared"`
}

type RDMAMetricRow struct {
	TS        time.Time         `ch:"ts"`
	DevPodUID string            `ch:"devpod_uid"`
	IBDev     string            `ch:"ibdev"`
	Port      uint8             `ch:"port"`
	TxBps     float32           `ch:"tx_bps"`
	RxBps     float32           `ch:"rx_bps"`
	TxPps     float32           `ch:"tx_pps"`
	RxPps     float32           `ch:"rx_pps"`
	Health    map[string]uint64 `ch:"health"`
	Scope     string            `ch:"scope"`
}

type ProcMetricRow struct {
	TS        time.Time `ch:"ts"`
	DevPodUID string    `ch:"devpod_uid"`
	BootID    string    `ch:"boot_id"`
	Pid       uint32    `ch:"pid"`
	StartTime uint64    `ch:"starttime"`
	CPU       float32   `ch:"cpu"`
	RSS       uint64    `ch:"rss"`
	PSS       uint64    `ch:"pss"`
	IOR       float32   `ch:"io_r"`
	IOW       float32   `ch:"io_w"`
	TCPRx     float32   `ch:"tcp_rx"`
	TCPTx     float32   `ch:"tcp_tx"`
	GPU       []uint8   `ch:"gpu"`
	RDMA      uint8     `ch:"rdma"`
	SessionID string    `ch:"session_id"`
	Peer      string    `ch:"peer"`
}

// ProcessRow is one process's lifetime record; attribution backfill
// re-inserts it with a higher Version (spec §8.3).
type ProcessRow struct {
	DevPodUID   string     `ch:"devpod_uid" json:"devpod_uid"`
	BootID      string     `ch:"boot_id" json:"boot_id"`
	Pid         uint32     `ch:"pid" json:"pid"`
	StartTime   uint64     `ch:"starttime" json:"starttime"`
	PPid        uint32     `ch:"ppid" json:"ppid"`
	Exe         string     `ch:"exe" json:"exe"`
	Cmdline     string     `ch:"cmdline" json:"cmdline"`
	UID         uint32     `ch:"uid" json:"uid"`
	FirstSeen   time.Time  `ch:"first_seen" json:"first_seen"`
	ExitSeen    *time.Time `ch:"exit_seen" json:"exit_seen,omitempty"`
	Source      string     `ch:"source" json:"source"`
	SessionID   string     `ch:"session_id" json:"session_id"`
	User        string     `ch:"user" json:"user"`
	Peer        string     `ch:"peer" json:"peer"`
	Attribution string     `ch:"attribution" json:"attribution"`
	Version     uint64     `ch:"version" json:"version"`
}

type SSHConnRow struct {
	DevPodUID string     `ch:"devpod_uid" json:"devpod_uid"`
	BootID    string     `ch:"boot_id" json:"boot_id"`
	Pid       uint32     `ch:"pid" json:"pid"`
	StartTime uint64     `ch:"starttime" json:"starttime"`
	PeerAddr  string     `ch:"peer_addr" json:"peer_addr"`
	FirstSeen time.Time  `ch:"first_seen" json:"first_seen"`
	ClosedAt  *time.Time `ch:"closed_at" json:"closed_at,omitempty"`
	SessionID string     `ch:"session_id" json:"session_id"`
	User      string     `ch:"user" json:"user"`
	Version   uint64     `ch:"version" json:"version"`
}

type ContainerInfoRow struct {
	TS        time.Time `ch:"ts" json:"ts"`
	DevPodUID string    `ch:"devpod_uid" json:"devpod_uid"`
	BootID    string    `ch:"boot_id" json:"boot_id"`
	Info      string    `ch:"info" json:"info"` // wire.Info JSON
	Version   uint64    `ch:"version" json:"version"`
}

type SnapshotRow struct {
	TS        time.Time `ch:"ts" json:"ts"`
	DevPodUID string    `ch:"devpod_uid" json:"devpod_uid"`
	Data      string    `ch:"data" json:"data"` // wire.Snapshot JSON
}

// TelemetryBatch groups the rows of one merged ingest write.
type TelemetryBatch struct {
	Containers []ContainerMetricRow
	GPUs       []GPUMetricRow
	RDMAs      []RDMAMetricRow
	Procs      []ProcMetricRow
	ProcRows   []ProcessRow
	SSHConns   []SSHConnRow
	Commands   []CommandRow
	Infos      []ContainerInfoRow
	Snapshots  []SnapshotRow
	Alerts     []AlertRow
}

// Empty reports whether the batch has no rows.
func (b TelemetryBatch) Empty() bool {
	return len(b.Containers)+len(b.GPUs)+len(b.RDMAs)+len(b.Procs)+len(b.ProcRows)+len(b.SSHConns)+
		len(b.Commands)+len(b.Infos)+len(b.Snapshots)+len(b.Alerts) == 0
}

// InsertTelemetry writes one insert per non-empty table, each carrying
// insert_deduplication_token = token/table.
func (c *CH) InsertTelemetry(ctx context.Context, b TelemetryBatch, token string) error {
	return errors.Join(
		insertRows(ctx, c, "metrics_container_1s", token, b.Containers),
		insertRows(ctx, c, "metrics_gpu_1s", token, b.GPUs),
		insertRows(ctx, c, "metrics_rdma_1s", token, b.RDMAs),
		insertRows(ctx, c, "metrics_process_1s", token, b.Procs),
		insertRows(ctx, c, "processes", token, b.ProcRows),
		insertRows(ctx, c, "ssh_conns", token, b.SSHConns),
		insertRows(ctx, c, "commands", token, b.Commands),
		insertRows(ctx, c, "container_info", token, b.Infos),
		insertRows(ctx, c, "container_snapshots", token, b.Snapshots),
		insertRows(ctx, c, "alerts", token, b.Alerts),
	)
}

// CommandAttribution maps a process attribution rule (tree,
// ssh_connection, sid_tty, inferred, …) to the commands table's
// exact / inferred / unattributed.
func CommandAttribution(how string) string {
	switch how {
	case "tree", "ssh_connection", "sid_tty":
		return "exact"
	case "inferred":
		return "inferred"
	default:
		return "unattributed"
	}
}
