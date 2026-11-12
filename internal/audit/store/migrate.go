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
	"sessions":             {"opened_at", "DAY", auditDays},
	"session_origin":       {"opened_at", "DAY", auditDays},
	"channels":             {"opened_at", "DAY", auditDays},
	"commands":             {"ts", "DAY", auditDays},
	"file_events":          {"ts", "DAY", auditDays},
	"policy_events":        {"ts", "DAY", auditDays},
	"raw_segments":         {"first_ts", "DAY", auditDays},
	"alerts":               {"ts", "DAY", auditDays},
	"access_log":           {"ts", "DAY", auditDays},
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

// Migrate creates the database, applies pending migrations in order
// (recorded in schema_migrations) and (re)applies the retention TTLs.
// Safe to run on every helm upgrade.
func Migrate(ctx context.Context, c *CH, r Retention) error {
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
		if applied[uint32(v)] {
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
		if err := c.conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s.schema_migrations (version) VALUES (?)", c.db), uint32(v)); err != nil {
			return fmt.Errorf("record migration %s: %w", e.Name(), err)
		}
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
