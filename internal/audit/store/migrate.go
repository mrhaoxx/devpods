// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Retention is the per-class data retention (spec §8.5).
type Retention struct {
	AuditDays      int // audit tables and container-side event tables
	Metrics1sDays  int // 1-second container / GPU / RDMA metrics
	Metrics1mDays  int // 1-minute rollups
	Process1sHours int // 1-second process metrics
	SnapshotDays   int // container_snapshots
}

// DefaultRetention is spec §8.5's defaults with the given audit days.
func DefaultRetention(auditDays int) Retention {
	return Retention{AuditDays: auditDays, Metrics1sDays: 7, Metrics1mDays: 90, Process1sHours: 48, SnapshotDays: 7}
}

type ttlRule struct {
	col  string
	unit string // DAY | HOUR
	n    func(Retention) int
}

func auditDays(r Retention) int { return r.AuditDays }

// ttlRules maps every table to its TTL column and retention class.
var ttlRules = map[string]ttlRule{
	"sessions":             {"last_active", "DAY", auditDays}, // 0007: by last activity, not opening
	"channels":             {"opened_at", "DAY", auditDays},
	"commands":             {"ts", "DAY", auditDays},
	"file_events":          {"ts", "DAY", auditDays},
	"policy_events":        {"ts", "DAY", auditDays},
	"raw_segments":         {"first_ts", "DAY", auditDays},
	"alerts":               {"ts", "DAY", auditDays},
	"access_log":           {"ts", "DAY", auditDays},
	"http_requests":        {"ts", "DAY", auditDays},
	"ssh_conns":            {"first_seen", "DAY", auditDays},
	"processes":            {"first_seen", "DAY", auditDays},
	"container_info":       {"ts", "DAY", auditDays},
	"container_snapshots":  {"ts", "DAY", func(r Retention) int { return r.SnapshotDays }},
	"metrics_container_1s": {"ts", "DAY", func(r Retention) int { return r.Metrics1sDays }},
	"metrics_gpu_1s":       {"ts", "DAY", func(r Retention) int { return r.Metrics1sDays }},
	"metrics_rdma_1s":      {"ts", "DAY", func(r Retention) int { return r.Metrics1sDays }},
	"metrics_process_1s":   {"ts", "HOUR", func(r Retention) int { return r.Process1sHours }},
	"metrics_container_1m": {"ts", "DAY", func(r Retention) int { return r.Metrics1mDays }},
	"metrics_gpu_1m":       {"ts", "DAY", func(r Retention) int { return r.Metrics1mDays }},
	"metrics_rdma_1m":      {"ts", "DAY", func(r Retention) int { return r.Metrics1mDays }},
}

// steps run after a migration's SQL, before it is recorded: what SQL
// alone cannot say (a value per distinct row).
var steps = map[uint32]func(context.Context, *CH) error{4: foldOrigins}

// Migrate creates the database, applies pending migrations in order
// (recorded in schema_migrations) and (re)applies the retention TTLs.
// Safe to run on every helm upgrade.
func Migrate(ctx context.Context, c *CH, r Retention) error {
	return migrate(ctx, c, r, ^uint32(0))
}

// migrate is Migrate up to version upTo (a test sets up an older schema).
func migrate(ctx context.Context, c *CH, r Retention, upTo uint32) error {
	for table, rule := range ttlRules {
		if rule.n(r) <= 0 {
			return fmt.Errorf("retention for %s must be positive, got %d", table, rule.n(r))
		}
	}
	if err := c.conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", c.db)); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	if err := c.conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.schema_migrations (
		version UInt32, applied_at DateTime DEFAULT now()) ENGINE = MergeTree ORDER BY version`, c.db)); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[uint32]bool{}
	rows, err := c.conn.Query(ctx, fmt.Sprintf("SELECT version FROM %s.schema_migrations", c.db))
	if err != nil {
		return err
	}
	for rows.Next() {
		var v uint32
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		v, err := strconv.ParseUint(strings.SplitN(e.Name(), "_", 2)[0], 10, 32)
		if err != nil {
			return fmt.Errorf("migration %s: bad version prefix", e.Name())
		}
		if applied[uint32(v)] || uint32(v) > upTo {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(strings.ReplaceAll(string(body), "{db}", c.db)) {
			if err := c.conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s: %w", e.Name(), err)
			}
		}
		if step := steps[uint32(v)]; step != nil {
			if err := step(ctx, c); err != nil {
				return fmt.Errorf("migration %s: %w", e.Name(), err)
			}
		}
		if err := c.conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s.schema_migrations (version) VALUES (?)", c.db), uint32(v)); err != nil {
			return fmt.Errorf("record migration %s: %w", e.Name(), err)
		}
	}

	if upTo != ^uint32(0) {
		return nil // an older schema (a test's) lacks later tables; the full run that follows sets retention
	}
	// Do not rewrite existing parts on a TTL change; merges apply it.
	ttlCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"materialize_ttl_after_modify": 0}))
	for table, rule := range ttlRules {
		q := fmt.Sprintf("ALTER TABLE %s.%s MODIFY TTL toDateTime(%s) + INTERVAL %d %s", c.db, table, rule.col, rule.n(r), rule.unit)
		if err := c.conn.Exec(ttlCtx, q); err != nil {
			return fmt.Errorf("ttl %s: %w", table, err)
		}
	}
	return nil
}

// splitStatements splits on lines holding only ";" and drops comment-
// only chunks.
func splitStatements(sql string) []string {
	var out []string
	var cur []string
	flush := func() {
		var keep []string
		for _, l := range cur {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "--") {
				keep = append(keep, l)
			}
		}
		if len(keep) > 0 {
			out = append(out, strings.Join(keep, "\n"))
		}
		cur = nil
	}
	for _, line := range strings.Split(sql, "\n") {
		if strings.TrimSpace(line) == ";" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

// foldOrigins (0004): each session takes the cluster and gateway that
// session_origin held for it; what was recorded in a session takes its
// cluster (commands, file events, policy events and alerts outside any
// session: that of their DevPod's sessions); then session_origin goes.
// Rows of DevPods that never had a session through a gateway of several
// clusters keep "" — what wrote them did not say.
func foldOrigins(ctx context.Context, c *CH) error {
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 2, "allow_nondeterministic_mutations": 1}))
	db := c.db
	exists := uint8(0)
	if err := c.conn.QueryRow(ctx, fmt.Sprintf("EXISTS TABLE %s.session_origin", db)).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		pairs, err := distinct2(ctx, c, fmt.Sprintf("SELECT DISTINCT cluster, gateway FROM %s.session_origin", db))
		if err != nil {
			return err
		}
		for _, p := range pairs {
			if err := c.conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %[1]s.sessions UPDATE cluster = ?, gateway = ? WHERE session_id IN (SELECT session_id FROM %[1]s.session_origin WHERE cluster = ? AND gateway = ?)", db),
				p[0], p[1], p[0], p[1]); err != nil {
				return fmt.Errorf("sessions: %w", err)
			}
		}
	}
	clusters, err := distinct2(ctx, c, fmt.Sprintf("SELECT DISTINCT cluster, '' FROM %s.sessions WHERE cluster != ''", db))
	if err != nil {
		return err
	}
	for _, cl := range clusters {
		for _, t := range []string{"channels", "commands", "file_events", "policy_events", "raw_segments", "alerts"} {
			if err := c.conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %[1]s.%[2]s UPDATE cluster = ? WHERE cluster = '' AND session_id IN (SELECT session_id FROM %[1]s.sessions WHERE cluster = ?)", db, t),
				cl[0], cl[0]); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
		}
		for _, t := range []string{"commands", "file_events", "policy_events", "alerts"} {
			if err := c.conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %[1]s.%[2]s UPDATE cluster = ? WHERE cluster = '' AND devpod_uid != '' AND devpod_uid IN (SELECT devpod_uid FROM %[1]s.sessions WHERE cluster = ?)", db, t),
				cl[0], cl[0]); err != nil {
				return fmt.Errorf("%s by devpod: %w", t, err)
			}
		}
	}
	return c.conn.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s.session_origin", db))
}

// distinct2 reads a two-column query.
func distinct2(ctx context.Context, c *CH, q string) ([][2]string, error) {
	rows, err := c.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, b})
	}
	return out, rows.Err()
}
