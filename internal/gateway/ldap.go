// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/go-ldap/ldap/v3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/singleflight"
)

// LDAPConfig is the runtime view of GatewayConfig.spec.ldap. Path
// fields point to the files mounted from the referenced Secrets.
type LDAPConfig struct {
	URL              string
	CAPath           string // file: PEM CA bundle, "ca.crt"
	BindDN           string
	BindPasswordPath string // file: bind password, "password"
	BaseDN           string
	UserFilter       string // empty ⇒ DefaultUserFilter
	PubkeyAttribute  string // empty ⇒ "sshPublicKey"
	RequestTimeout   time.Duration
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	StaleGrace       time.Duration
}

// DefaultUserFilter is used when LDAPConfig.UserFilter is empty.
const DefaultUserFilter = `(&(objectClass=posixAccount)(uid={{.Username}}))`

// defaultPubkeyAttribute is used when LDAPConfig.PubkeyAttribute is empty.
const defaultPubkeyAttribute = "sshPublicKey"

type ldapSource struct {
	cfg        LDAPConfig
	bindPass   []byte
	caPool     *x509.CertPool
	filterTmpl *template.Template
	pubkeyAttr string
	clock      func() time.Time

	connMu sync.Mutex
	conn   *ldap.Conn

	cacheMu sync.RWMutex
	cache   map[string]*ldapCacheEntry
	flight  singleflight.Group

	warnMu   sync.Mutex
	lastWarn map[string]time.Time // key: "username|errClass"
}

type ldapCacheEntry struct {
	keys      []ssh.PublicKey
	fetchedAt time.Time
	lastErrAt time.Time
}

// NewLDAPSource validates cfg, loads the on-disk Secret-mounted CA and
// bind password, and returns a ready (un-dialed) IdentitySource. The
// first call to Resolve will dial and bind.
func NewLDAPSource(_ context.Context, cfg LDAPConfig) (IdentitySource, error) {
	if !strings.HasPrefix(cfg.URL, "ldaps://") {
		return nil, fmt.Errorf("ldap: URL must use ldaps:// scheme, got %q", cfg.URL)
	}
	caBytes, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("ldap: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bytes.TrimSpace(caBytes)) {
		return nil, errors.New("ldap: CA file did not contain any PEM certificates")
	}
	pw, err := os.ReadFile(cfg.BindPasswordPath)
	if err != nil {
		return nil, fmt.Errorf("ldap: read bind password: %w", err)
	}
	filterSrc := cfg.UserFilter
	if filterSrc == "" {
		filterSrc = DefaultUserFilter
	}
	tmpl, err := template.New("ldap-filter").Parse(filterSrc)
	if err != nil {
		return nil, fmt.Errorf("ldap: parse UserFilter: %w", err)
	}
	// Probe-render with a known marker to ensure (a) template
	// executes and (b) the Username field actually substitutes
	// somewhere — a filter that ignores .Username would let any
	// authenticated bind match any username, which is a footgun.
	const probe = "DEVPODPROBE0"
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"Username": probe}); err != nil {
		return nil, fmt.Errorf("ldap: execute UserFilter (probe): %w", err)
	}
	if !strings.Contains(buf.String(), probe) {
		return nil, fmt.Errorf("ldap: UserFilter must reference {{.Username}}; got %q", filterSrc)
	}
	pubkeyAttr := cfg.PubkeyAttribute
	if pubkeyAttr == "" {
		pubkeyAttr = defaultPubkeyAttribute
	}
	return &ldapSource{
		cfg:        cfg,
		bindPass:   bytes.TrimRight(pw, "\r\n"),
		caPool:     pool,
		filterTmpl: tmpl,
		pubkeyAttr: pubkeyAttr,
		clock:      time.Now,
		cache:      make(map[string]*ldapCacheEntry),
		lastWarn:   make(map[string]time.Time),
	}, nil
}

func (s *ldapSource) Name() string { return "ldap" }

// Close shuts the pooled connection. Safe to call from tests and from
// graceful manager teardown. Not part of IdentitySource — callers
// that need it do a type assertion.
func (s *ldapSource) Close() error {
	s.resetConn()
	return nil
}

// Resolve consults the in-memory cache first and falls back to a live
// LDAP search on miss/expiry. During an upstream outage, an entry that
// is still within its stale-grace window is served back with
// ServedStale=true (soft-fail). Past the grace window, the error is
// surfaced.
//
// Concurrency: the live-fetch + cache-update + outcome-metric block
// runs inside the flight.Do closure so the singleflight winner owns
// it. Followers (shared=true) get the winner's return value without
// re-running the cache write or double-counting the outcome metric.
func (s *ldapSource) Resolve(ctx context.Context, username string) (ResolveResult, error) {
	now := s.clock()

	// Upfront cache check — read-locked because this branch only
	// reads the map. Short-circuits the common cache_fresh /
	// cache_stale paths without entering the flight. Followers that
	// piled up during a wire fetch will NOT take this branch (the
	// winner installs the entry inside the flight closure below);
	// they receive the winner's return value via the flight.
	s.cacheMu.RLock()
	if e := s.cache[username]; e != nil {
		ttl := s.cfg.CacheTTL
		if e.keys == nil {
			ttl = s.cfg.NegativeCacheTTL
		}
		age := now.Sub(e.fetchedAt)
		if e.lastErrAt.IsZero() && age < ttl {
			s.cacheMu.RUnlock()
			LDAPLookupsTotal.WithLabelValues("cache_fresh").Inc()
			return resolveResultFromEntry(e, false)
		}
		if !e.lastErrAt.IsZero() && age < ttl+s.cfg.StaleGrace {
			s.cacheMu.RUnlock()
			LDAPLookupsTotal.WithLabelValues("cache_stale").Inc()
			return resolveResultFromEntry(e, true)
		}
	}
	s.cacheMu.RUnlock()

	// Live lookup through singleflight. Everything below — wire
	// fetch, cache install, outcome metric, stale-grace fallback —
	// runs once per fan-out (the winner). Followers re-derive the
	// ResolveResult from the closure's return value without
	// touching cacheMu or the metric again.
	v, ferr, _ := s.flight.Do(username, func() (any, error) {
		keys, lerr := s.liveLookup(ctx, username)
		s.cacheMu.Lock()
		defer s.cacheMu.Unlock()
		if lerr != nil {
			LDAPLookupsTotal.WithLabelValues("error").Inc()
			if existing, ok := s.cache[username]; ok {
				existing.lastErrAt = now
				age := now.Sub(existing.fetchedAt)
				ttl := s.cfg.CacheTTL
				if existing.keys == nil {
					ttl = s.cfg.NegativeCacheTTL
				}
				if age < ttl+s.cfg.StaleGrace {
					LDAPLookupsTotal.WithLabelValues("cache_stale").Inc()
					// Signal to outer code: serve from stale entry.
					return existing, nil
				}
			}
			return nil, lerr
		}
		s.cache[username] = &ldapCacheEntry{keys: keys, fetchedAt: now}
		s.updateCacheGaugesLocked()
		if keys == nil {
			LDAPLookupsTotal.WithLabelValues("miss").Inc()
			return nil, ErrIdentityNotFound
		}
		LDAPLookupsTotal.WithLabelValues("hit").Inc()
		return keys, nil
	})

	if ferr != nil {
		return ResolveResult{}, ferr
	}
	switch x := v.(type) {
	case []ssh.PublicKey:
		return ResolveResult{Keys: x}, nil
	case *ldapCacheEntry:
		return resolveResultFromEntry(x, true)
	case nil:
		return ResolveResult{}, ErrIdentityNotFound
	default:
		return ResolveResult{}, fmt.Errorf("ldap: unexpected flight return type %T", v)
	}
}

// resolveResultFromEntry translates a cache entry to a ResolveResult.
// `stale` is the per-call served-from-stale-cache flag (true only when
// the entry was returned via the stale-grace branch).
func resolveResultFromEntry(e *ldapCacheEntry, stale bool) (ResolveResult, error) {
	if e.keys == nil {
		// Negative cache hit: identity not found, but still served-
		// stale if applicable (audit row reflects the cache state).
		return ResolveResult{ServedStale: stale}, ErrIdentityNotFound
	}
	return ResolveResult{Keys: e.keys, ServedStale: stale}, nil
}

// updateCacheGaugesLocked samples len(cache) into the positive/negative
// gauges. Caller MUST hold cacheMu.
func (s *ldapSource) updateCacheGaugesLocked() {
	var pos, neg int
	for _, v := range s.cache {
		if v.keys == nil {
			neg++
		} else {
			pos++
		}
	}
	LDAPCacheEntries.WithLabelValues("positive").Set(float64(pos))
	LDAPCacheEntries.WithLabelValues("negative").Set(float64(neg))
}

// liveLookup performs one round-trip against LDAP. Returns
// (keys, nil) on a hit, (nil, nil) on "no such user", (nil, err) on a
// real failure.
func (s *ldapSource) liveLookup(ctx context.Context, username string) ([]ssh.PublicKey, error) {
	_ = ctx // ctx is reserved for future cancellation; go-ldap v3 ties cancellation to conn timeout
	start := s.clock()
	defer func() {
		LDAPLookupDuration.Observe(s.clock().Sub(start).Seconds())
	}()

	conn, err := s.acquireConn()
	if err != nil {
		werr := fmt.Errorf("ldap dial/bind: %w", err)
		s.warnRateLimited(username, werr)
		return nil, werr
	}
	filter, err := s.renderFilter(username)
	if err != nil {
		return nil, fmt.Errorf("ldap render filter: %w", err)
	}
	req := ldap.NewSearchRequest(
		s.cfg.BaseDN,
		ldap.ScopeWholeSubtree,
		// DerefAlways per spec §4.3: chase aliases at search time so
		// directories that route entries through aliasedObjectName
		// resolve to the canonical entry, not the alias.
		ldap.DerefAlways,
		2, // SizeLimit
		roundUpSeconds(s.cfg.RequestTimeout),
		false,
		filter,
		[]string{s.pubkeyAttr},
		nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		// On any LDAP error, drop the conn so the next call redials.
		s.resetConn()
		werr := fmt.Errorf("ldap search: %w", err)
		s.warnRateLimited(username, werr)
		return nil, werr
	}
	if len(res.Entries) == 0 {
		return nil, nil
	}
	vals := res.Entries[0].GetAttributeValues(s.pubkeyAttr)
	keys := make([]ssh.PublicKey, 0, len(vals))
	for _, v := range vals {
		parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(v))
		if perr != nil {
			PubkeyParseErrorsTotal.WithLabelValues("ldap").Inc()
			continue
		}
		keys = append(keys, parsed)
	}
	return keys, nil
}

// renderFilter executes the configured template against an escaped
// username.
func (s *ldapSource) renderFilter(username string) (string, error) {
	var buf bytes.Buffer
	if err := s.filterTmpl.Execute(&buf, map[string]string{"Username": escapeRFC4515(username)}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// acquireConn returns a connected, bound LDAP conn, redialing on the
// first call or after a previous error reset it.
func (s *ldapSource) acquireConn() (*ldap.Conn, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	c, err := ldap.DialURL(s.cfg.URL,
		ldap.DialWithTLSConfig(s.tlsConfig()),
	)
	if err != nil {
		return nil, err
	}
	c.SetTimeout(s.cfg.RequestTimeout)
	if err := c.Bind(s.cfg.BindDN, string(s.bindPass)); err != nil {
		_ = c.Close()
		return nil, err
	}
	s.conn = c
	LDAPConnectionState.Set(1)
	return c, nil
}

// resetConn closes the current conn (if any) so the next acquire
// dials fresh. Safe to call without holding connMu.
func (s *ldapSource) resetConn() {
	s.connMu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
		LDAPConnectionState.Set(0)
	}
	s.connMu.Unlock()
}

func (s *ldapSource) tlsConfig() *tls.Config {
	// Use net/url so bracketed IPv6 literals (e.g. ldaps://[::1]:636)
	// strip cleanly via url.Hostname(); a plain IndexByte(':') split
	// would land mid-address.
	host := s.cfg.URL // fallback; URL was validated at construction
	if u, err := url.Parse(s.cfg.URL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return &tls.Config{
		RootCAs:    s.caPool,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
}

// warnRateLimited emits a slog.Warn on the first failure per
// (username, errClass) within a 1-minute window; subsequent failures
// in the same window drop to slog.Debug. Spec §6.3 — keeps an outage
// from drowning the audit channel.
func (s *ldapSource) warnRateLimited(username string, err error) {
	cls := classifyLDAPError(err)
	key := username + "|" + cls
	now := s.clock()
	s.warnMu.Lock()
	last, seen := s.lastWarn[key]
	fresh := !seen || now.Sub(last) >= time.Minute
	if fresh {
		s.lastWarn[key] = now
	}
	s.warnMu.Unlock()
	attrs := []any{"username", username, "err_class", cls, "err", err.Error()}
	if fresh {
		slog.Warn("ldap_lookup_failed", attrs...)
	} else {
		slog.Debug("ldap_lookup_failed", attrs...)
	}
}

// classifyLDAPError buckets LDAP errors into broad classes for the
// warn-rate-limit key. A sustained outage of one class won't drown
// the others.
func classifyLDAPError(err error) string {
	if err == nil {
		return "ok"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "Network Error"),
		strings.Contains(s, "connection refused"),
		strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "no such host"):
		return "network"
	case strings.Contains(s, "Invalid Credentials"):
		return "bind"
	case strings.Contains(s, "Time Limit Exceeded"):
		return "timeout"
	default:
		return "other"
	}
}

// roundUpSeconds converts a duration to integer seconds, rounding any
// sub-second remainder up. Returns at least 1 — LDAP's wire-level
// TimeLimit treats 0 as "server policy default" (effectively
// unlimited from the client's perspective), which is the opposite of
// what an operator asks when setting a sub-second timeout. CRD's
// Min=1 prevents this today but the conversion shouldn't be a
// footgun the next time someone relaxes that bound.
func roundUpSeconds(d time.Duration) int {
	secs := int(d / time.Second)
	if d%time.Second > 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return secs
}

// SetLDAPClockForTesting overrides the clock on an ldapSource.
// Returns silently if src is not an *ldapSource (e.g. a fake source
// in unit tests).
func SetLDAPClockForTesting(src IdentitySource, now func() time.Time) {
	if ls, ok := src.(*ldapSource); ok {
		ls.clock = now
	}
}

// ResetLDAPConnForTesting drops the bound LDAP connection so the next
// Resolve redials. No-op when src isn't an *ldapSource.
func ResetLDAPConnForTesting(src IdentitySource) {
	if ls, ok := src.(*ldapSource); ok {
		ls.resetConn()
	}
}
