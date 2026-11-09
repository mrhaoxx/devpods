// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/oklog/ulid/v2"
)

// freshDB opens a migrated, empty database unique to this test.
func freshDB(t *testing.T) *CH {
	t.Helper()
	ctx := context.Background()
	db := fmt.Sprintf("q_%d", time.Now().UnixNano())
	c := openTestCH(t, db)
	if err := Migrate(ctx, c, DefaultRetention(30)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db) })
	return c
}

var base = time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)

func sess(id, user, uid string, at time.Time, version uint64, closed bool) SessionRow {
	r := SessionRow{SessionID: id, User: user, DevPod: user + "-dev", DevPodUID: uid, OpenedAt: at, Version: version}
	if closed {
		c := at.Add(time.Minute)
		r.ClosedAt = &c
		r.CloseReason = "client_disconnect"
	}
	return r
}

func mustInsert(t *testing.T, c *CH, b Batch, token string) {
	t.Helper()
	if err := c.Insert(context.Background(), b, token); err != nil {
		t.Fatal(err)
	}
}

func day() SessionFilter {
	return SessionFilter{From: base.Add(-time.Hour), To: base.Add(24 * time.Hour)}
}

func TestListSessions_FiltersAndCursor(t *testing.T) {
	c := freshDB(t)
	var b Batch
	for i := 0; i < 250; i++ {
		b.Sessions = append(b.Sessions, sess(fmt.Sprintf("A%03d", i), "alice", "u1", base.Add(time.Duration(i)*time.Second), 1, false))
	}
	for i := 0; i < 10; i++ {
		b.Sessions = append(b.Sessions, sess(fmt.Sprintf("B%03d", i), "bob", "u2", base, 1, false))
	}
	mustInsert(t, c, b, "seed")

	f := day()
	f.User, f.Limit = "alice", 100
	seen := map[string]bool{}
	var sizes []int
	for page := 0; page < 5; page++ {
		rows, next, err := c.ListSessions(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(rows))
		for i, r := range rows {
			if r.User != "alice" {
				t.Fatalf("user filter leaked %s", r.User)
			}
			if seen[r.SessionID] {
				t.Fatalf("session %s returned twice", r.SessionID)
			}
			seen[r.SessionID] = true
			if i > 0 && r.OpenedAt.After(rows[i-1].OpenedAt) {
				t.Fatal("not newest-first")
			}
		}
		if next == "" {
			break
		}
		f.Cursor = next
	}
	if fmt.Sprint(sizes) != "[100 100 50]" || len(seen) != 250 {
		t.Fatalf("page sizes %v, %d distinct", sizes, len(seen))
	}
}

func TestListSessions_FinalUsesLatestVersion(t *testing.T) {
	c := freshDB(t)
	mustInsert(t, c, Batch{Sessions: []SessionRow{sess("S1", "alice", "u1", base, 1, false)}}, "v1")
	mustInsert(t, c, Batch{Sessions: []SessionRow{sess("S1", "alice", "u1", base, 2, true)}}, "v2")
	rows, _, err := c.ListSessions(context.Background(), day())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ClosedAt == nil || rows[0].CloseReason != "client_disconnect" {
		t.Fatalf("got %+v", rows)
	}
}

func TestListSessions_HasFilesHasAlerts(t *testing.T) {
	c := freshDB(t)
	mustInsert(t, c, Batch{
		Sessions: []SessionRow{
			sess("F", "alice", "u1", base, 1, false),
			sess("A", "alice", "u1", base.Add(time.Second), 1, false),
			sess("P", "alice", "u1", base.Add(2*time.Second), 1, false),
			sess("N", "alice", "u1", base.Add(3*time.Second), 1, false),
		},
		FileEvents:   []FileEventRow{{EventID: "f1", TS: base, SessionID: "F", User: "alice", DevPodUID: "u1", Protocol: "sftp", Op: "upload", Path: "/x"}},
		Alerts:       []AlertRow{{TS: base, DevPodUID: "u1", SessionID: "A", Kind: "gap"}},
		PolicyEvents: []PolicyEventRow{{TS: base, SessionID: "P", User: "alice", DevPodUID: "u1", Rule: "download", Action: "deny"}},
		Commands:     []CommandRow{{EventID: "c1", TS: base, SessionID: "F", DevPodUID: "u1", User: "alice", Cmdline: "ls", Version: 1}},
	}, "seed")

	ids := func(f SessionFilter) []string {
		rows, _, err := c.ListSessions(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.SessionID)
			if r.SessionID == "F" && (r.Files != 1 || r.Commands != 1) {
				t.Errorf("F counts: %+v", r)
			}
			if r.SessionID == "P" && r.PolicyHits != 1 {
				t.Errorf("P counts: %+v", r)
			}
		}
		return out
	}
	f := day()
	f.HasFiles = true
	if got := fmt.Sprint(ids(f)); got != "[F]" {
		t.Fatalf("hasFiles: %s", got)
	}
	f = day()
	f.HasAlerts = true
	if got := fmt.Sprint(ids(f)); got != "[P A]" {
		t.Fatalf("hasAlerts (alerts or policy hits): %s", got)
	}
}

func TestGetSession_DetailAndNotFound(t *testing.T) {
	c := freshDB(t)
	sid := ulidAt(base) // session ids are ULIDs of the open time
	b := sampleBatch(sid)
	b.Sessions[0].OpenedAt = base
	mustInsert(t, c, b, "seed")
	// The channel's close is a second version of its row.
	v2 := b.Channels[0]
	closed := base.Add(time.Minute)
	v2.ClosedAt, v2.BytesIn, v2.Version = &closed, 42, 2
	mustInsert(t, c, Batch{Channels: []ChannelRow{v2}}, "close")
	d, err := c.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Channels) != 1 || d.Channels[0].BytesIn != 42 || d.Channels[0].ClosedAt == nil {
		t.Fatalf("want only the latest channel version, got %+v", d.Channels)
	}
	if d.Session.SessionID != sid || len(d.Channels) != 1 || len(d.Commands) != 1 || len(d.Files) != 1 ||
		len(d.Policy) != 1 || len(d.Alerts) != 1 || len(d.Segments) != 1 {
		t.Fatalf("detail %+v", d)
	}
	for _, id := range []string{"nope", ulidAt(base.Add(time.Minute))} {
		if _, err := c.GetSession(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: want ErrNotFound, got %v", id, err)
		}
	}
}

func TestSearchCommands_Substring(t *testing.T) {
	c := freshDB(t)
	mustInsert(t, c, Batch{Commands: []CommandRow{
		{EventID: "1", TS: base, User: "alice", DevPodUID: "u1", Cmdline: "rm -rf /tmp/build", Version: 1},
		{EventID: "2", TS: base, User: "alice", DevPodUID: "u1", Cmdline: "ls -la", Version: 1},
		{EventID: "3", TS: base, User: "bob", DevPodUID: "u2", Cmdline: "sudo rm -rf /", Version: 1},
		{EventID: "4", TS: base, User: "alice", DevPodUID: "u1", Cmdline: "echo 100%_done", Version: 1},
	}}, "seed")
	rows, _, err := c.SearchCommands(context.Background(), CommandFilter{User: "alice", Q: "rm -rf", From: base.Add(-time.Hour), To: base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].EventID != "1" {
		t.Fatalf("got %+v", rows)
	}
	// LIKE metacharacters in the query are literal.
	rows, _, _ = c.SearchCommands(context.Background(), CommandFilter{Q: "%_", From: base.Add(-time.Hour), To: base.Add(time.Hour)})
	if len(rows) != 1 || rows[0].EventID != "4" {
		t.Fatalf("metachar search got %+v", rows)
	}
}

func TestSearchFiles_BySHA(t *testing.T) {
	c := freshDB(t)
	a, bID := ulidAt(base), ulidAt(base) // file event ids are ULIDs of their ts
	mustInsert(t, c, Batch{FileEvents: []FileEventRow{
		{EventID: a, TS: base, User: "alice", DevPodUID: "u1", Path: "/data/a.csv", SHA256: "aaa"},
		{EventID: bID, TS: base, User: "alice", DevPodUID: "u1", Path: "/data/b.csv", SHA256: "bbb"},
	}}, "seed")
	r := TimeFilter{From: base.Add(-time.Hour), To: base.Add(time.Hour)}
	rows, _, err := c.SearchFiles(context.Background(), FileFilter{SHA256: "bbb", From: r.From, To: r.To})
	if err != nil || len(rows) != 1 || rows[0].EventID != bID {
		t.Fatalf("by sha: %+v %v", rows, err)
	}
	rows, _, _ = c.SearchFiles(context.Background(), FileFilter{Path: "a.csv", From: r.From, To: r.To})
	if len(rows) != 1 || rows[0].EventID != a {
		t.Fatalf("by path: %+v", rows)
	}
	fe, err := c.GetFileEvent(context.Background(), a)
	if err != nil || fe.Path != "/data/a.csv" {
		t.Fatalf("GetFileEvent: %+v %v", fe, err)
	}
	for _, id := range []string{"zz", ulidAt(base.Add(time.Minute))} {
		if _, err := c.GetFileEvent(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: want ErrNotFound, got %v", id, err)
		}
	}
}

func TestAccessLogAndAlerts(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // identical rows must both be kept
		if err := c.InsertAccessLog(ctx, AccessLogRow{TS: base, Viewer: "admin", Action: "replay", TargetSession: "S1"}); err != nil {
			t.Fatal(err)
		}
	}
	tf := TimeFilter{From: base.Add(-time.Hour), To: base.Add(time.Hour)}
	rows, _, err := c.ListAccessLog(ctx, tf)
	if err != nil || len(rows) != 2 {
		t.Fatalf("access log %+v %v", rows, err)
	}
	mustInsert(t, c, Batch{Alerts: []AlertRow{{TS: base, Kind: "gap", SessionID: "S1"}, {TS: base.Add(time.Second), Kind: "decoder_error"}}}, "a")
	al, _, err := c.ListAlerts(ctx, tf)
	if err != nil || len(al) != 2 || al[0].Kind != "decoder_error" {
		t.Fatalf("alerts %+v %v", al, err)
	}
}

func TestPresence_OnlyOpenSessions(t *testing.T) {
	c := freshDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	mustInsert(t, c, Batch{Sessions: []SessionRow{
		sess("O1", "alice", "u1", now, 1, false),
		sess("O2", "bob", "u1", now, 1, false),
		sess("C1", "carol", "u1", now, 2, true),
		sess("X", "dave", "u9", now, 1, false),
	}}, "seed")
	got, err := c.Presence(context.Background(), []string{"u1", "u2"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "map[u1:[alice bob]]" {
		t.Fatalf("presence %v", got)
	}
}

func TestRangeRequired(t *testing.T) {
	c := freshDB(t)
	if _, _, err := c.ListSessions(context.Background(), SessionFilter{}); !errors.Is(err, ErrRangeRequired) {
		t.Fatalf("ListSessions: %v", err)
	}
	if _, _, err := c.SearchCommands(context.Background(), CommandFilter{}); !errors.Is(err, ErrRangeRequired) {
		t.Fatalf("SearchCommands: %v", err)
	}
}

func ulidAt(t time.Time) string { return ulid.MustNew(ulid.Timestamp(t), rand.Reader).String() }

// §9.3: every audit read must be bounded by time. With
// force_index_by_date ClickHouse refuses any query that cannot prune
// partitions by date, i.e. one that would scan all retained months.
func TestReadQueries_UseDateIndex(t *testing.T) {
	c := freshDB(t)
	sid := ulidAt(base)
	b := sampleBatch(sid)
	b.Sessions[0].OpenedAt = base
	for i := range b.Channels {
		b.Channels[i].OpenedAt = base
	}
	at := base.Add(time.Second)
	b.Commands[0].TS, b.PolicyEvents[0].TS, b.Alerts[0].TS = at, at, at
	b.FileEvents[0].TS, b.FileEvents[0].EventID = at, ulidAt(at)
	b.RawSegments[0].FirstTS, b.RawSegments[0].LastTS = at, at
	mustInsert(t, c, b, "seed")
	if err := c.InsertAccessLog(context.Background(), AccessLogRow{TS: at, Viewer: "root", Action: "replay", TargetSession: sid}); err != nil {
		t.Fatal(err)
	}
	c.readExtra = clickhouse.Settings{"force_index_by_date": 1}
	ctx := context.Background()
	f := day()

	check := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, _, err := c.ListSessions(ctx, f)
	check("ListSessions", err)
	f.HasFiles, f.HasAlerts = true, true
	_, _, err = c.ListSessions(ctx, f)
	check("ListSessions(has files, alerts)", err)
	d, err := c.GetSession(ctx, sid)
	check("GetSession", err)
	if err == nil && (len(d.Channels) != 1 || len(d.Commands) != 1 || len(d.Files) != 1 || len(d.Policy) != 1 || len(d.Alerts) != 1 || len(d.Segments) != 1) {
		t.Errorf("GetSession detail incomplete: %+v", d)
	}
	_, err = c.GetFileEvent(ctx, b.FileEvents[0].EventID)
	check("GetFileEvent", err)
	_, _, err = c.SearchCommands(ctx, CommandFilter{From: f.From, To: f.To, Q: "make"})
	check("SearchCommands", err)
	_, _, err = c.SearchFiles(ctx, FileFilter{From: f.From, To: f.To, SHA256: "abc"})
	check("SearchFiles", err)
	_, _, err = c.ListAlerts(ctx, TimeFilter{From: f.From, To: f.To})
	check("ListAlerts", err)
	_, _, err = c.ListAccessLog(ctx, TimeFilter{From: f.From, To: f.To})
	check("ListAccessLog", err)
	_, err = c.Presence(ctx, []string{"uid-1"})
	check("Presence", err)
	_, err = c.MetricsRange(ctx, "uid-1", time.Now().Add(-30*time.Minute), time.Now(), 0)
	check("MetricsRange 1s", err)
	_, err = c.MetricsRange(ctx, "uid-1", time.Now().Add(-3*time.Hour), time.Now(), 0)
	check("MetricsRange 1m", err)
	_, _, err = c.LatestInfo(ctx, "uid-1", time.Now().Add(-time.Hour))
	if !errors.Is(err, ErrNotFound) {
		check("LatestInfo", err)
	}
	_, err = c.IdleCandidates(ctx, time.Now(), 5, 5)
	check("IdleCandidates", err)
	_, err = c.PendingAttributions(ctx, time.Now().Add(-time.Hour))
	check("PendingAttributions", err)
	_, err = c.OpenSessionsByBackend(ctx, time.Now().Add(-time.Hour))
	check("OpenSessionsByBackend", err)
	_, err = c.HookMissing(ctx, time.Now().Add(-time.Hour), time.Now())
	check("HookMissing", err)
	_, err = c.LastSessionByUser(ctx, time.Now().Add(-24*time.Hour))
	check("LastSessionByUser", err)
}
