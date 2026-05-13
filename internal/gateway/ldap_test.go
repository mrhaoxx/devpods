// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimlambrt/gldap"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

// writeFile writes data to dir/name and returns the full path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// pemEncodedCA returns a freshly minted self-signed CA in PEM form.
func pemEncodedCA(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ldap-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func validLDAPConfig(t *testing.T) (gateway.LDAPConfig, string) {
	t.Helper()
	dir := t.TempDir()
	caPath := writeFile(t, dir, "ca.crt", pemEncodedCA(t))
	pwPath := writeFile(t, dir, "password", []byte("hunter2"))
	cfg := gateway.LDAPConfig{
		URL:              "ldaps://ldap.example.test:636",
		CAPath:           caPath,
		BindDN:           "cn=svc,dc=example,dc=test",
		BindPasswordPath: pwPath,
		BaseDN:           "dc=example,dc=test",
		UserFilter:       "", // exercise default
		PubkeyAttribute:  "sshPublicKey",
		RequestTimeout:   5 * time.Second,
		CacheTTL:         5 * time.Minute,
		NegativeCacheTTL: 30 * time.Second,
		StaleGrace:       15 * time.Minute,
	}
	return cfg, dir
}

func TestNewLDAPSource_OK(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	src, err := gateway.NewLDAPSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewLDAPSource: %v", err)
	}
	if got := src.Name(); got != "ldap" {
		t.Errorf("Name() = %q, want %q", got, "ldap")
	}
}

// TestNewLDAPSource_IPv6URL pins that a bracketed IPv6 literal in the
// LDAP URL constructs without panicking. The TLS ServerName is derived
// via net/url so the brackets strip cleanly to "::1"; the previous
// IndexByte(':') split would have landed mid-address and produced a
// nonsense ServerName.
func TestNewLDAPSource_IPv6URL(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.URL = "ldaps://[::1]:636"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err != nil {
		t.Fatalf("NewLDAPSource with IPv6 URL: %v", err)
	}
}

func TestNewLDAPSource_RejectsPlaintextURL(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.URL = "ldap://ldap.example.test:389"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for plaintext ldap://")
	}
}

func TestNewLDAPSource_MissingCAFile(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.CAPath = "/nonexistent/ca.crt"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestNewLDAPSource_MalformedCA(t *testing.T) {
	cfg, dir := validLDAPConfig(t)
	cfg.CAPath = writeFile(t, dir, "ca-bad.crt", []byte("not a pem block"))
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for malformed CA")
	}
}

func TestNewLDAPSource_MissingPasswordFile(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.BindPasswordPath = "/nonexistent/password"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for missing password file")
	}
}

func TestNewLDAPSource_BadFilterTemplate(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.UserFilter = "(&(uid={{.Username" // unclosed action
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for unparseable template")
	}
}

func TestNewLDAPSource_FilterMustReferenceUsername(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.UserFilter = "(uid=hardcoded)"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for filter that ignores Username")
	}
}

// fixtureLDAP starts a TLS-terminating LDAP server bound to 127.0.0.1
// and returns its ldaps:// URL plus a CA PEM path the gateway can
// trust. The server handles bind + search; the entries argument is a
// flat map keyed by uid → []sshPublicKey lines.
func fixtureLDAP(t *testing.T, entries map[string][]string) (url, caPath string, server *gldap.Server) {
	t.Helper()
	return fixtureLDAPWithSearchHook(t, entries, func() {})
}

// fixtureLDAPWithSearchHook is the implementation shared by
// fixtureLDAP (no-op hook) and by Task 10's countingFixture
// (counter-increment hook).
func fixtureLDAPWithSearchHook(t *testing.T, entries map[string][]string, hook func()) (string, string, *gldap.Server) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"127.0.0.1", "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	dir := t.TempDir()
	caPath := writeFile(t, dir, "ca.crt", certPEM)
	certPath := writeFile(t, dir, "tls.crt", certPEM)
	keyPath := writeFile(t, dir, "tls.key", keyPEM)

	srv, err := gldap.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	router, err := gldap.NewMux()
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Bind(func(w *gldap.ResponseWriter, r *gldap.Request) {
		_ = w.Write(r.NewBindResponse(gldap.WithResponseCode(gldap.ResultSuccess)))
	}); err != nil {
		t.Fatal(err)
	}
	if err := router.Search(func(w *gldap.ResponseWriter, r *gldap.Request) {
		hook()
		msg, _ := r.GetSearchMessage()
		uid := extractUIDFromFilter(msg.Filter)
		if vals, ok := entries[uid]; ok {
			_ = w.Write(r.NewSearchResponseEntry(
				"uid="+uid+",ou=People,dc=example,dc=test",
				gldap.WithAttributes(map[string][]string{"sshPublicKey": vals}),
			))
		}
		_ = w.Write(r.NewSearchDoneResponse())
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Router(router); err != nil {
		t.Fatal(err)
	}

	addr := freeLDAPSAddr(t)
	tlsCert := mustX509KeyPair(t, certPath, keyPath)
	go func() {
		// Server.Run is blocking; report errors via t.Log only —
		// Stop() in t.Cleanup triggers a normal exit.
		if err := srv.Run(addr, gldap.WithTLSConfig(&tls.Config{Certificates: []tls.Certificate{tlsCert}, MinVersion: tls.VersionTLS12})); err != nil {
			t.Logf("gldap server exited: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Stop() })
	// Wait for the listener to come up. gldap.Server.Ready() flips
	// true the moment net.Listen returns, before the goroutine has
	// looped into Accept — that's fine for our purposes because the
	// kernel will queue connect(2)s onto the listen backlog.
	deadline := time.Now().Add(2 * time.Second)
	for !srv.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("gldap server not ready within 2s")
		}
		time.Sleep(time.Millisecond)
	}
	return "ldaps://" + addr, caPath, srv
}

// freeLDAPSAddr asks the kernel for a free 127.0.0.1 port, closes the
// holding socket, and returns "127.0.0.1:<port>". There is a small
// race window between close-and-bind that we accept (the alternative
// is to thread a *net.Listener into gldap, which its API doesn't
// expose).
func freeLDAPSAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func mustX509KeyPair(t *testing.T, certPath, keyPath string) tls.Certificate {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// extractUIDFromFilter pulls the first uid=<val> token out of an LDAP
// filter rendered from the default template. Test-only.
func extractUIDFromFilter(f string) string {
	const tok = "(uid="
	i := strings.Index(f, tok)
	if i < 0 {
		return ""
	}
	rest := f[i+len(tok):]
	j := strings.IndexAny(rest, ")")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// closableLDAP is the contract the tests rely on to drain the pooled
// client conn before t.Cleanup unwinds into gldap.Server.Stop (which
// otherwise blocks on its connWg for the held client conn).
type closableLDAP interface {
	gateway.IdentitySource
	Close() error
}

// newLDAPSource constructs a source and registers a cleanup that
// closes it BEFORE the fixture's srv.Stop runs (t.Cleanup is LIFO,
// and this is registered after the fixture).
func newLDAPSource(t *testing.T, cfg gateway.LDAPConfig) gateway.IdentitySource {
	t.Helper()
	src, err := gateway.NewLDAPSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewLDAPSource: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := src.(closableLDAP); ok {
			_ = c.Close()
		}
	})
	return src
}

func TestLDAPSource_Resolve_SingleEntrySingleKey(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src := newLDAPSource(t, cfg)
	res, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Keys) != 1 {
		t.Fatalf("len Keys = %d, want 1", len(res.Keys))
	}
	if res.ServedStale {
		t.Errorf("ServedStale = true unexpectedly (no stale cache in Task 8)")
	}
}

func TestLDAPSource_SubSecondTimeoutFloorsToOneSecond(t *testing.T) {
	// The wire-level LDAP TimeLimit accepts integer seconds. A
	// sub-second configured RequestTimeout must not truncate to 0
	// (which LDAP interprets as "no limit"); ceiling-convert and
	// floor at 1 so the search remains bounded.
	_, line := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.RequestTimeout = 500 * time.Millisecond
	src := newLDAPSource(t, cfg)
	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestLDAPSource_Resolve_MultiValueKeys(t *testing.T) {
	_, a := ed25519Pubkey(t)
	_, b := ed25519Pubkey(t)
	_, c := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{"alice": {a, b, c}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src := newLDAPSource(t, cfg)
	res, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Keys) != 3 {
		t.Fatalf("len Keys = %d, want 3", len(res.Keys))
	}
}

func TestLDAPSource_Resolve_ZeroEntries_ReturnsNotFound(t *testing.T) {
	url, ca, _ := fixtureLDAP(t, map[string][]string{})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src := newLDAPSource(t, cfg)
	_, err := src.Resolve(context.Background(), "ghost")
	if !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want ErrIdentityNotFound", err)
	}
}

func TestLDAPSource_Resolve_DropsUnparseableValues(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{
		"alice": {line, "not-a-key", "still-garbage"},
	})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src := newLDAPSource(t, cfg)
	res, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Keys) != 1 {
		t.Fatalf("len Keys = %d, want 1 (garbage dropped)", len(res.Keys))
	}
}

func TestLDAPSource_Resolve_FilterInjectionIsEscaped(t *testing.T) {
	// Only alice has a key. A malicious username tries to broaden
	// the search; the escape MUST prevent it from matching alice.
	_, line := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src := newLDAPSource(t, cfg)
	_, err := src.Resolve(context.Background(), "alice)(uid=*")
	if !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want ErrIdentityNotFound (escape rejected injection)", err)
	}
}

// withClock swaps the clock on an *ldapSource for deterministic time
// tests. Returns a setter that advances by d on each call.
func withClock(t *testing.T, src gateway.IdentitySource, start time.Time) (advance func(d time.Duration)) {
	t.Helper()
	cur := start
	now := func() time.Time { return cur }
	advance = func(d time.Duration) { cur = cur.Add(d) }
	gateway.SetLDAPClockForTesting(src, now)
	return advance
}

// stopServer drains the source's pooled client connection (so gldap's
// connWg.Wait drops to zero) and then stops the server. Without the
// pre-close, srv.Stop() blocks forever on the held client conn.
func stopServer(t *testing.T, src gateway.IdentitySource, srv *gldap.Server) {
	t.Helper()
	if c, ok := src.(closableLDAP); ok {
		_ = c.Close()
	}
	_ = srv.Stop()
}

func TestLDAPSource_PositiveCache_FreshServesWithoutLDAP(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, srv := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.CacheTTL = time.Minute
	src := newLDAPSource(t, cfg)
	adv := withClock(t, src, time.Now())

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// Stop the server; a fresh cache must hide its absence.
	stopServer(t, src, srv)
	adv(30 * time.Second) // still within CacheTTL
	res, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("cached Resolve: %v", err)
	}
	if res.ServedStale {
		t.Errorf("ServedStale = true, want false for fresh cache hit")
	}
}

func TestLDAPSource_StaleGrace_ServesPriorKeysDuringOutage(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, srv := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.CacheTTL = 10 * time.Second
	cfg.StaleGrace = time.Minute
	src := newLDAPSource(t, cfg)
	adv := withClock(t, src, time.Now())

	// Warm.
	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("warm Resolve: %v", err)
	}
	stopServer(t, src, srv)

	// Past CacheTTL → next call tries live → errors → must serve stale.
	adv(15 * time.Second)
	res, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("stale Resolve: %v", err)
	}
	if !res.ServedStale {
		t.Errorf("ServedStale = false, want true (served from stale cache)")
	}
	if len(res.Keys) != 1 {
		t.Errorf("len Keys = %d, want 1 (stale entry preserved)", len(res.Keys))
	}
}

func TestLDAPSource_StaleGraceExpired_ReturnsError(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, srv := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.CacheTTL = 10 * time.Second
	cfg.StaleGrace = 20 * time.Second
	src := newLDAPSource(t, cfg)
	adv := withClock(t, src, time.Now())

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	stopServer(t, src, srv)

	adv(45 * time.Second) // past CacheTTL + StaleGrace
	if _, err := src.Resolve(context.Background(), "alice"); err == nil {
		t.Errorf("expected error past stale-grace window")
	}
}

func TestLDAPSource_NegativeCache_AvoidsLiveCallWithinTTL(t *testing.T) {
	url, ca, srv := fixtureLDAP(t, map[string][]string{})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.NegativeCacheTTL = time.Minute
	src := newLDAPSource(t, cfg)
	adv := withClock(t, src, time.Now())

	if _, err := src.Resolve(context.Background(), "ghost"); !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("first: %v", err)
	}
	stopServer(t, src, srv) // even if LDAP is down, negative cache must hold
	adv(30 * time.Second)
	if _, err := src.Resolve(context.Background(), "ghost"); !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("cached negative: %v", err)
	}
}

// countingFixture starts the LDAPS fixture with a search-hook that
// increments a counter on every Search. Returns the fixture details
// plus the counter pointer.
func countingFixture(t *testing.T, entries map[string][]string) (url, caPath string, counter *atomic.Int64, srv *gldap.Server) {
	t.Helper()
	counter = new(atomic.Int64)
	url, caPath, srv = fixtureLDAPWithSearchHook(t, entries, func() { counter.Add(1) })
	return
}

func TestLDAPSource_Singleflight_CollapsesConcurrentLookups(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, counter, _ := countingFixture(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src := newLDAPSource(t, cfg)

	// Reset outcome counter so we can assert it tracks wire calls.
	gateway.LDAPLookupsTotal.Reset()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = src.Resolve(context.Background(), "alice")
		}()
	}
	wg.Wait()

	got := counter.Load()
	if got < 1 || got > 5 {
		t.Errorf("LDAP searches under concurrent fan-out = %d, expected 1..5", got)
	}

	// The outcome metric must match the actual wire-call count.
	// Pre-fix, followers re-incremented "hit" so this read ~50.
	hitCount := testutil.ToFloat64(gateway.LDAPLookupsTotal.WithLabelValues("hit"))
	if int64(hitCount) != got {
		t.Errorf("LDAPLookupsTotal{outcome=hit} = %v, want %d (one inc per wire call, not per follower)", hitCount, got)
	}
}

func TestLDAPSource_Reconnect_AfterConnDropped(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, counter, _ := countingFixture(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	// Force every Resolve to hit the wire so we can count searches
	// directly. With CacheTTL=0 the positive-fresh check (age < ttl)
	// is always false, so the cache never short-circuits the live
	// path.
	cfg.CacheTTL = 0
	src := newLDAPSource(t, cfg)

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// Force the gateway-side conn to drop so the next Resolve must
	// redial.
	gateway.ResetLDAPConnForTesting(src)

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if got := counter.Load(); got < 2 {
		t.Errorf("expected >=2 LDAP searches after manual reset, got %d", got)
	}
}

func TestLDAPSource_WarnRateLimited(t *testing.T) {
	// Capture slog output to count WARN vs DEBUG emissions.
	var buf bytes.Buffer
	var bufMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{buf: &buf, mu: &bufMu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	url, ca, srv := fixtureLDAP(t, map[string][]string{})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.CacheTTL = time.Second
	cfg.NegativeCacheTTL = 0
	cfg.StaleGrace = 0
	src := newLDAPSource(t, cfg)
	adv := withClock(t, src, time.Now())

	// Take LDAP down so every Resolve produces a search/dial error.
	stopServer(t, src, srv)

	// Two failures within the 1-minute window: 1 warn, 1 debug.
	_, _ = src.Resolve(context.Background(), "alice")
	_, _ = src.Resolve(context.Background(), "alice")
	countLevel := func(level string) int {
		bufMu.Lock()
		defer bufMu.Unlock()
		return bytes.Count(buf.Bytes(), []byte(`"level":"`+level+`"`))
	}
	if got := countLevel("WARN"); got != 1 {
		t.Errorf("warns within window = %d, want 1", got)
	}
	if got := countLevel("DEBUG"); got != 1 {
		t.Errorf("debugs within window = %d, want 1", got)
	}

	// Advance past the 1-minute window — next failure warns again.
	adv(2 * time.Minute)
	_, _ = src.Resolve(context.Background(), "alice")
	if got := countLevel("WARN"); got != 2 {
		t.Errorf("warns after window = %d, want 2", got)
	}
}

// lockedWriter wraps a bytes.Buffer with a Mutex so slog's
// concurrent writer doesn't race against the test reading the buffer.
type lockedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
