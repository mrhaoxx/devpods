// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package store persists audit data: raw segments and file contents in
// S3, structured rows in ClickHouse. Row structs mirror the ClickHouse
// columns (ch tags) and are returned by the webui as JSON unchanged.
package store

import "time"

// SessionRow is one SSH connection through the gateway. Written twice:
// version 1 at open, version 2 (complete) at close.
type SessionRow struct {
	// Cluster: the cluster of its DevPod, in a console of several ("" in a console of one).
	Cluster string `ch:"cluster" json:"cluster,omitempty"`
	// Gateway: the registered gateway it came in at, for a gateway of
	// several clusters.
	Gateway    string `ch:"gateway" json:"gateway,omitempty"`
	SessionID  string `ch:"session_id" json:"session_id"`
	User       string `ch:"user" json:"user"`
	DevPod     string `ch:"devpod" json:"devpod"`
	DevPodUID  string `ch:"devpod_uid" json:"devpod_uid"`
	Owner      string `ch:"owner" json:"owner"`
	GatewayPod string `ch:"gateway_pod" json:"gateway_pod"`
	ClientIP   string `ch:"client_ip" json:"client_ip"`
	PubkeyFP   string `ch:"pubkey_fp" json:"pubkey_fp"`
	// Pubkey: the key itself, an authorized_keys line without a comment
	// ("" in a session of before it was kept)
	Pubkey string `ch:"pubkey" json:"pubkey,omitempty"`
	// LastActive: when its row was last written — open, every minute
	// while open, closed; a session's rows expire by it, so one longer
	// than the retention is not lost while it lasts
	LastActive       time.Time  `ch:"last_active" json:"last_active"`
	AuthKind         string     `ch:"auth_kind" json:"auth_kind"`
	ProxyAlias       string     `ch:"proxy_alias" json:"proxy_alias"`
	BackendLocalAddr string     `ch:"backend_local_addr" json:"backend_local_addr"`
	OpenedAt         time.Time  `ch:"opened_at" json:"opened_at"`
	ClosedAt         *time.Time `ch:"closed_at" json:"closed_at"`
	CloseReason      string     `ch:"close_reason" json:"close_reason"`
	BytesIn          int64      `ch:"bytes_in" json:"bytes_in"`
	BytesOut         int64      `ch:"bytes_out" json:"bytes_out"`
	PolicySnapshot   string     `ch:"policy_snapshot" json:"policy_snapshot"`
	Gaps             uint32     `ch:"gaps" json:"gaps"`
	Version          uint64     `ch:"version" json:"version"`
}

// ChannelRow is one SSH channel of a session (versioned like SessionRow).
type ChannelRow struct {
	// Cluster: the cluster of its DevPod, in a console of several ("" in a console of one).
	Cluster     string     `ch:"cluster" json:"cluster,omitempty"`
	SessionID   string     `ch:"session_id" json:"session_id"`
	ChannelID   uint32     `ch:"channel_id" json:"channel_id"`
	Type        string     `ch:"type" json:"type"`
	Command     string     `ch:"command" json:"command"`
	Subsystem   string     `ch:"subsystem" json:"subsystem"`
	Target      string     `ch:"target" json:"target"`
	Pty         bool       `ch:"pty" json:"pty"`
	Term        string     `ch:"term" json:"term"`
	OpenedAt    time.Time  `ch:"opened_at" json:"opened_at"`
	ClosedAt    *time.Time `ch:"closed_at" json:"closed_at"`
	ExitStatus  *int32     `ch:"exit_status" json:"exit_status"`
	BytesIn     int64      `ch:"bytes_in" json:"bytes_in"`
	BytesOut    int64      `ch:"bytes_out" json:"bytes_out"`
	RawRecorded bool       `ch:"raw_recorded" json:"raw_recorded"`
	Version     uint64     `ch:"version" json:"version"`
}

// CommandRow is one command: an exec request (gateway) or a shell-hook
// record (container, phase 2).
type CommandRow struct {
	// Cluster: the cluster of its DevPod, in a console of several ("" in a console of one).
	Cluster     string    `ch:"cluster" json:"cluster,omitempty"`
	EventID     string    `ch:"event_id" json:"event_id"`
	TS          time.Time `ch:"ts" json:"ts"`
	SessionID   string    `ch:"session_id" json:"session_id"`
	DevPodUID   string    `ch:"devpod_uid" json:"devpod_uid"`
	User        string    `ch:"user" json:"user"`
	Source      string    `ch:"source" json:"source"`
	ChannelID   uint32    `ch:"channel_id" json:"channel_id"`
	PID         int32     `ch:"pid" json:"pid"`
	Cwd         string    `ch:"cwd" json:"cwd"`
	Cmdline     string    `ch:"cmdline" json:"cmdline"`
	ExitCode    *int32    `ch:"exit_code" json:"exit_code"`
	DurationMS  int64     `ch:"duration_ms" json:"duration_ms"`
	Attribution string    `ch:"attribution" json:"attribution"`
	Version     uint64    `ch:"version" json:"version"`
}

// FileEventRow is one file transfer or file operation.
type FileEventRow struct {
	// Cluster: the cluster of its DevPod, in a console of several ("" in a console of one).
	Cluster   string    `ch:"cluster" json:"cluster,omitempty"`
	EventID   string    `ch:"event_id" json:"event_id"`
	TS        time.Time `ch:"ts" json:"ts"`
	SessionID string    `ch:"session_id" json:"session_id"`
	ChannelID uint32    `ch:"channel_id" json:"channel_id"`
	User      string    `ch:"user" json:"user"`
	DevPodUID string    `ch:"devpod_uid" json:"devpod_uid"`
	Protocol  string    `ch:"protocol" json:"protocol"`
	Op        string    `ch:"op" json:"op"`
	Path      string    `ch:"path" json:"path"`
	Path2     string    `ch:"path2" json:"path2"`
	Size      int64     `ch:"size" json:"size"`
	SHA256    string    `ch:"sha256" json:"sha256"`
	Complete  bool      `ch:"complete" json:"complete"`
	Blocked   bool      `ch:"blocked" json:"blocked"`
	S3Key     string    `ch:"s3_key" json:"s3_key"`
}

// PolicyEventRow is one policy enforcement.
type PolicyEventRow struct {
	// Cluster: the cluster of its DevPod, in a console of several ("" in a console of one).
	Cluster   string    `ch:"cluster" json:"cluster,omitempty"`
	TS        time.Time `ch:"ts" json:"ts"`
	SessionID string    `ch:"session_id" json:"session_id"`
	User      string    `ch:"user" json:"user"`
	DevPodUID string    `ch:"devpod_uid" json:"devpod_uid"`
	Rule      string    `ch:"rule" json:"rule"`
	Action    string    `ch:"action" json:"action"`
	Detail    string    `ch:"detail" json:"detail"`
}

// RawSegmentRow indexes one uploaded raw segment.
type RawSegmentRow struct {
	// Cluster: the cluster of its DevPod, in a console of several ("" in a console of one).
	Cluster         string    `ch:"cluster" json:"cluster,omitempty"`
	SessionID       string    `ch:"session_id" json:"session_id"`
	Seq             uint32    `ch:"seq" json:"seq"`
	S3Key           string    `ch:"s3_key" json:"s3_key"`
	FirstTS         time.Time `ch:"first_ts" json:"first_ts"`
	LastTS          time.Time `ch:"last_ts" json:"last_ts"`
	Bytes           int64     `ch:"bytes" json:"bytes"`
	CompressedBytes int64     `ch:"compressed_bytes" json:"compressed_bytes"`
}

// HTTPRequestRow is one request a gateway's HTTP proxy served to a DevPod
// port (one WebSocket connection, recorded when it closed).
type HTTPRequestRow struct {
	TS         time.Time `ch:"ts" json:"ts"`
	DurationMS uint32    `ch:"duration_ms" json:"duration_ms"`
	// Cluster: the DevPod's ("" in a console of one); Gateway: the
	// gateway's registered name ("" for a gateway of one cluster).
	Cluster    string `ch:"cluster" json:"cluster,omitempty"`
	Gateway    string `ch:"gateway" json:"gateway,omitempty"`
	GatewayPod string `ch:"gateway_pod" json:"gateway_pod"`
	DevPod     string `ch:"devpod" json:"devpod"`
	DevPodUID  string `ch:"devpod_uid" json:"devpod_uid"`
	Owner      string `ch:"owner" json:"owner"`
	Port       uint16 `ch:"port" json:"port"`
	// Access: the port's level when served (public, users, private).
	Access string `ch:"access" json:"access"`
	// User: by the gateway's login cookie; "" is anonymous.
	User      string `ch:"user" json:"user"`
	Method    string `ch:"method" json:"method"`
	Host      string `ch:"host" json:"host"`
	Path      string `ch:"path" json:"path"`
	Query     string `ch:"query" json:"query"`
	Status    uint16 `ch:"status" json:"status"`
	BytesIn   uint64 `ch:"bytes_in" json:"bytes_in"`
	BytesOut  uint64 `ch:"bytes_out" json:"bytes_out"`
	Websocket bool   `ch:"websocket" json:"websocket"`
	// ClientIP: the TCP peer; ForwardedFor: X-Forwarded-For as received,
	// unverified.
	ClientIP     string `ch:"client_ip" json:"client_ip"`
	ForwardedFor string `ch:"forwarded_for" json:"forwarded_for"`
	UserAgent    string `ch:"user_agent" json:"user_agent"`
	RequestID    string `ch:"request_id" json:"request_id"`
}

// AlertRow is a cross-check or data-loss alert.
type AlertRow struct {
	// Cluster: the cluster of its DevPod, in a console of several ("" in a console of one).
	Cluster   string    `ch:"cluster" json:"cluster,omitempty"`
	TS        time.Time `ch:"ts" json:"ts"`
	DevPodUID string    `ch:"devpod_uid" json:"devpod_uid"`
	SessionID string    `ch:"session_id" json:"session_id"`
	Kind      string    `ch:"kind" json:"kind"`
	Source    string    `ch:"source" json:"source"`
	Detail    string    `ch:"detail" json:"detail"`
}

// AccessLogRow records who looked at or changed audit data.
type AccessLogRow struct {
	TS            time.Time `ch:"ts" json:"ts"`
	Viewer        string    `ch:"viewer" json:"viewer"`
	Action        string    `ch:"action" json:"action"`
	TargetSession string    `ch:"target_session" json:"target_session"`
	TargetFile    string    `ch:"target_file" json:"target_file"`
	ClientIP      string    `ch:"client_ip" json:"client_ip"`
}

// Batch groups rows written together.
type Batch struct {
	Sessions     []SessionRow     `json:"sessions,omitempty"`
	Channels     []ChannelRow     `json:"channels,omitempty"`
	Commands     []CommandRow     `json:"commands,omitempty"`
	FileEvents   []FileEventRow   `json:"file_events,omitempty"`
	PolicyEvents []PolicyEventRow `json:"policy_events,omitempty"`
	RawSegments  []RawSegmentRow  `json:"raw_segments,omitempty"`
	Alerts       []AlertRow       `json:"alerts,omitempty"`
	// HTTPRequests carry each its own cluster (SetCluster leaves them).
	HTTPRequests []HTTPRequestRow `json:"http_requests,omitempty"`
}

// Empty reports whether the batch has no rows.
func (b Batch) Empty() bool {
	return len(b.Sessions)+len(b.Channels)+len(b.Commands)+len(b.FileEvents)+
		len(b.PolicyEvents)+len(b.RawSegments)+len(b.Alerts)+len(b.HTTPRequests) == 0
}

// SetCluster says every row of b is of a DevPod in cluster.
func (b *Batch) SetCluster(cluster string) {
	for i := range b.Sessions {
		b.Sessions[i].Cluster = cluster
	}
	for i := range b.Channels {
		b.Channels[i].Cluster = cluster
	}
	for i := range b.Commands {
		b.Commands[i].Cluster = cluster
	}
	for i := range b.FileEvents {
		b.FileEvents[i].Cluster = cluster
	}
	for i := range b.PolicyEvents {
		b.PolicyEvents[i].Cluster = cluster
	}
	for i := range b.RawSegments {
		b.RawSegments[i].Cluster = cluster
	}
	for i := range b.Alerts {
		b.Alerts[i].Cluster = cluster
	}
}
