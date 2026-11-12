// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// oneSecondMaxRange: longer ranges read the 1-minute rollups.
const oneSecondMaxRange = time.Hour

// MetricSeries is a time-aligned set of series for one DevPod. TS holds
// the bucket starts (unix seconds); each series has one value per
// bucket, nil where there was no sample (rendered as a gap).
type MetricSeries struct {
	Step   int64                 `json:"step"` // seconds
	TS     []int64               `json:"ts"`
	Series map[string][]*float64 `json:"series"`
}

// defaultStep keeps a chart around a few hundred points.
func defaultStep(r time.Duration) time.Duration {
	switch {
	case r <= 15*time.Minute:
		return time.Second
	case r <= oneSecondMaxRange:
		return 5 * time.Second
	case r <= 24*time.Hour:
		return time.Minute
	case r <= 7*24*time.Hour:
		return 5 * time.Minute
	default:
		return time.Hour
	}
}

// MetricsRange returns container, GPU and RDMA series for [from, to).
// Ranges up to an hour read the 1 s tables, longer ones the 1 m rollups.
func (c *CH) MetricsRange(ctx context.Context, uid string, from, to time.Time, step time.Duration) (*MetricSeries, error) {
	if !to.After(from) {
		return nil, ErrRangeRequired
	}
	minute := to.Sub(from) > oneSecondMaxRange
	if step <= 0 {
		step = defaultStep(to.Sub(from))
	}
	if minute && step < time.Minute {
		step = time.Minute
	}
	step = step.Truncate(time.Second)
	if step <= 0 {
		step = time.Second
	}
	secs := int64(step / time.Second)
	start := from.Unix() / secs * secs
	n := int((to.Unix()-start)/secs) + 1
	s := &MetricSeries{Step: secs, TS: make([]int64, n), Series: map[string][]*float64{}}
	for i := range s.TS {
		s.TS[i] = start + int64(i)*secs
	}
	set := func(name string, bucket int64, v float64) {
		i := int((bucket - start) / secs)
		if i < 0 || i >= n {
			return
		}
		col := s.Series[name]
		if col == nil {
			col = make([]*float64, n)
			s.Series[name] = col
		}
		col[i] = &v
	}
	ctx = c.readCtx(ctx)
	bucket := fmt.Sprintf("toUnixTimestamp(toStartOfInterval(ts, INTERVAL %d SECOND))", secs)

	var q string
	names := []string{"cpu", "cpu_limit", "cpu_throttled", "mem_anon", "mem_file", "mem_limit", "io_r", "io_w", "net_rx", "net_tx", "net_tcp_rx", "net_tcp_tx"}
	if minute {
		// throttled_usec is cumulative: the bucket's rate is (max - min) / seconds
		q = fmt.Sprintf(`SELECT %s AS b, avgMerge(cpu_avg), toFloat64(maxMerge(cpu_limit)),
			toFloat64(greatest(toInt64(maxMerge(throttled_max)) - toInt64(minMerge(throttled_min)), 0)) / %d,
			avgMerge(mem_anon_avg), avgMerge(mem_file_avg), toFloat64(maxMerge(mem_limit)), avgMerge(io_r_avg), avgMerge(io_w_avg),
			avgMerge(net_rx_avg), avgMerge(net_tx_avg), avgMerge(net_tcp_rx_avg), avgMerge(net_tcp_tx_avg)
			FROM %s.metrics_container_1m WHERE devpod_uid = ? AND ts >= ? AND ts < ? GROUP BY b ORDER BY b`, bucket, secs, c.db)
	} else {
		// throttled_usec is cumulative: each sample's increase over the
		// previous one (a reset counts as 0), summed per bucket
		q = fmt.Sprintf(`SELECT %s AS b, avg(cpu), toFloat64(max(cpu_limit)), toFloat64(sum(dthr)) / %d,
			avg(mem_anon), avg(mem_file), toFloat64(max(mem_limit)), avg(io_r), avg(io_w),
			avg(net_rx), avg(net_tx), avg(net_tcp_rx), avg(net_tcp_tx)
			FROM (SELECT *, greatest(toInt64(throttled_usec) - toInt64(lagInFrame(throttled_usec, 1, throttled_usec)
			        OVER (ORDER BY ts ASC ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)), 0) AS dthr
			      FROM %s.metrics_container_1s WHERE devpod_uid = ? AND ts >= ? AND ts < ?)
			GROUP BY b ORDER BY b`, bucket, secs, c.db)
	}
	// One step before the window, so its first bucket has the previous
	// sample to diff against (buckets before the window are dropped).
	rows, err := c.conn.Query(ctx, q, uid, from.Add(-step), to)
	if err != nil {
		return nil, fmt.Errorf("container series: %w", err)
	}
	for rows.Next() {
		var b uint32
		vals := make([]float64, len(names))
		dst := []any{&b}
		for i := range vals {
			dst = append(dst, &vals[i])
		}
		if err := rows.Scan(dst...); err != nil {
			rows.Close()
			return nil, err
		}
		for i, name := range names {
			set(name, int64(b), vals[i])
		}
	}
	rows.Close()

	if minute {
		q = fmt.Sprintf(`SELECT %s AS b, gpu_uuid, avgMerge(util_avg), avgMerge(mem_avg), avgMerge(power_avg), toFloat64(maxMerge(temp_max))
			FROM %s.metrics_gpu_1m WHERE devpod_uid = ? AND ts >= ? AND ts < ? GROUP BY b, gpu_uuid ORDER BY b`, bucket, c.db)
	} else {
		q = fmt.Sprintf(`SELECT %s AS b, gpu_uuid, avg(util), avg(mem_used), avg(power), toFloat64(max(temp))
			FROM %s.metrics_gpu_1s WHERE devpod_uid = ? AND ts >= ? AND ts < ? GROUP BY b, gpu_uuid ORDER BY b`, bucket, c.db)
	}
	if rows, err = c.conn.Query(ctx, q, uid, from, to); err != nil {
		return nil, fmt.Errorf("gpu series: %w", err)
	}
	for rows.Next() {
		var b uint32
		var uuid string
		var util, mem, power, temp float64
		if err := rows.Scan(&b, &uuid, &util, &mem, &power, &temp); err != nil {
			rows.Close()
			return nil, err
		}
		for k, v := range map[string]float64{"util": util, "mem": mem, "power": power, "temp": temp} {
			set("gpu."+uuid+"."+k, int64(b), v)
		}
	}
	rows.Close()

	if minute {
		q = fmt.Sprintf(`SELECT %s AS b, ibdev, port, avgMerge(rx_avg), avgMerge(tx_avg)
			FROM %s.metrics_rdma_1m WHERE devpod_uid = ? AND ts >= ? AND ts < ? GROUP BY b, ibdev, port ORDER BY b`, bucket, c.db)
	} else {
		q = fmt.Sprintf(`SELECT %s AS b, ibdev, port, avg(rx_bps), avg(tx_bps)
			FROM %s.metrics_rdma_1s WHERE devpod_uid = ? AND ts >= ? AND ts < ? GROUP BY b, ibdev, port ORDER BY b`, bucket, c.db)
	}
	if rows, err = c.conn.Query(ctx, q, uid, from, to); err != nil {
		return nil, fmt.Errorf("rdma series: %w", err)
	}
	for rows.Next() {
		var b uint32
		var dev string
		var port uint8
		var rx, tx float64
		if err := rows.Scan(&b, &dev, &port, &rx, &tx); err != nil {
			rows.Close()
			return nil, err
		}
		key := "rdma." + dev + "/" + strconv.Itoa(int(port))
		set(key+".rx", int64(b), rx)
		set(key+".tx", int64(b), tx)
	}
	rows.Close()
	return s, nil
}

// LatestInfo returns the newest container info and snapshot since since.
func (c *CH) LatestInfo(ctx context.Context, uid string, since time.Time) (*ContainerInfoRow, *SnapshotRow, error) {
	ctx = c.readCtx(ctx)
	var infos []ContainerInfoRow
	if err := c.conn.Select(ctx, &infos, fmt.Sprintf("SELECT %s FROM %s.container_info WHERE devpod_uid = ? AND ts >= ? ORDER BY ts DESC LIMIT 1", columns[ContainerInfoRow](), c.db), uid, since); err != nil {
		return nil, nil, fmt.Errorf("container info: %w", err)
	}
	if len(infos) == 0 {
		return nil, nil, ErrNotFound
	}
	var snaps []SnapshotRow
	if err := c.conn.Select(ctx, &snaps, fmt.Sprintf("SELECT %s FROM %s.container_snapshots WHERE devpod_uid = ? AND ts >= ? ORDER BY ts DESC LIMIT 1", columns[SnapshotRow](), c.db), uid, since); err != nil {
		return nil, nil, fmt.Errorf("container snapshot: %w", err)
	}
	var snap *SnapshotRow
	if len(snaps) > 0 {
		snap = &snaps[0]
	}
	return &infos[0], snap, nil
}

// idleCoverage: a DevPod must have rollups for most of the 24 h window
// to count as idle all day (one that just started is not).
const idleCoverage = 23 * 60

// IdleCandidates lists DevPods idle for the 24 h before now: CPU below
// cpuPct of their limit (of one core when unlimited), GPU utilisation
// below gpuPct, and no SSH session (UI spec §5.3).
func (c *CH) IdleCandidates(ctx context.Context, now time.Time, cpuPct, gpuPct float64) ([]string, error) {
	from := now.Add(-24 * time.Hour)
	q := fmt.Sprintf(`SELECT c.devpod_uid FROM (
			SELECT devpod_uid, avgMerge(cpu_avg) AS cpu, maxMerge(cpu_limit) AS lim, uniqExact(ts) AS minutes
			FROM %[1]s.metrics_container_1m WHERE ts >= ? AND ts < ? GROUP BY devpod_uid) AS c
		LEFT JOIN (
			SELECT devpod_uid, avgMerge(util_avg) AS util
			FROM %[1]s.metrics_gpu_1m WHERE ts >= ? AND ts < ? GROUP BY devpod_uid) AS g USING devpod_uid
		WHERE c.minutes >= %[2]d
		  AND if(c.lim > 0, c.cpu / c.lim, c.cpu) * 100 < ?
		  AND ifNull(g.util, 0) < ?
		  AND c.devpod_uid NOT IN (
			SELECT devpod_uid FROM %[1]s.sessions FINAL
			WHERE opened_at >= ? AND (opened_at >= ? OR closed_at IS NULL OR closed_at >= ?))
		ORDER BY c.devpod_uid`, c.db, idleCoverage)
	rows, err := c.conn.Query(c.readCtx(ctx), q, from, now, from, now, cpuPct, gpuPct, now.Add(-90*24*time.Hour), from, from)
	if err != nil {
		return nil, fmt.Errorf("idle candidates: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

// PendingAttributions lists ssh connections whose gateway session was
// not known yet when they were ingested (spec §5.4 backfill).
func (c *CH) PendingAttributions(ctx context.Context, since time.Time) ([]SSHConnRow, error) {
	var out []SSHConnRow
	err := c.conn.Select(c.readCtx(ctx), &out, fmt.Sprintf("SELECT %s FROM %s.ssh_conns FINAL WHERE first_seen >= ? AND session_id = 'pending'", columns[SSHConnRow](), c.db), since)
	if err != nil {
		return nil, fmt.Errorf("pending attributions: %w", err)
	}
	return out, nil
}

// SessionRef identifies a gateway session for attribution.
type SessionRef struct {
	SessionID string
	User      string
	OpenedAt  time.Time
	ClosedAt  *time.Time
}

// OpenSessionsByBackend maps devpod uid → backend_local_addr → session for
// sessions opened since since that are open or closed after since.
func (c *CH) OpenSessionsByBackend(ctx context.Context, since time.Time) (map[string]map[string]SessionRef, error) {
	q := fmt.Sprintf(`SELECT devpod_uid, backend_local_addr, session_id, user, opened_at, closed_at FROM %s.sessions FINAL
		WHERE opened_at >= ? AND backend_local_addr != '' AND (closed_at IS NULL OR closed_at >= ?)`, c.db)
	rows, err := c.conn.Query(c.readCtx(ctx), q, since, since)
	if err != nil {
		return nil, fmt.Errorf("sessions by backend: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]SessionRef{}
	for rows.Next() {
		var uid, addr string
		var r SessionRef
		if err := rows.Scan(&uid, &addr, &r.SessionID, &r.User, &r.OpenedAt, &r.ClosedAt); err != nil {
			return nil, err
		}
		if out[uid] == nil {
			out[uid] = map[string]SessionRef{}
		}
		if prev, ok := out[uid][addr]; !ok || r.OpenedAt.After(prev.OpenedAt) {
			out[uid][addr] = r // a reused source port: the newest session wins
		}
	}
	return out, rows.Err()
}

// HookMissing lists sessions that closed in [from, to), lasted ≥ 60 s
// and typed ≥ 1 KiB into a pty, yet recorded no shell-hook command
// (spec §5.8).
func (c *CH) HookMissing(ctx context.Context, from, to time.Time) ([]SessionRow, error) {
	lo := from.Add(-7 * 24 * time.Hour)
	q := fmt.Sprintf(`SELECT %[2]s FROM %[1]s.sessions FINAL
		WHERE opened_at >= ? AND closed_at >= ? AND closed_at < ? AND dateDiff('second', opened_at, closed_at) >= 60
		  AND session_id IN (SELECT session_id FROM %[1]s.channels WHERE opened_at >= ? AND pty AND bytes_in >= 1024)
		  AND session_id NOT IN (SELECT session_id FROM %[1]s.commands WHERE ts >= ? AND source = 'shell_hook')`, c.db, columns[SessionRow]())
	var out []SessionRow
	if err := c.conn.Select(c.readCtx(ctx), &out, q, lo, from, to, lo, lo); err != nil {
		return nil, fmt.Errorf("hook missing: %w", err)
	}
	return out, nil
}

// LastSessionByUser returns each user's most recent session start.
func (c *CH) LastSessionByUser(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	rows, err := c.conn.Query(c.readCtx(ctx), fmt.Sprintf("SELECT user, max(opened_at) FROM %s.sessions WHERE opened_at >= ? GROUP BY user", c.db), since)
	if err != nil {
		return nil, fmt.Errorf("last session: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var u string
		var t time.Time
		if err := rows.Scan(&u, &t); err != nil {
			return nil, err
		}
		out[u] = t
	}
	return out, rows.Err()
}

// PendingProcesses lists process rows whose session was unknown at
// ingest (spec §5.4 backfill).
func (c *CH) PendingProcesses(ctx context.Context, since time.Time) ([]ProcessRow, error) {
	var out []ProcessRow
	err := c.conn.Select(c.readCtx(ctx), &out, fmt.Sprintf("SELECT %s FROM %s.processes FINAL WHERE first_seen >= ? AND session_id = 'pending'", columns[ProcessRow](), c.db), since)
	if err != nil {
		return nil, fmt.Errorf("pending processes: %w", err)
	}
	return out, nil
}

// PendingCommand is a shell-hook command whose session was unknown at
// ingest, with the peer of the shell process that ran it.
type PendingCommand struct {
	CommandRow
	Peer            string    `ch:"peer"`
	ProcAttribution string    `ch:"proc_attribution"`
	ProcFirstSeen   time.Time `ch:"proc_first_seen"`
}

// PendingCommands joins pending commands to their shell's process row
// (same DevPod and pid, alive at the command's time) to recover the
// peer; procSince bounds how old that shell may be.
func (c *CH) PendingCommands(ctx context.Context, since, procSince time.Time) ([]PendingCommand, error) {
	q := fmt.Sprintf(`SELECT c.event_id AS event_id, c.ts AS ts, c.session_id AS session_id, c.devpod_uid AS devpod_uid, c.user AS user,
			c.source AS source, c.channel_id AS channel_id, c.pid AS pid, c.cwd AS cwd, c.cmdline AS cmdline, c.exit_code AS exit_code,
			c.duration_ms AS duration_ms, c.attribution AS attribution, c.version AS version, c.cluster AS cluster,
			p.peer AS peer, p.attribution AS proc_attribution, p.first_seen AS proc_first_seen
		FROM (SELECT * FROM %[1]s.commands FINAL WHERE ts >= ? AND session_id = 'pending') AS c
		INNER JOIN (SELECT devpod_uid, pid, peer, attribution, first_seen, exit_seen FROM %[1]s.processes FINAL
		            WHERE first_seen >= ? AND peer != '') AS p
		ON c.devpod_uid = p.devpod_uid AND c.pid = toInt32(p.pid)
		WHERE p.first_seen <= c.ts AND (p.exit_seen IS NULL OR p.exit_seen >= c.ts)`, c.db)
	var rows []PendingCommand
	if err := c.conn.Select(c.readCtx(ctx), &rows, q, since, procSince); err != nil {
		return nil, fmt.Errorf("pending commands: %w", err)
	}
	// A reused pid can match an older shell too: keep the newest.
	best := map[string]int{}
	var out []PendingCommand
	for _, r := range rows {
		if i, ok := best[r.EventID]; ok {
			if r.ProcFirstSeen.After(out[i].ProcFirstSeen) {
				out[i] = r
			}
			continue
		}
		best[r.EventID] = len(out)
		out = append(out, r)
	}
	return out, nil
}

// BackendSession is a gateway session with its backend address.
type BackendSession struct {
	UID       string     `ch:"devpod_uid"`
	Addr      string     `ch:"backend_local_addr"`
	SessionID string     `ch:"session_id"`
	User      string     `ch:"user"`
	OpenedAt  time.Time  `ch:"opened_at"`
	ClosedAt  *time.Time `ch:"closed_at"`
}

// BackendSessions lists sessions opened since since that have a backend
// address (all of them, unlike OpenSessionsByBackend's newest-per-addr).
func (c *CH) BackendSessions(ctx context.Context, since time.Time) ([]BackendSession, error) {
	var out []BackendSession
	q := fmt.Sprintf(`SELECT devpod_uid, backend_local_addr, session_id, user, opened_at, closed_at FROM %s.sessions FINAL
		WHERE opened_at >= ? AND backend_local_addr != ''`, c.db)
	if err := c.conn.Select(c.readCtx(ctx), &out, q, since); err != nil {
		return nil, fmt.Errorf("backend sessions: %w", err)
	}
	return out, nil
}

// InfoSeen returns each DevPod's latest container_info time since since.
func (c *CH) InfoSeen(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	rows, err := c.conn.Query(c.readCtx(ctx), fmt.Sprintf("SELECT devpod_uid, max(ts) FROM %s.container_info WHERE ts >= ? GROUP BY devpod_uid", c.db), since)
	if err != nil {
		return nil, fmt.Errorf("info seen: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var uid string
		var t time.Time
		if err := rows.Scan(&uid, &t); err != nil {
			return nil, err
		}
		out[uid] = t
	}
	return out, rows.Err()
}

// HooksSince returns, per DevPod, the first container_info since since
// from an agent that runs shell hooks (hook_missing is judged only there).
func (c *CH) HooksSince(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	q := fmt.Sprintf(`SELECT devpod_uid, min(ts) FROM %s.container_info
		WHERE ts >= ? AND JSONExtractString(info, 'collectors', 'hooks') = 'ok' GROUP BY devpod_uid`, c.db)
	rows, err := c.conn.Query(c.readCtx(ctx), q, since)
	if err != nil {
		return nil, fmt.Errorf("hooks since: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var uid string
		var t time.Time
		if err := rows.Scan(&uid, &t); err != nil {
			return nil, err
		}
		out[uid] = t
	}
	return out, rows.Err()
}

// UnattributedCounts counts, per DevPod, processes first seen in
// [from, to) that no rule attributed (the minute's unattributed alert).
func (c *CH) UnattributedCounts(ctx context.Context, from, to time.Time) (map[string]Unattributed, error) {
	q := fmt.Sprintf(`SELECT devpod_uid, any(cluster), count() FROM %s.processes FINAL
		WHERE first_seen >= ? AND first_seen < ? AND attribution = 'unattributed' AND pid != 1 GROUP BY devpod_uid`, c.db)
	rows, err := c.conn.Query(c.readCtx(ctx), q, from, to)
	if err != nil {
		return nil, fmt.Errorf("unattributed counts: %w", err)
	}
	defer rows.Close()
	out := map[string]Unattributed{}
	for rows.Next() {
		var uid, cluster string
		var n uint64
		if err := rows.Scan(&uid, &cluster, &n); err != nil {
			return nil, err
		}
		out[uid] = Unattributed{Cluster: cluster, N: n}
	}
	return out, rows.Err()
}

// Unattributed: how many processes of a DevPod no rule attributed, and
// the cluster it is in.
type Unattributed struct {
	Cluster string
	N       uint64
}

// RecentSeries is a short CPU / memory history for many DevPods at once
// (the console's sparklines, UI feedback round 1): buckets of Step
// seconds, series aligned with TS, nil where a bucket had no sample.
type RecentSeries struct {
	Step  int64                 `json:"step"`
	TS    []int64               `json:"ts"`
	Items map[string]RecentItem `json:"items"` // by DevPod UID
}

// RecentItem is one DevPod's series: CPU in cores, Mem the working set.
type RecentItem struct {
	CPU []*float64 `json:"cpu"`
	Mem []*float64 `json:"mem"`
}

// RecentContainer reads the 1 s container table for uids over
// [from, to); uids without a sample in the window are left out.
func (c *CH) RecentContainer(ctx context.Context, uids []string, from, to time.Time, step time.Duration) (*RecentSeries, error) {
	secs := int64(step / time.Second)
	if secs <= 0 {
		secs = 15
	}
	start := from.Unix() / secs * secs
	n := int((to.Unix()-start)/secs) + 1
	out := &RecentSeries{Step: secs, TS: make([]int64, n), Items: map[string]RecentItem{}}
	for i := range out.TS {
		out.TS[i] = start + int64(i)*secs
	}
	if len(uids) == 0 {
		return out, nil
	}
	q := fmt.Sprintf(`SELECT devpod_uid, toUnixTimestamp(toStartOfInterval(ts, INTERVAL %d SECOND)) AS b, avg(cpu), avg(mem_ws)
		FROM %s.metrics_container_1s WHERE devpod_uid IN ? AND ts >= ? AND ts < ? GROUP BY devpod_uid, b ORDER BY devpod_uid, b`, secs, c.db)
	rows, err := c.conn.Query(c.readCtx(ctx), q, uids, time.Unix(start, 0).UTC(), to)
	if err != nil {
		return nil, fmt.Errorf("recent container series: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		var b uint32
		var cpu, mem float64
		if err := rows.Scan(&uid, &b, &cpu, &mem); err != nil {
			return nil, err
		}
		i := int((int64(b) - start) / secs)
		if i < 0 || i >= n {
			continue
		}
		it, ok := out.Items[uid]
		if !ok {
			it = RecentItem{CPU: make([]*float64, n), Mem: make([]*float64, n)}
		}
		it.CPU[i], it.Mem[i] = &cpu, &mem
		out.Items[uid] = it
	}
	return out, rows.Err()
}
