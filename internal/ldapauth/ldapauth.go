// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package ldapauth checks a user name and password against an LDAP
// directory: the console's sign-in for users whose identity lives there
// (the gateway takes their SSH keys from the same directory).
//
// One check is search-then-bind: the service account finds the one
// entry the configured filter gives for the name, then the entry's DN
// is bound with the password the user typed. The filter is the
// gateway's, so whoever may log in over SSH may sign in here — a group
// named in it applies to both.
package ldapauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// Config is GatewayConfig.spec.ldap as the console uses it. The paths
// are files mounted from the Secrets the spec names.
type Config struct {
	URL              string // ldaps:// only
	CAPath           string // PEM CA bundle
	BindDN           string // the service account
	BindPasswordPath string
	BaseDN           string
	UserFilter       string // text/template over {.Username}; empty: DefaultUserFilter
	Timeout          time.Duration
}

// DefaultUserFilter is the gateway's default.
const DefaultUserFilter = `(&(objectClass=posixAccount)(uid={{.Username}}))`

// userRE: a DevPod user name. The directory compares names without
// regard to case and DevPod does not, so an upper-case spelling of a
// name is not that user.
var userRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Authenticator checks passwords against one directory.
type Authenticator struct {
	cfg      Config
	bindPass string
	tls      *tls.Config
	filter   *template.Template
}

// New validates cfg and reads the CA and the service account's
// password. It does not contact the directory.
func New(cfg Config) (*Authenticator, error) {
	if !strings.HasPrefix(cfg.URL, "ldaps://") {
		return nil, fmt.Errorf("ldap: URL must use ldaps://, got %q", cfg.URL)
	}
	if cfg.BaseDN == "" || cfg.BindDN == "" {
		return nil, errors.New("ldap: baseDN and bindDN are required")
	}
	ca, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("ldap: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bytes.TrimSpace(ca)) {
		return nil, errors.New("ldap: the CA file holds no PEM certificate")
	}
	pw, err := os.ReadFile(cfg.BindPasswordPath)
	if err != nil {
		return nil, fmt.Errorf("ldap: read bind password: %w", err)
	}
	src := cfg.UserFilter
	if src == "" {
		src = DefaultUserFilter
	}
	tmpl, err := template.New("ldap-filter").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("ldap: parse userFilter: %w", err)
	}
	// a filter that ignores the name would find the same entry for
	// every name
	const probe = "devpodprobe0"
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"Username": probe}); err != nil {
		return nil, fmt.Errorf("ldap: userFilter: %w", err)
	}
	if !strings.Contains(buf.String(), probe) {
		return nil, fmt.Errorf("ldap: userFilter must use {{.Username}}; got %q", src)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	host := cfg.URL
	if u, err := url.Parse(cfg.URL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return &Authenticator{
		cfg:      cfg,
		bindPass: string(bytes.TrimRight(pw, "\r\n")),
		tls:      &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12},
		filter:   tmpl,
	}, nil
}

// Authenticate reports whether password is username's in the
// directory. false with a nil error is "no": no such user (for the
// filter), more than one, or a wrong password. An error is the
// directory itself: unreachable, or refusing the service account.
func (a *Authenticator) Authenticate(_ context.Context, username, password string) (bool, error) {
	// An empty password would be an anonymous bind, which a directory
	// accepts whatever the DN: it never gets that far.
	if password == "" || !userRE.MatchString(username) {
		return false, nil
	}
	conn, err := ldap.DialURL(a.cfg.URL, ldap.DialWithTLSConfig(a.tls))
	if err != nil {
		return false, fmt.Errorf("ldap dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetTimeout(a.cfg.Timeout)
	if err := conn.Bind(a.cfg.BindDN, a.bindPass); err != nil {
		return false, fmt.Errorf("ldap service bind: %w", err)
	}
	var filter bytes.Buffer
	if err := a.filter.Execute(&filter, map[string]string{"Username": ldap.EscapeFilter(username)}); err != nil {
		return false, fmt.Errorf("ldap filter: %w", err)
	}
	secs := int((a.cfg.Timeout + time.Second - 1) / time.Second)
	res, err := conn.Search(ldap.NewSearchRequest(
		a.cfg.BaseDN, ldap.ScopeWholeSubtree, ldap.DerefAlways,
		2, secs, false, filter.String(), []string{"1.1"}, nil, // "1.1": no attributes, the DN is all that is needed
	))
	if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return false, fmt.Errorf("ldap search: %w", err)
	}
	// none, or more than one: nobody is signed in as "one of them"
	if res == nil || len(res.Entries) != 1 || res.Entries[0].DN == "" {
		return false, nil
	}
	if err := conn.Bind(res.Entries[0].DN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) ||
			ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) ||
			ldap.IsErrorWithCode(err, ldap.LDAPResultUnwillingToPerform) ||
			ldap.IsErrorWithCode(err, ldap.LDAPResultConstraintViolation) {
			return false, nil
		}
		return false, fmt.Errorf("ldap user bind: %w", err)
	}
	return true, nil
}
