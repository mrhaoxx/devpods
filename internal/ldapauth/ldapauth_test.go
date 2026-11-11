// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package ldapauth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimlambrt/gldap"

	"github.com/mrhaoxx/devpod/internal/ldapauth"
)

const (
	serviceDN   = "cn=svc,ou=users,dc=example,dc=test"
	servicePass = "service-secret"
	filter      = `(&(objectClass=user)(memberOf=cn=staff,ou=groups,dc=example,dc=test)(cn={{.Username}}))`
)

// directory is a tiny LDAP server: a service account that may search,
// and people who may only bind as themselves.
type directory struct {
	mu       sync.Mutex
	people   map[string]string // cn → password
	twins    map[string]bool   // cn that matches two entries
	binds    []string          // every DN a bind was tried for
	searches []string          // every filter searched
	bound    map[int]string    // conn id → DN it is bound as
}

func (d *directory) dn(cn string) string { return "cn=" + cn + ",ou=users,dc=example,dc=test" }

func startDirectory(t *testing.T, people map[string]string) (*directory, ldapauth.Config) {
	t.Helper()
	d := &directory{people: people, twins: map[string]bool{}, bound: map[int]string{}}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	caPath := write("ca.crt", certPEM)
	passPath := write("password", []byte(servicePass+"\n"))
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	srv, err := gldap.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	mux, _ := gldap.NewMux()
	_ = mux.Bind(func(w *gldap.ResponseWriter, r *gldap.Request) {
		code := gldap.ResultInvalidCredentials
		if m, err := r.GetSimpleBindMessage(); err == nil {
			d.mu.Lock()
			d.binds = append(d.binds, m.UserName)
			ok := m.UserName == serviceDN && string(m.Password) == servicePass
			for cn, pw := range d.people {
				if m.UserName == d.dn(cn) && string(m.Password) == pw {
					ok = true
				}
			}
			// like a real directory: a bind without a password is
			// an anonymous bind, and succeeds
			if string(m.Password) == "" {
				ok = true
			}
			if ok {
				code = gldap.ResultSuccess
				d.bound[r.ConnectionID()] = m.UserName
			}
			d.mu.Unlock()
		}
		_ = w.Write(r.NewBindResponse(gldap.WithResponseCode(code)))
	})
	_ = mux.Search(func(w *gldap.ResponseWriter, r *gldap.Request) {
		m, _ := r.GetSearchMessage()
		d.mu.Lock()
		d.searches = append(d.searches, m.Filter)
		allowed := d.bound[r.ConnectionID()] == serviceDN
		var hits []string
		for cn := range d.people {
			if strings.Contains(m.Filter, "(cn="+cn+")") {
				hits = append(hits, d.dn(cn))
				if d.twins[cn] {
					hits = append(hits, "cn="+cn+",ou=other,dc=example,dc=test")
				}
			}
		}
		d.mu.Unlock()
		if !allowed {
			_ = w.Write(r.NewSearchDoneResponse(gldap.WithResponseCode(gldap.ResultInsufficientAccessRights)))
			return
		}
		for _, dn := range hits {
			_ = w.Write(r.NewSearchResponseEntry(dn))
		}
		_ = w.Write(r.NewSearchDoneResponse())
	})
	_ = srv.Router(mux)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	go func() {
		_ = srv.Run(addr, gldap.WithTLSConfig(&tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}))
	}()
	t.Cleanup(func() { _ = srv.Stop() })
	for deadline := time.Now().Add(2 * time.Second); !srv.Ready(); {
		if time.Now().After(deadline) {
			t.Fatal("directory not ready")
		}
		time.Sleep(time.Millisecond)
	}
	return d, ldapauth.Config{
		URL: "ldaps://" + addr, CAPath: caPath, BindDN: serviceDN, BindPasswordPath: passPath,
		BaseDN: "dc=example,dc=test", UserFilter: filter, Timeout: 2 * time.Second,
	}
}

func mustNew(t *testing.T, cfg ldapauth.Config) *ldapauth.Authenticator {
	t.Helper()
	a, err := ldapauth.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthenticate(t *testing.T) {
	d, cfg := startDirectory(t, map[string]string{"alice": "correct horse", "bob": "bob-pass", "twin": "twin-pass"})
	d.twins["twin"] = true
	a := mustNew(t, cfg)
	ctx := context.Background()
	check := func(user, pass string, want bool) {
		t.Helper()
		ok, err := a.Authenticate(ctx, user, pass)
		if err != nil || ok != want {
			t.Fatalf("Authenticate(%q, %q) = %v, %v; want %v", user, pass, ok, err, want)
		}
	}
	check("alice", "correct horse", true)
	check("alice", "wrong", false)
	check("alice", "bob-pass", false) // someone else's password
	check("bob", "bob-pass", true)
	check("nobody", "x", false)
	// two entries for one name: nobody is logged in as "one of them"
	check("twin", "twin-pass", false)

	// the search carries the configured filter — the group, too
	d.mu.Lock()
	first := d.searches[0]
	d.mu.Unlock()
	if first != `(&(objectClass=user)(memberOf=cn=staff,ou=groups,dc=example,dc=test)(cn=alice))` {
		t.Fatalf("filter %q", first)
	}
}

// An empty password is an anonymous bind to a directory, and succeeds:
// it must never reach one.
func TestAuthenticate_EmptyPasswordNeverReachesTheDirectory(t *testing.T) {
	d, cfg := startDirectory(t, map[string]string{"alice": "pw"})
	a := mustNew(t, cfg)
	if ok, err := a.Authenticate(context.Background(), "alice", ""); ok || err != nil {
		t.Fatalf("empty password: %v %v", ok, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.binds) != 0 {
		t.Fatalf("the directory was asked: %v", d.binds)
	}
}

// Only DevPod user names are looked up: nothing that could change the
// filter, and no upper case (the directory matches names without regard
// to case, DevPod does not).
func TestAuthenticate_OnlyDevPodUserNames(t *testing.T) {
	d, cfg := startDirectory(t, map[string]string{"alice": "pw"})
	a := mustNew(t, cfg)
	for _, name := range []string{"", "Alice", "alice)(cn=*", "*", "a b", "alice\x00", strings.Repeat("a", 33)} {
		if ok, err := a.Authenticate(context.Background(), name, "pw"); ok || err != nil {
			t.Fatalf("%q: %v %v", name, ok, err)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.binds) != 0 || len(d.searches) != 0 {
		t.Fatalf("the directory was asked: %v %v", d.binds, d.searches)
	}
}

// A directory that cannot be reached, or refuses the service account,
// is an error — not a wrong password.
func TestAuthenticate_DirectoryTrouble(t *testing.T) {
	_, cfg := startDirectory(t, map[string]string{"alice": "pw"})
	down := cfg
	down.URL = "ldaps://127.0.0.1:1"
	if ok, err := mustNew(t, down).Authenticate(context.Background(), "alice", "pw"); ok || err == nil {
		t.Fatalf("unreachable: %v %v", ok, err)
	}
	bad := cfg
	bad.BindPasswordPath = filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(bad.BindPasswordPath, []byte("not-the-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := mustNew(t, bad).Authenticate(context.Background(), "alice", "pw"); ok || err == nil {
		t.Fatalf("service account refused: %v %v", ok, err)
	}
}

func TestNew_RefusesWhatCannotWork(t *testing.T) {
	_, cfg := startDirectory(t, nil)
	for name, change := range map[string]func(c *ldapauth.Config){
		"plaintext":             func(c *ldapauth.Config) { c.URL = "ldap://127.0.0.1:389" },
		"no CA":                 func(c *ldapauth.Config) { c.CAPath = "/nonexistent" },
		"no bind password":      func(c *ldapauth.Config) { c.BindPasswordPath = "/nonexistent" },
		"filter without user":   func(c *ldapauth.Config) { c.UserFilter = "(objectClass=user)" },
		"filter not a template": func(c *ldapauth.Config) { c.UserFilter = "(cn={{.Username" },
		"no base":               func(c *ldapauth.Config) { c.BaseDN = "" },
	} {
		c := cfg
		change(&c)
		if _, err := ldapauth.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the default filter is the gateway's
	c := cfg
	c.UserFilter = ""
	if _, err := ldapauth.New(c); err != nil {
		t.Fatalf("default filter: %v", err)
	}
}
