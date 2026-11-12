// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Integration tests run against a ClickHouse and an S3 endpoint provided
// by hack/test-integration.sh (test containers on a Kubernetes cluster,
// reached through port-forwards). Without the env they are skipped.

func testCH(t *testing.T) CHConfig {
	t.Helper()
	url := os.Getenv("AUDIT_IT_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("AUDIT_IT_CLICKHOUSE_URL not set; run via hack/test-integration.sh")
	}
	return CHConfig{URL: url, Username: os.Getenv("AUDIT_IT_CLICKHOUSE_USER"), Password: os.Getenv("AUDIT_IT_CLICKHOUSE_PASSWORD")}
}

func testS3(t *testing.T) S3Config {
	t.Helper()
	ep := os.Getenv("AUDIT_IT_S3_ENDPOINT")
	if ep == "" {
		t.Skip("AUDIT_IT_S3_ENDPOINT not set; run via hack/test-integration.sh")
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

var auditTables = []string{"sessions", "session_origin", "channels", "commands", "file_events", "policy_events", "raw_segments", "alerts", "access_log"}

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
