// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit"
	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

// DefaultAuditNotice is prepended to the login banner while audit is on.
const DefaultAuditNotice = "本会话将被记录用于安全审计"

type auditFlags struct {
	s3Endpoint, s3Bucket, s3Region, s3CredsDir string
	s3PathStyle                                bool
	chURL, chDatabase, chCredsDir              string
	spoolDir, spoolMax                         string
	drainTimeout                               time.Duration
}

func auditEnabled(gc *devpodv1alpha1.GatewayConfig) bool {
	return gc != nil && gc.Spec.Audit != nil && gc.Spec.Audit.Enabled
}

// sessionPolicy resolves the effective policy for a session started by
// the user u (nil when the user has no User CR, e.g. LDAP-only).
func sessionPolicy(gc *devpodv1alpha1.GatewayConfig, u *devpodv1alpha1.User) policy.Effective {
	var def, usr *devpodv1alpha1.AuditPolicy
	if gc != nil && gc.Spec.Audit != nil {
		def = &gc.Spec.Audit.Policy
	}
	if u != nil {
		usr = u.Spec.AuditPolicy
	}
	return policy.Resolve(def, usr)
}

// bannerText renders the login banner: the audit notice (when audit is
// enabled) followed by the configured template.
func bannerText(gc *devpodv1alpha1.GatewayConfig, tmpl *template.Template, meta ssh.ConnMetadata) string {
	body := renderBanner(tmpl, meta)
	if !auditEnabled(gc) {
		return body
	}
	notice := gc.Spec.Audit.Notice
	if notice == "" {
		notice = DefaultAuditNotice
	}
	return notice + "\n" + body
}

func renderBanner(tmpl *template.Template, meta ssh.ConnMetadata) string {
	if tmpl == nil {
		return ""
	}
	login := meta.User()
	user, pod, _ := gateway.ParseLoginName(login)
	host, _ := os.Hostname()
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, bannerData{
		Now: time.Now(), ClientIP: meta.RemoteAddr().String(), Login: login, User: user, Pod: pod, Host: host,
	}); err != nil {
		// Best-effort: a malformed substitution must not confuse auth.
		return ""
	}
	return buf.String()
}

// parseSize accepts a byte count or a Kubernetes quantity ("20Gi").
func parseSize(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("size must be positive, got %d", n)
		}
		return n, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	if q.Value() <= 0 {
		return 0, fmt.Errorf("size must be positive, got %q", s)
	}
	return q.Value(), nil
}

// lazyEvents connects to ClickHouse on first use, so the gateway starts
// (and keeps proxying, spooling locally) while ClickHouse is down.
type lazyEvents struct {
	cfg store.CHConfig
	mu  sync.Mutex
	ch  *store.CH
}

func (l *lazyEvents) Insert(ctx context.Context, b store.Batch, token string) error {
	l.mu.Lock()
	ch := l.ch
	if ch == nil {
		c, err := store.OpenCH(ctx, l.cfg)
		if err != nil {
			l.mu.Unlock()
			return err
		}
		l.ch, ch = c, c
	}
	l.mu.Unlock()
	return ch.Insert(ctx, b, token)
}

// buildRecorder wires the audit recorder from flags; nil when audit
// storage is not configured (--audit-clickhouse-url empty).
func buildRecorder(ctx context.Context, f auditFlags) (*audit.Recorder, error) {
	if f.chURL == "" {
		return nil, nil
	}
	if f.s3Bucket == "" {
		return nil, errors.New("--audit-s3-bucket is required with --audit-clickhouse-url")
	}
	chCfg := store.CHConfig{URL: f.chURL, Database: f.chDatabase}
	if f.chCredsDir != "" {
		u, p, err := store.LoadCHCredentials(f.chCredsDir)
		if err != nil {
			return nil, fmt.Errorf("clickhouse credentials: %w", err)
		}
		chCfg.Username, chCfg.Password = u, p
	}
	s3Cfg := store.S3Config{Endpoint: f.s3Endpoint, Region: f.s3Region, Bucket: f.s3Bucket, PathStyle: f.s3PathStyle}
	if f.s3CredsDir != "" {
		a, s, err := store.LoadS3Credentials(f.s3CredsDir)
		if err != nil {
			return nil, fmt.Errorf("s3 credentials: %w", err)
		}
		s3Cfg.AccessKey, s3Cfg.SecretKey = a, s
	}
	objects, err := store.NewS3(ctx, s3Cfg)
	if err != nil {
		return nil, err
	}
	max, err := parseSize(f.spoolMax)
	if err != nil {
		return nil, fmt.Errorf("--audit-spool-max-bytes: %w", err)
	}
	return audit.NewRecorder(audit.Config{
		SpoolDir:      f.spoolDir,
		SpoolMaxBytes: max,
		Objects:       objects,
		Events:        &lazyEvents{cfg: chCfg},
		GatewayPod:    strings.TrimSpace(os.Getenv("POD_NAME")),
	})
}

// auditDeps carries what a connection handler needs for audit and the
// dynamic banner.
type auditDeps struct {
	cache        client.Reader
	gcName       string // GatewayConfig to read (default "default")
	ns           string
	rec          *audit.Recorder // nil = audit storage not configured
	tmpl         *template.Template
	sessionGrace time.Duration
}

// gatewayConfig reads GatewayConfig/default from the informer cache, so
// banner, audit switch and policy changes apply to new sessions.
func (a *auditDeps) gatewayConfig(ctx context.Context) *devpodv1alpha1.GatewayConfig {
	var gc devpodv1alpha1.GatewayConfig
	if err := a.cache.Get(ctx, types.NamespacedName{Name: a.gcName}, &gc); err != nil {
		return nil
	}
	return &gc
}

// startSession begins recording when audit is configured and enabled.
func (a *auditDeps) startSession(ctx context.Context, meta audit.SessionMeta) (audit.SessionHooks, *audit.Session) {
	if a.rec == nil {
		return audit.Noop(), nil
	}
	gc := a.gatewayConfig(ctx)
	if !auditEnabled(gc) {
		return audit.Noop(), nil
	}
	var u devpodv1alpha1.User
	up := &u
	if err := a.cache.Get(ctx, types.NamespacedName{Name: meta.User}, &u); err != nil {
		up = nil // LDAP-only user: cluster default
	}
	var dp devpodv1alpha1.DevPod
	if err := a.cache.Get(ctx, types.NamespacedName{Namespace: a.ns, Name: meta.DevPod}, &dp); err == nil {
		meta.DevPodUID, meta.Owner = string(dp.UID), dp.Spec.Owner
	}
	s := a.rec.Start(meta, sessionPolicy(gc, up))
	return s, s
}

// waitWithGrace waits for connection handlers after the listener closed;
// after grace it closes the remaining connections and waits again.
func waitWithGrace(wait func() error, conns *sync.Map, grace time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		conns.Range(func(k, _ any) bool {
			_ = k.(net.Conn).Close()
			return true
		})
		return <-done
	}
}
