// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/oklog/ulid/v2"
)

var (
	// ErrNotFound is returned for an unknown session / file event.
	ErrNotFound = errors.New("not found")
	// ErrRangeRequired is returned when a list query has no time range.
	ErrRangeRequired = errors.New("a time range (from, to) is required")
	errBadCursor     = errors.New("invalid cursor")
)

const (
	defaultLimit = 100
	maxLimit     = 1000
)

// SessionFilter selects sessions opened in [From, To).
type SessionFilter struct {
	User, DevPodUID     string
	Cluster             string // the cluster of its DevPod
	From, To            time.Time
	HasFiles, HasAlerts bool // HasAlerts: any alert, policy hit or gap
	Cursor              string
	Limit               int
}

// SessionSummary is a session plus per-session counters.
type SessionSummary struct {
	SessionRow
	Commands   int `json:"commands"`
	Files      int `json:"files"`
	PolicyHits int `json:"policy_hits"`
	Alerts     int `json:"alerts"`
}

// SessionDetail is everything recorded for one session.
type SessionDetail struct {
	Session  SessionRow       `json:"session"`
	Cluster  string           `json:"cluster,omitempty"`
	Gateway  string           `json:"gateway,omitempty"`
	Channels []ChannelRow     `json:"channels"`
	Commands []CommandRow     `json:"commands"`
	Files    []FileEventRow   `json:"files"`
	Policy   []PolicyEventRow `json:"policy"`
	Alerts   []AlertRow       `json:"alerts"`
	Segments []RawSegmentRow  `json:"segments"`
}

// CommandFilter searches commands by substring.
type CommandFilter struct {
	User, DevPodUID, Q string
	Cluster            string // the cluster of its DevPod
	From, To           time.Time
	Cursor             string
	Limit              int
}

// FileFilter searches file events by path substring or exact sha256.
type FileFilter struct {
	User, DevPodUID, Path, SHA256 string
	Cluster                       string // the cluster of its DevPod
	From, To                      time.Time
	Cursor                        string
	Limit                         int
}

// HTTPFilter searches HTTP requests; zero fields do not filter.
type HTTPFilter struct {
	User, DevPodUID, Cluster, Method string
	Port, Status                     int
	From, To                         time.Time
	Cursor                           string
	Limit                            int
}

// TimeFilter pages a time-ordered log.
type TimeFilter struct {
	From, To time.Time
	// Cluster: the cluster of the row's DevPod, for a log whose rows are
	// a DevPod's (alerts); the access log has none.
	Cluster string
	Cursor  string
	Limit   int
}

func (c *CH) readCtx(ctx context.Context) context.Context {
	set := clickhouse.Settings{"max_execution_time": 10}
	for k, v := range c.readExtra {
		set[k] = v
	}
	return clickhouse.Context(ctx, clickhouse.WithSettings(set))
}

func limitOf(n int) int {
	if n <= 0 {
		return defaultLimit
	}
	return min(n, maxLimit)
}

func encodeCursor(t time.Time, key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "|" + key))
}

func decodeCursor(c string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", errBadCursor
	}
	ns, key, ok := strings.Cut(string(b), "|")
	if !ok {
		return time.Time{}, "", errBadCursor
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, "", errBadCursor
	}
	return time.Unix(0, n).UTC(), key, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// where accumulates conditions and their bound args.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) sql() string { return strings.Join(w.conds, " AND ") }

func (w *where) cursor(c, timeCol, keyExpr string) error {
	if c == "" {
		return nil
	}
	t, key, err := decodeCursor(c)
	if err != nil {
		return err
	}
	w.add(fmt.Sprintf("(%s, %s) < (?, ?)", timeCol, keyExpr), t, key)
	return nil
}

// ListSessions returns sessions newest first and the next page's cursor
// ("" when there is none).
func (c *CH) ListSessions(ctx context.Context, f SessionFilter) ([]SessionSummary, string, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return nil, "", ErrRangeRequired
	}
	ctx = c.readCtx(ctx)
	limit := limitOf(f.Limit)
	w := &where{}
	w.add("opened_at >= ? AND opened_at < ?", f.From, f.To)
	if f.User != "" {
		w.add("user = ?", f.User)
	}
	if f.DevPodUID != "" {
		w.add("devpod_uid = ?", f.DevPodUID)
	}
	if f.Cluster != "" {
		w.add("cluster = ?", f.Cluster)
	}
	if f.HasFiles {
		w.add(fmt.Sprintf("session_id IN (SELECT session_id FROM %s.file_events WHERE ts >= ?)", c.db), f.From)
	}
	if f.HasAlerts {
		w.add(fmt.Sprintf("(gaps > 0 OR session_id IN (SELECT session_id FROM %[1]s.alerts WHERE ts >= ?) OR session_id IN (SELECT session_id FROM %[1]s.policy_events WHERE ts >= ?))", c.db), f.From, f.From)
	}
	if err := w.cursor(f.Cursor, "opened_at", "session_id"); err != nil {
		return nil, "", err
	}
	var rows []SessionRow
	q := fmt.Sprintf("SELECT %s FROM %s.sessions FINAL WHERE %s ORDER BY opened_at DESC, session_id DESC LIMIT %d", columns[SessionRow](), c.db, w.sql(), limit+1)
	if err := c.conn.Select(ctx, &rows, q, w.args...); err != nil {
		return nil, "", fmt.Errorf("list sessions: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = encodeCursor(last.OpenedAt, last.SessionID)
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.SessionID
	}
	counts := map[string]*SessionSummary{}
	out := make([]SessionSummary, len(rows))
	for i, r := range rows {
		out[i] = SessionSummary{SessionRow: r}
		counts[r.SessionID] = &out[i]
	}
	if len(ids) > 0 {
		// Events never predate their session: bound the counts by the
		// page's oldest session so they prune partitions.
		lo := rows[0].OpenedAt
		for _, r := range rows {
			if r.OpenedAt.Before(lo) {
				lo = r.OpenedAt
			}
		}
		lo = lo.Add(-eventSkew)
		for table, field := range map[string]func(*SessionSummary) *int{
			"commands FINAL": func(s *SessionSummary) *int { return &s.Commands },
			"file_events":    func(s *SessionSummary) *int { return &s.Files },
			"policy_events":  func(s *SessionSummary) *int { return &s.PolicyHits },
			"alerts":         func(s *SessionSummary) *int { return &s.Alerts },
		} {
			r, err := c.conn.Query(ctx, fmt.Sprintf("SELECT session_id, count() FROM %s.%s WHERE has(?, session_id) AND ts >= ? GROUP BY session_id", c.db, table), ids, lo)
			if err != nil {
				return nil, "", fmt.Errorf("count %s: %w", table, err)
			}
			for r.Next() {
				var id string
				var n uint64
				if err := r.Scan(&id, &n); err != nil {
					r.Close()
					return nil, "", err
				}
				if s := counts[id]; s != nil {
					*field(s) = int(n)
				}
			}
			r.Close()
		}
	}
	return out, next, nil
}

// eventSkew is how far an event's timestamp may precede its session's
// opened_at (clock differences between writers).
const eventSkew = time.Minute

// idSkew bounds the distance between the time encoded in a ULID id and
// the row's own timestamp.
const idSkew = time.Hour

// idTime is the creation time encoded in a ULID id (session and file
// event ids); ok is false for anything else.
func idTime(id string) (time.Time, bool) {
	u, err := ulid.ParseStrict(id)
	if err != nil {
		return time.Time{}, false
	}
	return ulid.Time(u.Time()), true
}

// GetSession loads one session with all of its rows. Every query is
// bounded by the time encoded in the session's ULID.
func (c *CH) GetSession(ctx context.Context, id string) (*SessionDetail, error) {
	at, ok := idTime(id)
	if !ok {
		return nil, ErrNotFound
	}
	ctx = c.readCtx(ctx)
	var sessions []SessionRow
	if err := c.conn.Select(ctx, &sessions, fmt.Sprintf("SELECT %s FROM %s.sessions FINAL WHERE session_id = ? AND opened_at >= ? AND opened_at < ? LIMIT 1", columns[SessionRow](), c.db),
		id, at.Add(-idSkew), at.Add(idSkew)); err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if len(sessions) == 0 {
		return nil, ErrNotFound
	}
	d := &SessionDetail{Session: sessions[0], Cluster: sessions[0].Cluster, Gateway: sessions[0].Gateway}
	lo := d.Session.OpenedAt.Add(-eventSkew)
	sel := func(dst any, cols, table, cond, order string, args ...any) error {
		q := fmt.Sprintf("SELECT %s FROM %s.%s WHERE session_id = ? AND %s ORDER BY %s", cols, c.db, table, cond, order)
		return c.conn.Select(ctx, dst, q, append([]any{id}, args...)...)
	}
	uid := d.Session.DevPodUID
	if err := errors.Join(
		// Latest version per channel without FINAL: FINAL would apply
		// the opened_at filter only after merging, losing partition pruning.
		sel(&d.Channels, columns[ChannelRow](), "channels", "opened_at >= ?", "channel_id, version DESC LIMIT 1 BY channel_id", lo),
		sel(&d.Commands, columns[CommandRow](), "commands FINAL", "devpod_uid = ? AND ts >= ?", "ts, event_id", uid, lo),
		sel(&d.Files, columns[FileEventRow](), "file_events", "devpod_uid = ? AND ts >= ?", "ts, event_id", uid, lo),
		sel(&d.Policy, columns[PolicyEventRow](), "policy_events", "ts >= ?", "ts", lo),
		sel(&d.Alerts, columns[AlertRow](), "alerts", "ts >= ?", "ts", lo),
		sel(&d.Segments, columns[RawSegmentRow](), "raw_segments", "first_ts >= ?", "seq", lo),
	); err != nil {
		return nil, fmt.Errorf("get session %s: %w", id, err)
	}
	return d, nil
}

// SearchCommands finds commands whose command line contains Q.
func (c *CH) SearchCommands(ctx context.Context, f CommandFilter) ([]CommandRow, string, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return nil, "", ErrRangeRequired
	}
	w := &where{}
	w.add("ts >= ? AND ts < ?", f.From, f.To)
	if f.User != "" {
		w.add("user = ?", f.User)
	}
	if f.DevPodUID != "" {
		w.add("devpod_uid = ?", f.DevPodUID)
	}
	if f.Cluster != "" {
		w.add("cluster = ?", f.Cluster)
	}
	if f.Q != "" {
		w.add("cmdline LIKE concat('%', ?, '%')", likeEscape(f.Q))
	}
	if err := w.cursor(f.Cursor, "ts", "event_id"); err != nil {
		return nil, "", err
	}
	limit := limitOf(f.Limit)
	var rows []CommandRow
	q := fmt.Sprintf("SELECT %s FROM %s.commands FINAL WHERE %s ORDER BY ts DESC, event_id DESC LIMIT %d", columns[CommandRow](), c.db, w.sql(), limit+1)
	if err := c.conn.Select(c.readCtx(ctx), &rows, q, w.args...); err != nil {
		return nil, "", fmt.Errorf("search commands: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(rows[limit-1].TS, rows[limit-1].EventID)
	}
	return rows, next, nil
}

// SearchFiles finds file events by path substring and/or sha256.
func (c *CH) SearchFiles(ctx context.Context, f FileFilter) ([]FileEventRow, string, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return nil, "", ErrRangeRequired
	}
	w := &where{}
	w.add("ts >= ? AND ts < ?", f.From, f.To)
	if f.User != "" {
		w.add("user = ?", f.User)
	}
	if f.DevPodUID != "" {
		w.add("devpod_uid = ?", f.DevPodUID)
	}
	if f.Cluster != "" {
		w.add("cluster = ?", f.Cluster)
	}
	if f.Path != "" {
		w.add("path LIKE concat('%', ?, '%')", likeEscape(f.Path))
	}
	if f.SHA256 != "" {
		w.add("sha256 = ?", f.SHA256)
	}
	if err := w.cursor(f.Cursor, "ts", "event_id"); err != nil {
		return nil, "", err
	}
	limit := limitOf(f.Limit)
	var rows []FileEventRow
	q := fmt.Sprintf("SELECT %s FROM %s.file_events WHERE %s ORDER BY ts DESC, event_id DESC LIMIT %d", columns[FileEventRow](), c.db, w.sql(), limit+1)
	if err := c.conn.Select(c.readCtx(ctx), &rows, q, w.args...); err != nil {
		return nil, "", fmt.Errorf("search files: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(rows[limit-1].TS, rows[limit-1].EventID)
	}
	return rows, next, nil
}

// SearchHTTP finds HTTP requests, newest first.
func (c *CH) SearchHTTP(ctx context.Context, f HTTPFilter) ([]HTTPRequestRow, string, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return nil, "", ErrRangeRequired
	}
	w := &where{}
	w.add("ts >= ? AND ts < ?", f.From, f.To)
	for _, eq := range []struct {
		col string
		v   any
		set bool
	}{
		{"user", f.User, f.User != ""}, {"devpod_uid", f.DevPodUID, f.DevPodUID != ""},
		{"cluster", f.Cluster, f.Cluster != ""}, {"method", f.Method, f.Method != ""},
		{"port", f.Port, f.Port != 0}, {"status", f.Status, f.Status != 0},
	} {
		if eq.set {
			w.add(eq.col+" = ?", eq.v)
		}
	}
	if err := w.cursor(f.Cursor, "ts", "request_id"); err != nil {
		return nil, "", err
	}
	limit := limitOf(f.Limit)
	var rows []HTTPRequestRow
	q := fmt.Sprintf("SELECT %s FROM %s.http_requests WHERE %s ORDER BY ts DESC, request_id DESC LIMIT %d", columns[HTTPRequestRow](), c.db, w.sql(), limit+1)
	if err := c.conn.Select(c.readCtx(ctx), &rows, q, w.args...); err != nil {
		return nil, "", fmt.Errorf("search http: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(rows[limit-1].TS, rows[limit-1].RequestID)
	}
	return rows, next, nil
}

// GetFileEvent loads one file event.
func (c *CH) GetFileEvent(ctx context.Context, eventID string) (*FileEventRow, error) {
	var rows []FileEventRow
	at, ok := idTime(eventID)
	if !ok {
		return nil, ErrNotFound
	}
	if err := c.conn.Select(c.readCtx(ctx), &rows, fmt.Sprintf("SELECT %s FROM %s.file_events WHERE event_id = ? AND ts >= ? AND ts < ? LIMIT 1", columns[FileEventRow](), c.db),
		eventID, at.Add(-idSkew), at.Add(idSkew)); err != nil {
		return nil, fmt.Errorf("get file event: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

const (
	alertKey  = "concat(kind, '|', session_id, '|', detail)"
	accessKey = "concat(viewer, '|', action, '|', target_session, '|', target_file)"
)

// ListAlerts pages alerts newest first.
func (c *CH) ListAlerts(ctx context.Context, f TimeFilter) ([]AlertRow, string, error) {
	var rows []AlertRow
	next, err := c.listLog(ctx, &rows, columns[AlertRow](), "alerts", alertKey, f, func() (time.Time, string, int) {
		if len(rows) == 0 {
			return time.Time{}, "", 0
		}
		l := rows[len(rows)-1]
		return l.TS, l.Kind + "|" + l.SessionID + "|" + l.Detail, len(rows)
	}, func(n int) { rows = rows[:n] })
	return rows, next, err
}

// ListAccessLog pages the access log newest first.
func (c *CH) ListAccessLog(ctx context.Context, f TimeFilter) ([]AccessLogRow, string, error) {
	var rows []AccessLogRow
	next, err := c.listLog(ctx, &rows, columns[AccessLogRow](), "access_log", accessKey, f, func() (time.Time, string, int) {
		if len(rows) == 0 {
			return time.Time{}, "", 0
		}
		l := rows[len(rows)-1]
		return l.TS, l.Viewer + "|" + l.Action + "|" + l.TargetSession + "|" + l.TargetFile, len(rows)
	}, func(n int) { rows = rows[:n] })
	return rows, next, err
}

func (c *CH) listLog(ctx context.Context, dst any, cols, table, keyExpr string, f TimeFilter, last func() (time.Time, string, int), trim func(int)) (string, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return "", ErrRangeRequired
	}
	w := &where{}
	w.add("ts >= ? AND ts < ?", f.From, f.To)
	if f.Cluster != "" && table == "alerts" {
		w.add("cluster = ?", f.Cluster)
	}
	if err := w.cursor(f.Cursor, "ts", keyExpr); err != nil {
		return "", err
	}
	limit := limitOf(f.Limit)
	q := fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s ORDER BY ts DESC, %s DESC LIMIT %d", cols, c.db, table, w.sql(), keyExpr, limit+1)
	if err := c.conn.Select(c.readCtx(ctx), dst, q, w.args...); err != nil {
		return "", fmt.Errorf("list %s: %w", table, err)
	}
	if _, _, n := last(); n > limit {
		trim(limit)
		t, key, _ := last()
		return encodeCursor(t, key), nil
	}
	return "", nil
}

// InsertAccessLog records one access. Identical rows are all kept.
func (c *CH) InsertAccessLog(ctx context.Context, r AccessLogRow) error {
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"insert_deduplicate": 0}))
	batch, err := c.conn.PrepareBatch(ctx, fmt.Sprintf("INSERT INTO %s.access_log (%s)", c.db, columns[AccessLogRow]()))
	if err != nil {
		return err
	}
	if err := batch.AppendStruct(&r); err != nil {
		_ = batch.Abort()
		return err
	}
	return batch.Send()
}

// Presence maps each DevPod UID to the users with sessions still open
// (opened within the last 7 days, so a crashed gateway's sessions age out).
func (c *CH) Presence(ctx context.Context, devpodUIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(devpodUIDs) == 0 {
		return out, nil
	}
	q := fmt.Sprintf(`SELECT devpod_uid, groupUniqArray(user) FROM %s.sessions FINAL
		WHERE has(?, devpod_uid) AND closed_at IS NULL AND opened_at > now() - INTERVAL 7 DAY
		GROUP BY devpod_uid`, c.db)
	rows, err := c.conn.Query(c.readCtx(ctx), q, devpodUIDs)
	if err != nil {
		return nil, fmt.Errorf("presence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		var users []string
		if err := rows.Scan(&uid, &users); err != nil {
			return nil, err
		}
		sort.Strings(users)
		out[uid] = users
	}
	return out, rows.Err()
}
