// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Minute-job windows.
const (
	pendingLookback = time.Hour          // pending rows retried for an hour
	sessionLookback = 7 * 24 * time.Hour // sessions (and shells) this old still match
	matchSkew       = 5 * time.Second    // clock skew between gateway and agent
	infoLookback    = 30 * 24 * time.Hour
)

// JobStore is the part of *store.CH the minute jobs use.
type JobStore interface {
	IdleCandidates(ctx context.Context, now time.Time, cpuPct, gpuPct float64) ([]string, error)
	PendingAttributions(ctx context.Context, since time.Time) ([]store.SSHConnRow, error)
	PendingProcesses(ctx context.Context, since time.Time) ([]store.ProcessRow, error)
	PendingCommands(ctx context.Context, since, procSince time.Time) ([]store.PendingCommand, error)
	BackendSessions(ctx context.Context, since time.Time) ([]store.BackendSession, error)
	HookMissing(ctx context.Context, from, to time.Time) ([]store.SessionRow, error)
	InfoSeen(ctx context.Context, since time.Time) (map[string]time.Time, error)
	InsertTelemetry(ctx context.Context, b store.TelemetryBatch, token string) error
}

// RunningDevPod is a Running DevPod and when its Pod was created.
type RunningDevPod struct {
	UID, Name string
	PodStart  time.Time
}

// Jobs are the leader's once-a-minute tasks (spec §5.4, §5.8, UI §5.3).
type Jobs struct {
	Store      JobStore
	Running    func(ctx context.Context) []RunningDevPod
	LastSample func(uid string) (time.Time, bool) // live state's last container sample
	SetIdle    func(map[string]bool)
	Event      func(uid, kind, detail string) // k8s Event on the DevPod; nil = none

	IdleCPUPct, IdleGPUPct float64
	SilentAfter            time.Duration
	Since                  time.Time // leadership start

	silent map[string]bool // open agent_silent incidents
}

// Run calls RunOnce every minute until ctx ends (leadership lost).
func (j *Jobs) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := j.RunOnce(ctx, now.UTC()); err != nil && ctx.Err() == nil {
				slog.Warn("telemetry: minute jobs", "err", err)
			}
		}
	}
}

// RunOnce runs backfill, cross-checks and idle detection once. It stops
// at the first cancellation and writes nothing after it.
func (j *Jobs) RunOnce(ctx context.Context, now time.Time) error {
	var errs []error
	for _, step := range []func(context.Context, time.Time) error{j.backfill, j.crossCheck, j.idle} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := step(ctx, now); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// token derives an insert dedup token from the rows' identities, so a
// retried or repeated insert of the same rows is dropped and different
// rows never share a token.
func token(kind string, keys []string) string {
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(kind + "\n" + strings.Join(keys, "\n")))
	return kind + "/" + hex.EncodeToString(sum[:12])
}

type sessionIndex map[[2]string][]store.BackendSession

// match finds the session on (uid, addr) that was open at `at`; the
// newest wins when a reused source port matches several.
func (ix sessionIndex) match(uid, addr string, at time.Time) (store.BackendSession, bool) {
	var best store.BackendSession
	found := false
	for _, s := range ix[[2]string{uid, addr}] {
		if s.OpenedAt.After(at.Add(matchSkew)) || (s.ClosedAt != nil && s.ClosedAt.Before(at.Add(-matchSkew))) {
			continue
		}
		if !found || s.OpenedAt.After(best.OpenedAt) {
			best, found = s, true
		}
	}
	return best, found
}

// backfill re-inserts pending ssh_conns / processes / commands whose
// gateway session has landed, one version higher (spec §5.4).
func (j *Jobs) backfill(ctx context.Context, now time.Time) error {
	sessions, err := j.Store.BackendSessions(ctx, now.Add(-sessionLookback))
	if err != nil {
		return err
	}
	ix := sessionIndex{}
	for _, s := range sessions {
		k := [2]string{s.UID, s.Addr}
		ix[k] = append(ix[k], s)
	}
	since := now.Add(-pendingLookback)
	conns, err := j.Store.PendingAttributions(ctx, since)
	if err != nil {
		return err
	}
	procs, err := j.Store.PendingProcesses(ctx, since)
	if err != nil {
		return err
	}
	cmds, err := j.Store.PendingCommands(ctx, since, now.Add(-sessionLookback))
	if err != nil {
		return err
	}
	var b store.TelemetryBatch
	var keys []string
	for _, c := range conns {
		if s, ok := ix.match(c.DevPodUID, c.PeerAddr, c.FirstSeen); ok {
			c.SessionID, c.User, c.Version = s.SessionID, s.User, c.Version+1
			b.SSHConns = append(b.SSHConns, c)
			keys = append(keys, fmt.Sprintf("c/%s/%s/%d/%d/%d", c.DevPodUID, c.BootID, c.Pid, c.StartTime, c.Version))
		}
	}
	for _, p := range procs {
		if s, ok := ix.match(p.DevPodUID, p.Peer, p.FirstSeen); ok {
			p.SessionID, p.User, p.Version = s.SessionID, s.User, p.Version+1
			b.ProcRows = append(b.ProcRows, p)
			keys = append(keys, fmt.Sprintf("p/%s/%s/%d/%d/%d", p.DevPodUID, p.BootID, p.Pid, p.StartTime, p.Version))
		}
	}
	for _, pc := range cmds {
		if s, ok := ix.match(pc.DevPodUID, pc.Peer, pc.TS); ok {
			r := pc.CommandRow
			r.SessionID, r.User, r.Attribution, r.Version = s.SessionID, s.User, store.CommandAttribution(pc.ProcAttribution), r.Version+1
			b.Commands = append(b.Commands, r)
			keys = append(keys, fmt.Sprintf("x/%s/%d", r.EventID, r.Version))
		}
	}
	if b.Empty() || ctx.Err() != nil {
		return ctx.Err()
	}
	return j.Store.InsertTelemetry(ctx, b, token("backfill", keys))
}

// crossCheck raises hook_missing and agent_silent (spec §5.8).
func (j *Jobs) crossCheck(ctx context.Context, now time.Time) error {
	var alerts []store.AlertRow
	// The previous full minute, one minute late so the gateway's
	// session rows (flushed every 5 s) have landed.
	to := now.Truncate(time.Minute).Add(-time.Minute)
	missing, err := j.Store.HookMissing(ctx, to.Add(-time.Minute), to)
	if err != nil {
		return err
	}
	for _, s := range missing {
		at := to
		if s.ClosedAt != nil {
			at = *s.ClosedAt
		}
		alerts = append(alerts, store.AlertRow{TS: at, DevPodUID: s.DevPodUID, SessionID: s.SessionID, Kind: "hook_missing", Source: "webui",
			Detail: fmt.Sprintf("%s typed into a terminal for %s but no shell-hook command was recorded", s.User, at.Sub(s.OpenedAt).Truncate(time.Second))})
	}

	running := j.Running(ctx)
	seen, err := j.Store.InfoSeen(ctx, now.Add(-infoLookback))
	if err != nil {
		return err
	}
	if j.silent == nil {
		j.silent = map[string]bool{}
	}
	alive := map[string]bool{}
	for _, d := range running {
		alive[d.UID] = true
		info, ok := seen[d.UID]
		if !ok || info.Before(d.PodStart) {
			delete(j.silent, d.UID) // this Pod has no telemetry (older supervisor, or disabled)
			continue
		}
		quiet := latest(j.Since, d.PodStart, info)
		if t, ok := j.LastSample(d.UID); ok && t.After(quiet) {
			quiet = t
		}
		if now.Sub(quiet) <= j.SilentAfter {
			delete(j.silent, d.UID)
			continue
		}
		if !j.silent[d.UID] {
			j.silent[d.UID] = true
			alerts = append(alerts, store.AlertRow{TS: now, DevPodUID: d.UID, Kind: "agent_silent", Source: "webui",
				Detail: fmt.Sprintf("%s: no telemetry for %s", d.Name, now.Sub(quiet).Truncate(time.Second))})
		}
	}
	for uid := range j.silent {
		if !alive[uid] {
			delete(j.silent, uid)
		}
	}
	if len(alerts) == 0 || ctx.Err() != nil {
		return ctx.Err()
	}
	keys := make([]string, 0, len(alerts))
	for _, a := range alerts {
		keys = append(keys, fmt.Sprintf("%s/%s/%s/%d", a.Kind, a.DevPodUID, a.SessionID, a.TS.UnixMilli()))
	}
	if err := j.Store.InsertTelemetry(ctx, store.TelemetryBatch{Alerts: alerts}, token("crosscheck", keys)); err != nil {
		return err
	}
	if j.Event != nil {
		for _, a := range alerts {
			j.Event(a.DevPodUID, a.Kind, a.Detail)
		}
	}
	return nil
}

// idle refreshes the idle set the overview's "needs attention" uses.
func (j *Jobs) idle(ctx context.Context, now time.Time) error {
	uids, err := j.Store.IdleCandidates(ctx, now, j.IdleCPUPct, j.IdleGPUPct)
	if err != nil {
		return err
	}
	m := make(map[string]bool, len(uids))
	for _, u := range uids {
		m[u] = true
	}
	j.SetIdle(m)
	return nil
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
