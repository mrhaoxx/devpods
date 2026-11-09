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

// ttlColumns maps every audit table to the time column its TTL keys on.
var ttlColumns = map[string]string{
	"sessions":      "opened_at",
	"channels":      "opened_at",
	"commands":      "ts",
	"file_events":   "ts",
	"policy_events": "ts",
	"raw_segments":  "first_ts",
	"alerts":        "ts",
	"access_log":    "ts",
}

// Migrate creates the database, applies pending migrations in order
// (recorded in schema_migrations) and (re)applies the retention TTL to
// every audit table. Safe to run on every helm upgrade.
func Migrate(ctx context.Context, c *CH, retentionDays int) error {
	if retentionDays <= 0 {
		return fmt.Errorf("retention days must be positive, got %d", retentionDays)
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
	for table, col := range ttlColumns {
		q := fmt.Sprintf("ALTER TABLE %s.%s MODIFY TTL toDateTime(%s) + INTERVAL %d DAY", c.db, table, col, retentionDays)
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
