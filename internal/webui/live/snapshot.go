// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package live is the leader's in-memory telemetry state (spec §5.10):
// the latest sample per DevPod and the per-node / per-user rollups the
// console UI streams every second.
package live

import "time"

// GPULive is one card of a DevPod (UI spec §6.1).
type GPULive struct {
	Index    int     `json:"index"`
	UUID     string  `json:"uuid"`
	Name     string  `json:"name"`
	Util     float64 `json:"util"`
	MemUsed  float64 `json:"memUsed"`  // MiB
	MemTotal float64 `json:"memTotal"` // MiB
	Power    float64 `json:"power"`    // W
}

// SessionLive is one user connected to a DevPod.
type SessionLive struct {
	User string `json:"user"`
}

// DevPodLive is the per-DevPod live record; nil numbers mean "no data"
// (no telemetry, or the last sample is older than the staleness window).
type DevPodLive struct {
	CPU      *float64      `json:"cpu"`
	CPULimit float64       `json:"cpuLimit"`
	Mem      *float64      `json:"mem"`
	MemLimit float64       `json:"memLimit"`
	GPU      []GPULive     `json:"gpu"`
	NetRx    *float64      `json:"netRx"`
	NetTx    *float64      `json:"netTx"`
	NetKind  string        `json:"netKind"`
	RDMARx   *float64      `json:"rdmaRx"`
	RDMATx   *float64      `json:"rdmaTx"`
	RDMAKind string        `json:"rdmaKind"` // vf | shared | ""
	IOR      *float64      `json:"ioR"`
	IOW      *float64      `json:"ioW"`
	Sessions []SessionLive `json:"sessions"`
	// Idle: the leader's idle definition holds (no session for 24 h, CPU
	// and GPU averages under 5 %; UI spec §5.3).
	Idle bool `json:"idle"`
}

// CardLive is one GPU slot of a node.
type CardLive struct {
	Index  int     `json:"index"`
	UUID   string  `json:"uuid"`
	DevPod string  `json:"devpod"`
	Owner  string  `json:"owner"`
	Util   float64 `json:"util"`
}

// NodeLive is a node's rollup. Allocated counts DevPod requests only.
type NodeLive struct {
	// Cluster: the cluster it is in, in a console of several (a name can
	// be in two).
	Cluster        string            `json:"cluster,omitempty"`
	Name           string            `json:"name"`
	Ready          bool              `json:"ready"`
	Labels         map[string]string `json:"labels,omitempty"`
	CPUAllocatable float64           `json:"cpuAllocatable"`
	CPUAllocated   float64           `json:"cpuAllocated"`
	CPUUsed        float64           `json:"cpuUsed"`
	MemAllocatable float64           `json:"memAllocatable"`
	MemAllocated   float64           `json:"memAllocated"`
	MemUsed        float64           `json:"memUsed"`
	GPUModel       string            `json:"gpuModel,omitempty"`
	GPUTotal       int               `json:"gpuTotal"`
	GPUUsed        int               `json:"gpuUsed"`
	Cards          []CardLive        `json:"cards,omitempty"`
	DevPods        int               `json:"devpods"`
}

// UserLive is an owner's rollup.
type UserLive struct {
	User    string  `json:"user"`
	DevPods int     `json:"devpods"`
	Running int     `json:"running"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	GPU     int     `json:"gpu"`
}

// KPI is the overview's top row (UI spec §5.3).
type KPI struct {
	CPUUsed        float64 `json:"cpuUsed"`
	CPUAllocatable float64 `json:"cpuAllocatable"`
	MemUsed        float64 `json:"memUsed"`
	MemAllocatable float64 `json:"memAllocatable"`
	GPUUsed        int     `json:"gpuUsed"`
	GPUTotal       int     `json:"gpuTotal"`
	Running        int     `json:"running"`
	Pending        int     `json:"pending"`
	Failed         int     `json:"failed"`
	Stopped        int     `json:"stopped"`
	Sessions       int     `json:"sessions"`
	SessionUsers   int     `json:"sessionUsers"`
	RDMA           float64 `json:"rdma"`
}

// AttentionItem is one "needs attention" row (UI spec §5.3).
type AttentionItem struct {
	Cluster string    `json:"cluster,omitempty"`
	Kind    string    `json:"kind"` // alert | idle_gpu | pending_long | quota_high
	UID     string    `json:"uid,omitempty"`
	DevPod  string    `json:"devpod,omitempty"`
	Owner   string    `json:"owner,omitempty"`
	Since   time.Time `json:"since"`
	Detail  string    `json:"detail,omitempty"`
}

// Snapshot is the per-second state pushed to browsers and followers.
type Snapshot struct {
	TS        time.Time             `json:"ts"`
	DevPods   map[string]DevPodLive `json:"devpods"` // by DevPod UID
	Nodes     []NodeLive            `json:"nodes"`
	Users     []UserLive            `json:"users"`
	KPI       KPI                   `json:"kpi"`
	Attention []AttentionItem       `json:"attention"`
	// Clusters: each cluster's own numbers, in a console of several (the
	// fields above are the console's; nodes and attention say their cluster).
	Clusters map[string]ClusterLive `json:"clusters,omitempty"`
}

// ClusterLive is one cluster's share of a snapshot.
type ClusterLive struct {
	KPI   KPI        `json:"kpi"`
	Users []UserLive `json:"users"`
}

// QuotaKey is where Inputs.Quotas holds an owner's quota in a cluster
// (quotas are counted per cluster; no cluster: the owner alone).
func QuotaKey(cluster, owner string) string {
	if cluster == "" {
		return owner
	}
	return cluster + "/" + owner
}

// ProcLive is one process line for the detail page.
type ProcLive struct {
	Pid     int     `json:"pid"`
	Start   uint64  `json:"start"`
	PPid    int     `json:"ppid,omitempty"` // the parent, for the tree; 0: not known
	Exe     string  `json:"exe"`
	Cmdline string  `json:"cmdline"`
	CPU     float64 `json:"cpu"`
	RSS     uint64  `json:"rss"`
	IOR     float64 `json:"ioR"`
	IOW     float64 `json:"ioW"`
	TCPRx   float64 `json:"tcpRx"`
	TCPTx   float64 `json:"tcpTx"`
	GPU     []int   `json:"gpu,omitempty"`
	User    string  `json:"user,omitempty"` // session owner
	How     string  `json:"how,omitempty"`  // attribution rule
}

// Inputs come from the k8s caches (and the leader's minute jobs).
type Inputs struct {
	DevPods  []DevPodRef
	Nodes    []NodeRef
	Quotas   map[string]Quota // by QuotaKey(cluster, owner); zero fields = unlimited
	Idle     map[string]bool  // by DevPod UID
	Alerts   []AlertRef
	Presence map[string][]string // DevPod UID → users (audit sessions), used when telemetry has none
}

// DevPodRef is a DevPod as the cluster sees it.
type DevPodRef struct {
	Cluster                       string // in a console of several
	UID, Name, Owner, Node, Phase string
	PhaseSince                    time.Time
	CPULimit, MemLimit            float64
	CPURequest, MemRequest        float64
	GPURequest                    int
}

// NodeRef is a node's capacity.
type NodeRef struct {
	Cluster        string // in a console of several
	Name           string
	Ready          bool
	Labels         map[string]string
	CPUAllocatable float64
	MemAllocatable float64
	GPUAllocatable int
	GPUModel       string
	// What pods other than DevPods hold here (their requests, as the
	// cluster's agent reports them): taken, as DevPods' requests are.
	OtherCPU, OtherMem float64
	OtherGPU           int
}

// Quota is an owner's compute quota.
type Quota struct {
	CPU, Mem float64
	GPU      int
}

// AlertRef is a recent audit alert.
type AlertRef struct {
	TS                         time.Time
	DevPodUID, SessionID, Kind string
	Detail                     string
}
