// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/leader"
)

type auditFlags struct {
	chURL, chDatabase, chCredsDir              string
	s3Endpoint, s3Bucket, s3Region, s3CredsDir string
	s3PathStyle                                bool

	// live session viewing (audit spec §16)
	liveKeyFile, liveGatewayService, liveGatewayNS string
	livePort, liveMaxViewers                       int
}

func addAuditFlags(fs *flag.FlagSet) *auditFlags {
	f := &auditFlags{}
	fs.StringVar(&f.liveKeyFile, "audit-live-key-file", "", "file with the key for the gateways' live session endpoint (empty disables live viewing)")
	fs.StringVar(&f.liveGatewayService, "audit-live-gateway-service", "devpod-gateway", "Service whose EndpointSlices locate gateway Pods")
	fs.StringVar(&f.liveGatewayNS, "audit-live-gateway-namespace", os.Getenv("POD_NAMESPACE"), "namespace of the gateway Service (default: this Pod's)")
	fs.IntVar(&f.livePort, "audit-live-port", 8082, "port of the gateways' live session endpoint")
	fs.IntVar(&f.liveMaxViewers, "audit-live-max-viewers", 16, "concurrent live session views per webui replica")
	fs.StringVar(&f.chURL, "audit-clickhouse-url", "", "audit ClickHouse URL, e.g. clickhouse://host:9000 (empty disables the audit UI)")
	fs.StringVar(&f.chDatabase, "audit-clickhouse-database", "devpod_audit", "audit ClickHouse database")
	fs.StringVar(&f.chCredsDir, "audit-clickhouse-credentials-dir", "", "directory with files username and password")
	fs.StringVar(&f.s3Endpoint, "audit-s3-endpoint", "", "audit S3 endpoint URL (empty = AWS)")
	fs.StringVar(&f.s3Bucket, "audit-s3-bucket", "devpod-audit", "audit S3 bucket")
	fs.StringVar(&f.s3Region, "audit-s3-region", "us-east-1", "audit S3 region")
	fs.BoolVar(&f.s3PathStyle, "audit-s3-path-style", true, "use path-style S3 addressing")
	fs.StringVar(&f.s3CredsDir, "audit-s3-credentials-dir", "", "directory with files access-key and secret-key")
	return f
}

func (f *auditFlags) chConfig() (store.CHConfig, error) {
	cfg := store.CHConfig{URL: f.chURL, Database: f.chDatabase}
	if f.chCredsDir != "" {
		u, p, err := store.LoadCHCredentials(f.chCredsDir)
		if err != nil {
			return cfg, fmt.Errorf("clickhouse credentials: %w", err)
		}
		cfg.Username, cfg.Password = u, p
	}
	return cfg, nil
}

func (f *auditFlags) s3(ctx context.Context) (*store.S3, error) {
	cfg := store.S3Config{Endpoint: f.s3Endpoint, Region: f.s3Region, Bucket: f.s3Bucket, PathStyle: f.s3PathStyle}
	if f.s3CredsDir != "" {
		a, s, err := store.LoadS3Credentials(f.s3CredsDir)
		if err != nil {
			return nil, fmt.Errorf("s3 credentials: %w", err)
		}
		cfg.AccessKey, cfg.SecretKey = a, s
	}
	return store.NewS3(ctx, cfg)
}

// deps returns the audit backends, or nil when audit is not configured.
// ClickHouse is connected lazily so the webui starts while it is down.
func (f *auditFlags) deps(ctx context.Context) (*webui.AuditDeps, error) {
	if f.chURL == "" {
		return nil, nil
	}
	chCfg, err := f.chConfig()
	if err != nil {
		return nil, err
	}
	objects, err := f.s3(ctx)
	if err != nil {
		return nil, err
	}
	return &webui.AuditDeps{Store: &lazyCH{cfg: chCfg}, Objects: objects}, nil
}

// lazyCH connects on first use and reconnects after a failed connect.
type lazyCH struct {
	cfg store.CHConfig
	mu  sync.Mutex
	ch  *store.CH
}

func (l *lazyCH) get(ctx context.Context) (*store.CH, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ch == nil {
		c, err := store.OpenCH(ctx, l.cfg)
		if err != nil {
			return nil, err
		}
		l.ch = c
	}
	return l.ch, nil
}

func (l *lazyCH) ListSessions(ctx context.Context, f store.SessionFilter) ([]store.SessionSummary, string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, "", err
	}
	return c.ListSessions(ctx, f)
}

func (l *lazyCH) GetSession(ctx context.Context, id string) (*store.SessionDetail, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetSession(ctx, id)
}

func (l *lazyCH) SearchCommands(ctx context.Context, f store.CommandFilter) ([]store.CommandRow, string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, "", err
	}
	return c.SearchCommands(ctx, f)
}

func (l *lazyCH) SearchFiles(ctx context.Context, f store.FileFilter) ([]store.FileEventRow, string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, "", err
	}
	return c.SearchFiles(ctx, f)
}

func (l *lazyCH) GetFileEvent(ctx context.Context, id string) (*store.FileEventRow, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetFileEvent(ctx, id)
}

func (l *lazyCH) ListAlerts(ctx context.Context, f store.TimeFilter) ([]store.AlertRow, string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, "", err
	}
	return c.ListAlerts(ctx, f)
}

func (l *lazyCH) ListAccessLog(ctx context.Context, f store.TimeFilter) ([]store.AccessLogRow, string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, "", err
	}
	return c.ListAccessLog(ctx, f)
}

func (l *lazyCH) InsertAccessLog(ctx context.Context, r store.AccessLogRow) error {
	c, err := l.get(ctx)
	if err != nil {
		return err
	}
	return c.InsertAccessLog(ctx, r)
}

func (l *lazyCH) Presence(ctx context.Context, uids []string) (map[string][]string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.Presence(ctx, uids)
}

// runMigrate implements `devpod-webui migrate`: create the bucket and
// lifecycle rules (optional) and apply ClickHouse migrations + TTLs.
// Run as a Helm hook Job on install and upgrade.
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	af := addAuditFlags(fs)
	retention := fs.Int("retention-days", 180, "retention for audit data (ClickHouse TTL and S3 lifecycle)")
	m1s := fs.Int("metrics-retention-1s-days", 7, "retention of 1-second container/GPU/RDMA metrics")
	m1m := fs.Int("metrics-retention-1m-days", 90, "retention of 1-minute metric rollups")
	p1s := fs.Int("process-retention-hours", 48, "retention of 1-second process metrics")
	snapDays := fs.Int("snapshot-retention-days", 7, "retention of container snapshots")
	manageLifecycle := fs.Bool("manage-lifecycle", false, "set S3 lifecycle expiration rules on the bucket")
	createBucket := fs.Bool("create-bucket", false, "create the S3 bucket when missing")
	wait := fs.Duration("wait", 5*time.Minute, "how long to retry connecting to ClickHouse")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if af.chURL == "" {
		return fmt.Errorf("--audit-clickhouse-url is required")
	}
	ctx := context.Background()

	if *createBucket || *manageLifecycle {
		s3, err := af.s3(ctx)
		if err != nil {
			return err
		}
		if *createBucket {
			if err := s3.EnsureBucket(ctx); err != nil {
				return err
			}
		}
		if *manageLifecycle {
			if err := s3.EnsureLifecycle(ctx, *retention); err != nil {
				return err
			}
		}
	}

	chCfg, err := af.chConfig()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(*wait)
	var ch *store.CH
	for {
		ch, err = store.OpenCH(ctx, chCfg)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connect clickhouse: %w", err)
		}
		slog.Info("waiting for clickhouse", "err", err)
		time.Sleep(5 * time.Second)
	}
	defer ch.Close()
	if err := store.Migrate(ctx, ch, store.Retention{AuditDays: *retention, Metrics1sDays: *m1s, Metrics1mDays: *m1m,
		Process1sHours: *p1s, SnapshotDays: *snapDays}); err != nil {
		return err
	}
	slog.Info("audit storage migrated", "database", chCfg.Database, "retentionDays", *retention)
	return nil
}

func init() {
	// `devpod-webui migrate ...` short-circuits the server's flag parsing.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func (l *lazyCH) InsertTelemetry(ctx context.Context, b store.TelemetryBatch, token string) error {
	c, err := l.get(ctx)
	if err != nil {
		return err
	}
	return c.InsertTelemetry(ctx, b, token)
}

func (l *lazyCH) OpenSessionsByBackend(ctx context.Context, since time.Time) (map[string]map[string]store.SessionRef, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.OpenSessionsByBackend(ctx, since)
}

func (l *lazyCH) MetricsRange(ctx context.Context, uid string, from, to time.Time, step time.Duration) (*store.MetricSeries, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.MetricsRange(ctx, uid, from, to, step)
}

func (l *lazyCH) RecentContainer(ctx context.Context, uids []string, from, to time.Time, step time.Duration) (*store.RecentSeries, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.RecentContainer(ctx, uids, from, to, step)
}

func (l *lazyCH) LatestInfo(ctx context.Context, uid string, since time.Time) (*store.ContainerInfoRow, *store.SnapshotRow, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c.LatestInfo(ctx, uid, since)
}

func (l *lazyCH) LastSessionByUser(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.LastSessionByUser(ctx, since)
}

func (l *lazyCH) IdleCandidates(ctx context.Context, now time.Time, cpuPct, gpuPct float64) ([]string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.IdleCandidates(ctx, now, cpuPct, gpuPct)
}

func (l *lazyCH) PendingAttributions(ctx context.Context, since time.Time) ([]store.SSHConnRow, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.PendingAttributions(ctx, since)
}

func (l *lazyCH) PendingProcesses(ctx context.Context, since time.Time) ([]store.ProcessRow, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.PendingProcesses(ctx, since)
}

func (l *lazyCH) PendingCommands(ctx context.Context, since, procSince time.Time) ([]store.PendingCommand, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.PendingCommands(ctx, since, procSince)
}

func (l *lazyCH) BackendSessions(ctx context.Context, since time.Time) ([]store.BackendSession, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.BackendSessions(ctx, since)
}

func (l *lazyCH) HookMissing(ctx context.Context, from, to time.Time) ([]store.SessionRow, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.HookMissing(ctx, from, to)
}

func (l *lazyCH) InfoSeen(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.InfoSeen(ctx, since)
}

func (l *lazyCH) HooksSince(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.HooksSince(ctx, since)
}

func (l *lazyCH) UnattributedCounts(ctx context.Context, from, to time.Time) (map[string]uint64, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return c.UnattributedCounts(ctx, from, to)
}

// lazyCH serves every store the webui uses.
var (
	_ webui.AuditStore     = (*lazyCH)(nil)
	_ webui.TelemetryStore = (*lazyCH)(nil)
	_ leader.JobStore      = (*lazyCH)(nil)
)

// liveSessions wires live session viewing when audit and the key are set.
func (f *auditFlags) liveSessions(reader client.Reader) (*webui.LiveSessions, error) {
	if f.liveKeyFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(f.liveKeyFile)
	if err != nil {
		return nil, fmt.Errorf("audit live key: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return nil, errors.New("audit live key file is empty")
	}
	ns := f.liveGatewayNS
	if ns == "" {
		ns = "devpod-system"
	}
	return webui.NewLiveSessions(webui.GatewayLiveResolver(reader, ns, f.liveGatewayService, f.livePort), key, f.liveMaxViewers), nil
}
