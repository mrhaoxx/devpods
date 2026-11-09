// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// Buffer holds unacknowledged records in two partitions (spec §5.7):
// audit records (process events, ssh connections, commands, alerts) get
// a quarter of the budget, metrics the rest. Metrics overflow drops the
// oldest metrics; audit overflow drops the oldest audit records and
// keeps one cumulative gap alert.
type Buffer struct {
	mu                   sync.Mutex
	auditCap, metricsCap int64
	audit, metrics       []bufEntry
	auditSize, metSize   int64
	dropped              int // audit records dropped since the last acked gap
	notify               chan struct{}
}

type bufEntry struct {
	seq  uint64
	rec  wire.Record
	size int64
	gap  bool
}

// NewBuffer budgets total bytes (JSON size of the records).
func NewBuffer(total int64) *Buffer {
	return &Buffer{auditCap: total / 4, metricsCap: total - total/4, notify: make(chan struct{}, 1)}
}

func recSize(r wire.Record) int64 {
	b, _ := json.Marshal(r)
	return int64(len(b))
}

// Add appends one second's records.
func (b *Buffer) Add(seq uint64, recs []wire.Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range recs {
		e := bufEntry{seq: seq, rec: r, size: recSize(r)}
		if r.K.Audit() {
			b.audit = append(b.audit, e)
			b.auditSize += e.size
		} else {
			b.metrics = append(b.metrics, e)
			b.metSize += e.size
		}
	}
	for b.metSize > b.metricsCap && len(b.metrics) > 0 {
		b.metSize -= b.metrics[0].size
		b.metrics = b.metrics[1:]
	}
	overflow := false
	for b.auditSize > b.auditCap && len(b.audit) > 0 {
		if !b.audit[0].gap {
			b.dropped++
		}
		b.auditSize -= b.audit[0].size
		b.audit = b.audit[1:]
		overflow = true
	}
	if overflow {
		// Replace the gap alert with one carrying the running count.
		for i := range b.audit {
			if b.audit[i].gap {
				b.auditSize -= b.audit[i].size
				b.audit = append(b.audit[:i], b.audit[i+1:]...)
				break
			}
		}
		g := bufEntry{seq: seq, gap: true, rec: wire.Record{K: wire.KAlert, Alert: &wire.Alert{TS: time.Now().UTC(), Kind: "gap",
			Detail: fmt.Sprintf("%d audit records dropped (telemetry buffer full)", b.dropped)}}}
		g.size = recSize(g.rec)
		b.audit = append(b.audit, g)
		b.auditSize += g.size
	}
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// Pending returns the oldest whole seconds whose records fit maxBytes
// (always at least one second), merged in seq order.
func (b *Buffer) Pending(maxBytes int) (from, to uint64, recs []wire.Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, j := 0, 0
	var size int64
	for i < len(b.audit) || j < len(b.metrics) {
		takeAudit := j >= len(b.metrics) || (i < len(b.audit) && b.audit[i].seq <= b.metrics[j].seq)
		e := bufEntry{}
		if takeAudit {
			e = b.audit[i]
		} else {
			e = b.metrics[j]
		}
		if to != 0 && e.seq != to && size+e.size > int64(maxBytes) {
			break // a new second that would not fit
		}
		if takeAudit {
			i++
		} else {
			j++
		}
		if from == 0 {
			from = e.seq
		}
		to = e.seq
		size += e.size
		recs = append(recs, e.rec)
	}
	return from, to, recs
}

// Ack drops every record with seq ≤ acked.
func (b *Buffer) Ack(acked uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := func(es []bufEntry, size *int64) []bufEntry {
		n := 0
		for n < len(es) && es[n].seq <= acked {
			if es[n].gap {
				b.dropped = 0 // the count has been delivered
			}
			*size -= es[n].size
			n++
		}
		return es[n:]
	}
	b.audit = cut(b.audit, &b.auditSize)
	b.metrics = cut(b.metrics, &b.metSize)
}

// Len is the number of buffered records.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.audit) + len(b.metrics)
}

// Size is the buffered bytes.
func (b *Buffer) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.auditSize + b.metSize
}

// Notify fires (coalesced) whenever records are added.
func (b *Buffer) Notify() <-chan struct{} { return b.notify }
