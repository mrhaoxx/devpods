// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// emitter writes the snapshot v2 protocol (spec §2.1) to the Job's
// stdout: "@" lines for the console and CLI, anything else is log text.
type emitter struct {
	mu       sync.Mutex
	w        io.Writer
	termPath string
	now      func() time.Time

	lastPush            time.Time
	pushed, total       int64
	sentPush, sentTotal int64
	havePush            bool
}

func newEmitter(w io.Writer, termPath string) *emitter {
	return &emitter{w: w, termPath: termPath, now: time.Now, sentPush: -1}
}

func (e *emitter) line(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Fprintf(e.w, format+"\n", args...)
}

func (e *emitter) Stage(name string) { e.line("@stage %s", name) }
func (e *emitter) Size(n int64)      { e.line("@size %d", n) }
func (e *emitter) Digest(d string)   { e.line("@digest %s", d) }
func (e *emitter) Done()             { e.line("@done") }
func (e *emitter) Error(msg string)  { e.line("@error %s", oneLine(msg)) }

// Log writes ordinary log text (never a protocol line).
func (e *emitter) Log(msg string) { e.line("%s", strings.TrimLeft(oneLine(msg), "@")) }

// Logf is Log with a format: the human-readable account of the run that
// the console and the CLI show next to the progress.
func (e *emitter) Logf(format string, args ...any) { e.Log(fmt.Sprintf(format, args...)) }

// Layer says that one blob of the image is in the registry.
func (e *emitter) Layer(id string, size int64) {
	if size > 0 {
		e.Logf("%s %s 已推送", blobName(id), humanBytes(size))
	} else {
		e.Logf("%s 仓库里已有，跳过", blobName(id))
	}
}

// blobName turns a push key ("layer-sha256:…", "manifest-sha256:…", a
// Docker layer id) into "层 sha256:1a2b3c4d5e6f".
func blobName(id string) string {
	kind := "层"
	for prefix, name := range map[string]string{"layer-": "层", "manifest-": "清单", "config-": "配置", "index-": "索引"} {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			kind, id = name, rest
			break
		}
	}
	return kind + " " + shortDigest(id)
}

// shortDigest keeps the algorithm and 12 hex digits.
func shortDigest(d string) string {
	algo, hex, ok := strings.Cut(d, ":")
	if !ok {
		algo, hex = "", d
	}
	if len(hex) > 12 {
		hex = hex[:12]
	}
	if algo == "" {
		return hex
	}
	return algo + ":" + hex
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// pushInterval: progress goes out at most this often.
const pushInterval = time.Second

// Push reports push progress; it is throttled to one line per second and
// never repeats an unchanged pair.
func (e *emitter) Push(pushed, total int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pushed, e.total, e.havePush = pushed, total, true
	if e.now().Sub(e.lastPush) < pushInterval {
		return
	}
	e.flushPushLocked()
}

// PushEnd writes the last progress line if it was held back.
func (e *emitter) PushEnd() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.havePush {
		e.flushPushLocked()
	}
}

func (e *emitter) flushPushLocked() {
	if e.pushed == e.sentPush && e.total == e.sentTotal {
		return
	}
	e.sentPush, e.sentTotal, e.lastPush = e.pushed, e.total, e.now()
	fmt.Fprintf(e.w, "@push %d %d\n", e.pushed, e.total)
}

// Result writes the termination message the controller reads:
// "digest=… size=…" (either may be missing) or "error=…".
func (e *emitter) Result(digest string, size int64, errMsg string) {
	var msg string
	if errMsg != "" {
		msg = "error=" + oneLine(errMsg)
	} else {
		var parts []string
		if digest != "" {
			parts = append(parts, "digest="+digest)
		}
		if size > 0 {
			parts = append(parts, fmt.Sprintf("size=%d", size))
		}
		msg = strings.Join(parts, " ")
	}
	if e.termPath != "" {
		_ = os.WriteFile(e.termPath, []byte(msg+"\n"), 0o644)
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// layerProgress sums per-layer upload progress. A layer's total is
// remembered once seen; a layer reported done counts in full; a layer the
// registry already has may never report a size and then counts as zero.
type layerProgress struct {
	mu     sync.Mutex
	offset map[string]int64
	total  map[string]int64
	done   map[string]bool
	// onDone, when set, hears once per layer that it is up; size 0 means
	// the registry already had it.
	onDone func(id string, size int64)
}

func newLayerProgress(onDone func(id string, size int64)) *layerProgress {
	return &layerProgress{offset: map[string]int64{}, total: map[string]int64{}, done: map[string]bool{}, onDone: onDone}
}

// finishLocked marks id complete; it returns the callback to run once
// the lock is released, or nil when id was complete already.
func (p *layerProgress) finishLocked(id string) func() {
	if p.done[id] {
		return nil
	}
	p.done[id] = true
	if p.onDone == nil {
		return nil
	}
	size, fn := p.total[id], p.onDone
	return func() { fn(id, size) }
}

// Set records a layer's progress; total <= 0 keeps the remembered total.
func (p *layerProgress) Set(id string, offset, total int64) {
	p.mu.Lock()
	if total > 0 {
		p.total[id] = total
	}
	t := p.total[id]
	if t > 0 && offset > t {
		offset = t
	}
	if offset > p.offset[id] { // progress never goes backwards
		p.offset[id] = offset
	}
	var tell func()
	if t > 0 && p.offset[id] >= t {
		tell = p.finishLocked(id)
	}
	p.mu.Unlock()
	if tell != nil {
		tell()
	}
}

// Finish marks a layer complete.
func (p *layerProgress) Finish(id string) {
	p.mu.Lock()
	if t := p.total[id]; t > 0 {
		p.offset[id] = t
	}
	tell := p.finishLocked(id)
	p.mu.Unlock()
	if tell != nil {
		tell()
	}
}

// Sum returns (pushed, total) over the layers seen.
func (p *layerProgress) Sum() (pushed, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, t := range p.total {
		total += t
		pushed += p.offset[id]
	}
	return pushed, total
}

// report folds a push tracker's per-blob status into lp: jobs maps each
// blob the push handles to its size; status answers for the ones the
// tracker has seen (a blob it has not started yet counts as zero).
func report(jobs map[string]int64, status func(ref string) (offset, total int64, ok bool), lp *layerProgress) (pushed, total int64) {
	for ref, size := range jobs {
		lp.Set(ref, 0, size)
		if off, tot, ok := status(ref); ok {
			if tot <= 0 {
				tot = size
			}
			lp.Set(ref, off, tot)
		}
	}
	return lp.Sum()
}
