// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Integration tests run against a ClickHouse and an S3 endpoint named by
// AUDIT_IT_CLICKHOUSE_URL (_USER, _PASSWORD) and AUDIT_IT_S3_ENDPOINT
// (_ACCESS_KEY, _SECRET_KEY): throwaway ones, never production's.
// Without the env they are skipped.

func testCH(t *testing.T) CHConfig {
	t.Helper()
	url := os.Getenv("AUDIT_IT_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("AUDIT_IT_CLICKHOUSE_URL not set")
	}
	return CHConfig{URL: url, Username: os.Getenv("AUDIT_IT_CLICKHOUSE_USER"), Password: os.Getenv("AUDIT_IT_CLICKHOUSE_PASSWORD")}
}

func testS3(t *testing.T) S3Config {
	t.Helper()
	ep := os.Getenv("AUDIT_IT_S3_ENDPOINT")
	if ep == "" {
		t.Skip("AUDIT_IT_S3_ENDPOINT not set")
	}
	return S3Config{Endpoint: ep, Region: "us-east-1", AccessKey: os.Getenv("AUDIT_IT_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("AUDIT_IT_S3_SECRET_KEY"), PathStyle: true}
}

func openTestCH(t *testing.T, db string) *CH {
	t.Helper()
	cfg := testCH(t)
	cfg.Database = db
	c, err := OpenCH(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

var auditTables = []string{"sessions", "channels", "commands", "file_events", "policy_events", "raw_segments", "alerts", "access_log", "reservations", "http_requests", "events"}

func TestMigrate_Idempotent(t *testing.T) {
	ctx := context.Background()
	c := openTestCH(t, "audit_migrate")
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, c, DefaultRetention(30)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, tbl := range auditTables {
		var q string
		if err := c.conn.QueryRow(ctx, "SELECT create_table_query FROM system.tables WHERE database = ? AND name = ?", "audit_migrate", tbl).Scan(&q); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		// ClickHouse normalises "INTERVAL 30 DAY" to toIntervalDay(30).
		if !strings.Contains(q, "toIntervalDay(30)") {
			t.Errorf("%s: TTL not updated to 30 days: %s", tbl, q)
		}
	}
	files, _ := migrationFS.ReadDir("migrations")
	var n uint64
	if err := c.conn.QueryRow(ctx, "SELECT count() FROM audit_migrate.schema_migrations").Scan(&n); err != nil || n != uint64(len(files)) {
		t.Fatalf("schema_migrations rows = %d for %d migrations (%v); a migration ran twice", n, len(files), err)
	}
}

// A reservation's history: every change is a row of its own.
func TestReservationEvents(t *testing.T) {
	ctx := context.Background()
	c := openTestCH(t, "audit_rsv")
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	// the test database outlives a run: start from an empty table
	if err := c.conn.Exec(ctx, "TRUNCATE TABLE audit_rsv.reservations"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	row := ReservationEventRow{TS: now, Cluster: "st", ID: "g1-alice-abc", Node: "g1", User: "alice", Start: now, End: now.Add(time.Hour), Event: "created"}
	for _, ev := range []string{"created", "created", "hibernated"} {
		row.Event = ev
		if err := c.InsertReservationEvent(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	var n uint64
	if err := c.conn.QueryRow(ctx, "SELECT count() FROM audit_rsv.reservations WHERE id = ?", row.ID).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows = %d (%v); want 3, identical ones too", n, err)
	}
}

// A reservation's purpose is in its history, whole (CJK and all).
func TestReservationEvents_Purpose(t *testing.T) {
	ctx := context.Background()
	c := openTestCH(t, "audit_rsv_purpose")
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	purpose := strings.Repeat("测", 200)
	if err := c.InsertReservationEvent(ctx, ReservationEventRow{TS: now, ID: "g1-alice-p", Node: "g1", User: "alice", Start: now, End: now.Add(time.Hour), Event: "created", Purpose: purpose}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := c.conn.QueryRow(ctx, "SELECT purpose FROM audit_rsv_purpose.reservations WHERE id = ?", "g1-alice-p").Scan(&got); err != nil || got != purpose {
		t.Fatalf("purpose = %q (%v)", got, err)
	}
}

// What is done to DevPods, jobs and machines: found by object, by kind,
// newest first, and only one's own for one without audit rights.
func TestEvents(t *testing.T) {
	ctx := context.Background()
	c := openTestCH(t, "audit_events")
	// the test database outlives a run: start from none
	if err := c.conn.Exec(ctx, "DROP DATABASE IF EXISTS audit_events"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, r := range []EventRow{
		{TS: now.Add(-2 * time.Second), Cluster: "st", Kind: "devpod", Object: "al-dev", ObjectUID: "uid-1", Owner: "al", Actor: "al", Via: "dp", Action: "create", Origin: "st/al-dev"},
		{TS: now.Add(-time.Second), Cluster: "st", Kind: "devpod", Object: "al-dev", ObjectUID: "uid-1", Owner: "al", Actor: "root", Via: "web", Action: "stop", Detail: "测试"},
		{TS: now, Cluster: "st", Kind: "job", Object: "abc12", Owner: "bo", Actor: "system:queue", Via: "system", Action: "start", Node: "g1"},
	} {
		if err := c.InsertEvents(ctx, []EventRow{r}, r.Action+r.Object); err != nil {
			t.Fatal(err)
		}
	}
	// a batch sent again with its token is kept once
	again := EventRow{TS: now.Add(-3 * time.Second), Cluster: "st", Kind: "devpod", Object: "al-dev", ObjectUID: "uid-1", Owner: "al", Actor: "al", Via: "web", Action: "edit"}
	for range 2 {
		if err := c.InsertEvents(ctx, []EventRow{again}, "retried"); err != nil {
			t.Fatal(err)
		}
	}
	span := EventFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	f := span
	f.ObjectUID = "uid-1"
	rows, _, err := c.SearchEvents(ctx, f)
	if err != nil || len(rows) != 3 || rows[0].Action != "stop" || rows[0].Detail != "测试" || rows[1].Action != "create" || rows[1].Origin != "st/al-dev" || rows[2].Action != "edit" {
		t.Fatalf("by object: %+v %v", rows, err)
	}
	f = span
	f.Kind = "job"
	if rows, _, err = c.SearchEvents(ctx, f); err != nil || len(rows) != 1 || rows[0].Node != "g1" {
		t.Fatalf("by kind: %+v %v", rows, err)
	}
	f = span
	f.Scope = &Scope{User: "root"}
	if rows, _, err = c.SearchEvents(ctx, f); err != nil || len(rows) != 1 || rows[0].Actor != "root" {
		t.Fatalf("what root did: %+v %v", rows, err)
	}
	f = span
	f.Limit = 1
	rows, next, err := c.SearchEvents(ctx, f)
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("page 1: %+v %q %v", rows, next, err)
	}
	f.Cursor = next
	if rows, _, err = c.SearchEvents(ctx, f); err != nil || len(rows) != 1 || rows[0].Action != "stop" {
		t.Fatalf("page 2: %+v %v", rows, err)
	}
	if _, _, err := c.SearchEvents(ctx, EventFilter{}); err != ErrRangeRequired {
		t.Fatalf("no range: %v", err)
	}
}

// A machine's auditor sees what was done on it, and what else was done to
// what was on it (a job's submission, a DevPod made before it had a
// machine) — not what never was.
func TestEventsMachineScope(t *testing.T) {
	ctx := context.Background()
	c := openTestCH(t, "audit_events_scope")
	if err := c.conn.Exec(ctx, "DROP DATABASE IF EXISTS audit_events_scope"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := []EventRow{
		{TS: now.Add(-3 * time.Second), Cluster: "st", Kind: "job", Object: "abc12", Owner: "bo", Actor: "bo", Via: "web", Action: "submit"},
		{TS: now.Add(-2 * time.Second), Cluster: "st", Kind: "job", Object: "abc12", Owner: "bo", Actor: "system:queue", Via: "system", Action: "start", Node: "g1"},
		{TS: now.Add(-time.Second), Cluster: "st", Kind: "devpod", Object: "al-dev", ObjectUID: "u1", Owner: "al", Actor: "al", Via: "web", Action: "create"},
		{TS: now, Cluster: "st", Kind: "devpod", Object: "al-dev", ObjectUID: "u1", Owner: "al", Actor: "system:queue", Via: "system", Action: "queue-start", Node: "g1"},
		{TS: now, Cluster: "st", Kind: "devpod", Object: "al-dev", ObjectUID: "u0", Owner: "al", Actor: "al", Via: "web", Action: "delete"},
		{TS: now, Cluster: "st", Kind: "job", Object: "zzz99", Owner: "bo", Actor: "bo", Via: "web", Action: "submit"},
	}
	if err := c.InsertEvents(ctx, rows, "scope"); err != nil {
		t.Fatal(err)
	}
	got, _, err := c.SearchEvents(ctx, EventFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour), Scope: &Scope{User: "x", Machines: []ClusterNode{{Cluster: "st", Node: "g1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, r := range got {
		seen = append(seen, r.Object+"/"+r.ObjectUID+"/"+r.Action)
	}
	want := map[string]bool{"abc12//submit": true, "abc12//start": true, "al-dev/u1/create": true, "al-dev/u1/queue-start": true}
	if len(seen) != len(want) {
		t.Fatalf("seen %v", seen)
	}
	for _, k := range seen {
		if !want[k] {
			t.Fatalf("seen %v", seen)
		}
	}
}

// Ended queue jobs are kept whole — found by id with their spec and
// status as written, listed newest ended first by cluster, owner and
// state, page by page — and a job sent again with its token is kept once.
func TestJobs(t *testing.T) {
	ctx := context.Background()
	c := openTestCH(t, "audit_jobs")
	if err := c.conn.Exec(ctx, "DROP DATABASE IF EXISTS audit_jobs"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	row := func(id, cluster, owner, phase string, ago time.Duration) JobRow {
		return JobRow{ID: id, UID: "uid-" + id, Cluster: cluster, Owner: owner, Title: "t-" + id, Phase: phase,
			SubmittedAt: now.Add(-ago - time.Hour), FinishedAt: now.Add(-ago),
			Spec: `{"owner":"` + owner + `","command":"echo ` + id + `"}`, Status: `{"phase":"` + phase + `"}`}
	}
	for _, r := range []JobRow{
		row("aaaa1", "st", "al", "Succeeded", 3*time.Minute),
		row("bbbb2", "st", "bo", "Failed", 2*time.Minute),
		row("cccc3", "yc", "al", "Cancelled", time.Minute),
	} {
		for range 2 {
			if err := c.InsertJob(ctx, r, r.UID); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := c.GetJob(ctx, "bbbb2")
	if err != nil || got == nil || got.Owner != "bo" || got.Spec != `{"owner":"bo","command":"echo bbbb2"}` || got.Status != `{"phase":"Failed"}` || !got.FinishedAt.Equal(now.Add(-2*time.Minute)) {
		t.Fatalf("by id: %+v %v", got, err)
	}
	if got, err := c.GetJob(ctx, "none9"); err != nil || got != nil {
		t.Fatalf("an id never kept: %+v %v", got, err)
	}
	ids := func(f JobFilter) ([]string, string) {
		t.Helper()
		rows, next, err := c.SearchJobs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out, next
	}
	if all, _ := ids(JobFilter{}); strings.Join(all, ",") != "cccc3,bbbb2,aaaa1" {
		t.Fatalf("all, newest ended first, each once: %v", all)
	}
	if mine, _ := ids(JobFilter{Owner: "al"}); strings.Join(mine, ",") != "cccc3,aaaa1" {
		t.Fatalf("al's: %v", mine)
	}
	if in, _ := ids(JobFilter{Cluster: "st", Phase: "Failed"}); strings.Join(in, ",") != "bbbb2" {
		t.Fatalf("st's failed: %v", in)
	}
	page1, next := ids(JobFilter{Limit: 2})
	if strings.Join(page1, ",") != "cccc3,bbbb2" || next == "" {
		t.Fatalf("page 1: %v %q", page1, next)
	}
	if page2, next2 := ids(JobFilter{Limit: 2, Cursor: next}); strings.Join(page2, ",") != "aaaa1" || next2 != "" {
		t.Fatalf("page 2: %v %q", page2, next2)
	}
}

func sampleBatch(sid string) Batch {
	now := time.Now().UTC().Truncate(time.Millisecond)
	exit := int32(3)
	return Batch{
		Sessions:     []SessionRow{{SessionID: sid, User: "alice", DevPod: "alice-dev", DevPodUID: "uid-1", OpenedAt: now, Version: 1}},
		Channels:     []ChannelRow{{SessionID: sid, ChannelID: 1, Type: "session", OpenedAt: now, Version: 1}},
		Commands:     []CommandRow{{EventID: sid + "-1", TS: now, SessionID: sid, DevPodUID: "uid-1", User: "alice", Source: "gateway_exec", ChannelID: 1, Cmdline: "make test", ExitCode: &exit, Attribution: "exact", Version: 1}},
		FileEvents:   []FileEventRow{{EventID: sid + "-f1", TS: now, SessionID: sid, ChannelID: 1, User: "alice", DevPodUID: "uid-1", Protocol: "sftp", Op: "upload", Path: "/tmp/x", Size: 3, SHA256: "abc", Complete: true}},
		PolicyEvents: []PolicyEventRow{{TS: now, SessionID: sid, User: "alice", DevPodUID: "uid-1", Rule: "download", Action: "deny"}},
		RawSegments:  []RawSegmentRow{{SessionID: sid, Seq: 1, S3Key: "raw/x", FirstTS: now, LastTS: now, Bytes: 10, CompressedBytes: 5}},
		Alerts:       []AlertRow{{TS: now, DevPodUID: "uid-1", SessionID: sid, Kind: "gap", Source: "gateway"}},
	}
}

func TestInsert_DedupToken(t *testing.T) {
	ctx := context.Background()
	c := openTestCH(t, "audit_dedup")
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	b := sampleBatch("S1")
	for i := 0; i < 2; i++ {
		if err := c.Insert(ctx, b, "batch-1"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"sessions", "channels", "commands", "file_events", "policy_events", "raw_segments", "alerts"} {
		var n uint64
		if err := c.conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM audit_dedup.%s", tbl)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s: %d rows after a retried insert, want 1", tbl, n)
		}
	}
}

// HTTP requests: kept once when their batch is sent again, each with the
// cluster it was written with, searched newest first by any filter.
func TestHTTPRequests_InsertDedupSearch(t *testing.T) {
	ctx := context.Background()
	db := fmt.Sprintf("audit_http_%d", time.Now().UnixNano())
	c := openTestCH(t, db)
	t.Cleanup(func() { _ = c.conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db) })
	if err := Migrate(ctx, c, DefaultRetention(180)); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	b := Batch{HTTPRequests: []HTTPRequestRow{
		{TS: t0, Cluster: "a", Gateway: "g1", DevPod: "alice-dev", DevPodUID: "u1", Port: 8080, Access: "private", User: "alice",
			Method: "GET", Path: "/lab", Query: "token=abc", Status: 200, BytesOut: 12, RequestID: "01A"},
		{TS: t0.Add(time.Second), Cluster: "b", Gateway: "g2", DevPod: "bob-dev", DevPodUID: "u2", Port: 3000, Access: "public",
			Method: "POST", Path: "/api", Status: 502, RequestID: "01B"},
	}}
	for i := 0; i < 2; i++ {
		if err := c.Insert(ctx, b, "tok-http"); err != nil {
			t.Fatal(err)
		}
	}
	from, to := t0.Add(-time.Minute), t0.Add(time.Minute)
	all, _, err := c.SearchHTTP(ctx, HTTPFilter{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].RequestID != "01B" || all[0].Cluster != "b" || all[1].Query != "token=abc" || all[1].BytesOut != 12 {
		t.Fatalf("all: %+v", all)
	}
	if got, _, _ := c.SearchHTTP(ctx, HTTPFilter{User: "alice", From: from, To: to}); len(got) != 1 || got[0].RequestID != "01A" {
		t.Fatalf("by user: %+v", got)
	}
	if got, _, _ := c.SearchHTTP(ctx, HTTPFilter{Cluster: "b", Status: 502, From: from, To: to}); len(got) != 1 || got[0].RequestID != "01B" {
		t.Fatalf("by cluster and status: %+v", got)
	}
	if got, _, _ := c.SearchHTTP(ctx, HTTPFilter{Port: 8080, Method: "GET", DevPodUID: "u1", From: from, To: to}); len(got) != 1 {
		t.Fatalf("by port, method and DevPod: %+v", got)
	}
	if _, _, err := c.SearchHTTP(ctx, HTTPFilter{}); !errors.Is(err, ErrRangeRequired) {
		t.Fatalf("no range: %v", err)
	}
}

func newTestS3(t *testing.T, bucket string) *S3 {
	t.Helper()
	cfg := testS3(t)
	cfg.Bucket = bucket
	s, err := NewS3(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3_PutGetMultipart(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "put-get")
	body := make([]byte, 20<<20)
	_, _ = rand.Read(body)
	if err := s.Put(ctx, "files/2026/11/09/S/1-1", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(ctx, "files/2026/11/09/S/1-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes back, content differs", len(got))
	}
}

func TestS3_EnsureBucketAndLifecycleIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "lifecycle")
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatalf("second EnsureBucket: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.EnsureLifecycle(ctx, 180); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("lifecycle")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rules) != 2 {
		t.Fatalf("%d lifecycle rules, want 2", len(out.Rules))
	}
	for _, r := range out.Rules {
		if r.Expiration == nil || aws.ToInt32(r.Expiration.Days) != 180 {
			t.Errorf("rule %s: expiration %+v", aws.ToString(r.ID), r.Expiration)
		}
	}
}
