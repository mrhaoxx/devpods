// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

const (
	hookFileMax      = 1 << 20  // spec §5.1: truncate + gap above 1 MiB
	hookTruncateFrom = 64 << 10 // truncate fully consumed files past this size
)

// HookReader turns hook files into wire.Command records.
type HookReader struct {
	Dirs     []string
	BootID   string
	ProcRoot string        // for shell starttime / tty; default /proc
	Sweep    time.Duration // exited-shell sweep; default 5 s
	Now      func() time.Time

	files map[string]*hookFile
	seq   map[string]int
}

type hookFile struct {
	offset int64
}

type hookRec struct {
	pid               int
	shellStart        uint64
	tty, cwd          string
	start, end, durMs float64
	exit              int
	cmd               string
}

// Run watches Dirs until ctx ends. alive reports whether a shell pid
// still exists; files of exited shells are read one last time, then
// removed.
func (h *HookReader) Run(ctx context.Context, alive func(pid int) bool, out chan<- wire.Command, alerts chan<- wire.Alert) {
	if h.ProcRoot == "" {
		h.ProcRoot = "/proc"
	}
	if h.Sweep == 0 {
		h.Sweep = 5 * time.Second
	}
	if h.Now == nil {
		h.Now = time.Now
	}
	h.files, h.seq = map[string]*hookFile{}, map[string]int{}
	w, err := fsnotify.NewWatcher()
	if err == nil {
		defer w.Close()
	}
	for _, d := range h.Dirs {
		// World-writable + sticky like /tmp: shells of any user append
		// their own files; the directory's existence switches hooks on.
		if os.MkdirAll(d, 0o1777) == nil {
			_ = os.Chmod(d, 0o1777)
		}
		if w != nil {
			_ = w.Add(d)
		}
	}
	emit := func(c wire.Command) bool {
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	gap := func(path string) {
		select {
		case alerts <- wire.Alert{TS: h.Now(), Kind: "gap", Detail: "hook file " + filepath.Base(path) + " exceeded 1 MiB and was truncated"}:
		case <-ctx.Done():
		}
	}
	h.sweep(alive, emit, gap)
	tick := time.NewTicker(h.Sweep)
	defer tick.Stop()
	var events chan fsnotify.Event
	if w != nil {
		events = w.Events
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				h.consume(ev.Name, emit, gap)
			}
		case <-tick.C:
			h.sweep(alive, emit, gap)
		}
	}
}

func (h *HookReader) sweep(alive func(int) bool, emit func(wire.Command) bool, gap func(string)) {
	for _, d := range h.Dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			path := filepath.Join(d, e.Name())
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			h.consume(path, emit, gap)
			if !alive(pid) {
				_ = os.Remove(path)
				delete(h.files, path)
			}
		}
	}
}

// consume reads complete records appended since the last read. It keeps
// a per-file offset instead of truncating after every read, which would
// race with a shell appending; fully consumed files are truncated once
// they pass 64 KiB.
func (h *HookReader) consume(path string, emit func(wire.Command) bool, gap func(string)) {
	if _, err := strconv.Atoi(filepath.Base(path)); err != nil {
		return
	}
	st := h.files[path]
	if st == nil {
		st = &hookFile{}
		h.files[path] = st
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	if info.Size() < st.offset { // truncated by someone else
		st.offset = 0
	}
	if info.Size()-st.offset > hookFileMax {
		_ = f.Truncate(0)
		st.offset = 0
		gap(path)
		return
	}
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return
	}
	recs, rest := splitHookRecords(buf)
	st.offset += int64(len(buf) - len(rest))
	for _, raw := range recs {
		r, err := parseHookRecord(raw)
		if err != nil {
			continue
		}
		if !emit(h.toCommand(r)) {
			return
		}
	}
	if st.offset >= hookTruncateFrom && len(rest) == 0 {
		if cur, err := f.Stat(); err == nil && cur.Size() == st.offset {
			_ = f.Truncate(0)
			st.offset = 0
		}
	}
}

// splitHookRecords returns the complete NUL-terminated records and the
// trailing partial one.
func splitHookRecords(b []byte) ([][]byte, []byte) {
	var out [][]byte
	for {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return out, b
		}
		out = append(out, b[:i])
		b = b[i+1:]
	}
}

func parseHookRecord(b []byte) (hookRec, error) {
	f := strings.SplitN(string(b), "\x1f", 9)
	if len(f) != 9 {
		return hookRec{}, fmt.Errorf("hook record: %d fields", len(f))
	}
	pid, err := strconv.Atoi(f[0])
	if err != nil {
		return hookRec{}, errors.New("hook record: bad pid")
	}
	fl := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	start, _ := strconv.ParseUint(f[1], 10, 64)
	exit, _ := strconv.Atoi(f[7])
	return hookRec{pid: pid, shellStart: start, tty: f[2], cwd: f[3], start: fl(f[4]), end: fl(f[5]), durMs: fl(f[6]), exit: exit, cmd: f[8]}, nil
}

func unixFloat(v float64) time.Time {
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

func (h *HookReader) toCommand(r hookRec) wire.Command {
	pidDir := filepath.Join(h.ProcRoot, strconv.Itoa(r.pid))
	if r.shellStart == 0 {
		stat := read(pidDir, "stat")
		if i := strings.LastIndexByte(stat, ')'); i >= 0 && i+2 <= len(stat) {
			if f := strings.Fields(stat[i+2:]); len(f) > 19 {
				r.shellStart, _ = strconv.ParseUint(f[19], 10, 64)
			}
		}
	}
	if r.tty == "" {
		if t, err := os.Readlink(filepath.Join(pidDir, "fd", "0")); err == nil && strings.HasPrefix(t, "/dev/") {
			r.tty = strings.TrimPrefix(t, "/dev/")
		}
	}
	end := h.Now().UTC()
	if r.end > 0 {
		end = unixFloat(r.end)
	}
	start := end
	switch {
	case r.start > 0:
		start = unixFloat(r.start)
	case r.durMs > 0:
		start = end.Add(-time.Duration(r.durMs * float64(time.Millisecond)))
	}
	key := fmt.Sprintf("%d/%d", r.pid, r.shellStart)
	h.seq[key]++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", h.BootID, key, h.seq[key])))
	return wire.Command{TS: start, EventID: hex.EncodeToString(sum[:]), ShellPid: r.pid, ShellStart: r.shellStart,
		TTY: r.tty, Cwd: r.cwd, Cmd: r.cmd, End: end, Exit: r.exit}
}
