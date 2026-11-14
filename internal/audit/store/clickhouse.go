// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// EventStore is the row side of the audit store.
type EventStore interface {
	Insert(ctx context.Context, b Batch, token string) error
}

// CHConfig locates the audit database.
type CHConfig struct {
	URL      string // clickhouse://host:9000
	Database string // default devpod_audit
	Username string
	Password string
}

// LoadCHCredentials reads files username and password from dir.
func LoadCHCredentials(dir string) (user, pass string, err error) {
	if user, err = readTrimmed(filepath.Join(dir, "username")); err != nil {
		return "", "", err
	}
	if pass, err = readTrimmed(filepath.Join(dir, "password")); err != nil {
		return "", "", err
	}
	return user, pass, nil
}

var identRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// CH is a ClickHouse-backed EventStore (and, see query.go, the reader).
type CH struct {
	conn driver.Conn
	db   string
	// readExtra is merged into every read query's settings (tests set
	// force_index_by_date to prove each query is time-bounded).
	readExtra clickhouse.Settings
	// tables: the columns each table has, as last read (see known)
	tables *tableCols
}

// CheckCHURL says whether the client takes url: one it does not never
// connects, so a gateway that connects only when it first writes checks it
// when it starts. (An https:// address needs ?secure=true.)
func CheckCHURL(url string) error {
	if _, err := clickhouse.ParseDSN(url); err != nil {
		return fmt.Errorf("clickhouse url: %w", err)
	}
	return nil
}

// OpenCH connects and pings.
func OpenCH(ctx context.Context, cfg CHConfig) (*CH, error) {
	db := cfg.Database
	if db == "" {
		db = "devpod_audit"
	}
	if !identRE.MatchString(db) {
		return nil, fmt.Errorf("clickhouse: invalid database name %q", db)
	}
	opts, err := clickhouse.ParseDSN(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("clickhouse url: %w", err)
	}
	opts.Auth = clickhouse.Auth{Username: cfg.Username, Password: cfg.Password}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	return &CH{conn: conn, db: db, tables: &tableCols{m: map[string]colSet{}}}, nil
}

// Close releases the connection pool.
func (c *CH) Close() error { return c.conn.Close() }

// Insert writes every non-empty table of b. Each table insert carries
// insert_deduplication_token = token+"/"+table, so a retried batch is a
// no-op.
func (c *CH) Insert(ctx context.Context, b Batch, token string) error {
	// a session's rows expire by last_active: one written without it (a
	// writer of before, a test) has its close or opening, as the column's
	// default gives a writer that does not name it — never 1970, gone at once
	for i := range b.Sessions {
		if r := &b.Sessions[i]; r.LastActive.IsZero() {
			r.LastActive = r.OpenedAt
			if r.ClosedAt != nil {
				r.LastActive = *r.ClosedAt
			}
		}
	}
	var errs []error
	errs = append(errs, insertRows(ctx, c, "sessions", token, b.Sessions))
	errs = append(errs, insertRows(ctx, c, "channels", token, b.Channels))
	errs = append(errs, insertRows(ctx, c, "commands", token, b.Commands))
	errs = append(errs, insertRows(ctx, c, "file_events", token, b.FileEvents))
	errs = append(errs, insertRows(ctx, c, "policy_events", token, b.PolicyEvents))
	errs = append(errs, insertRows(ctx, c, "raw_segments", token, b.RawSegments))
	errs = append(errs, insertRows(ctx, c, "alerts", token, b.Alerts))
	errs = append(errs, insertRows(ctx, c, "http_requests", token, b.HTTPRequests))
	return errors.Join(errs...)
}

func insertRows[T any](ctx context.Context, c *CH, table, token string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"insert_deduplicate":         1,
		"insert_deduplication_token": token + "/" + table,
	}))
	// the columns named: a table that has more (a later version's) takes
	// their defaults, so adding a column never breaks a writer
	batch, err := c.conn.PrepareBatch(ctx, fmt.Sprintf("INSERT INTO %s.%s (%s)", c.db, table, c.known(ctx, table, columns[T]())))
	if err != nil {
		return fmt.Errorf("prepare %s: %w", table, err)
	}
	for i := range rows {
		if err := batch.AppendStruct(&rows[i]); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("append %s: %w", table, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("insert %s: %w", table, err)
	}
	return nil
}
