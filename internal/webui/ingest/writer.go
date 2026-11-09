// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package ingest receives supervisor telemetry on the webui leader
// (spec §5.10): it authenticates each DevPod, feeds the live state and
// merges all batches of one second into one ClickHouse insert per table.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// ErrNotLeader fails batches that were waiting when leadership ended.
var ErrNotLeader = errors.New("not the telemetry leader")

// Store is the part of *store.CH the writer uses.
type Store interface {
	InsertTelemetry(ctx context.Context, b store.TelemetryBatch, token string) error
}

// Writer merges submitted rows for Interval (from the first submit) and
// writes them with one InsertTelemetry call; inserts never overlap.
type Writer struct {
	Store    Store
	Interval time.Duration // merge window; default 1 s
	Timeout  time.Duration // per insert; default 30 s
	IsLeader func() bool   // checked before each insert; nil = always

	mu        sync.Mutex
	pending   store.TelemetryBatch
	keys      []string
	dones     []func(error)
	scheduled bool
	flushing  bool
}

// NewWriter creates a writer for st.
func NewWriter(st Store) *Writer { return &Writer{Store: st} }

func (w *Writer) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return time.Second
}

// Submit queues rows; key ("uid/boot/from-to") goes into the merged
// insert's dedup token. done runs once the insert has finished.
func (w *Writer) Submit(key string, rows store.TelemetryBatch, done func(error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	merge(&w.pending, rows)
	w.keys = append(w.keys, key)
	w.dones = append(w.dones, done)
	w.scheduleLocked()
}

func (w *Writer) scheduleLocked() {
	if !w.scheduled && !w.flushing && len(w.dones) > 0 {
		w.scheduled = true
		time.AfterFunc(w.interval(), w.flush)
	}
}

func (w *Writer) flush() {
	w.mu.Lock()
	b, keys, dones := w.pending, w.keys, w.dones
	w.pending, w.keys, w.dones = store.TelemetryBatch{}, nil, nil
	w.scheduled, w.flushing = false, true
	w.mu.Unlock()

	err := w.write(b, keys)
	for _, d := range dones {
		d(err)
	}

	w.mu.Lock()
	w.flushing = false
	w.scheduleLocked()
	w.mu.Unlock()
}

func (w *Writer) write(b store.TelemetryBatch, keys []string) error {
	if b.Empty() {
		return nil
	}
	if w.IsLeader != nil && !w.IsLeader() {
		return ErrNotLeader
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return w.Store.InsertTelemetry(ctx, b, hex.EncodeToString(sum[:]))
}

func merge(dst *store.TelemetryBatch, b store.TelemetryBatch) {
	dst.Containers = append(dst.Containers, b.Containers...)
	dst.GPUs = append(dst.GPUs, b.GPUs...)
	dst.RDMAs = append(dst.RDMAs, b.RDMAs...)
	dst.Procs = append(dst.Procs, b.Procs...)
	dst.ProcRows = append(dst.ProcRows, b.ProcRows...)
	dst.SSHConns = append(dst.SSHConns, b.SSHConns...)
	dst.Commands = append(dst.Commands, b.Commands...)
	dst.Infos = append(dst.Infos, b.Infos...)
	dst.Snapshots = append(dst.Snapshots, b.Snapshots...)
	dst.Alerts = append(dst.Alerts, b.Alerts...)
}
