// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Sentinel errors returned by Authenticate. Callers can use errors.Is.
var (
	ErrUserNotFound    = errors.New("user not found")
	ErrPubkeyMismatch  = errors.New("pubkey does not match any identity source")
	ErrDevPodNotFound  = errors.New("devpod not found")
	ErrDevPodNotReady  = errors.New("devpod not running")
	ErrAccessDenied    = errors.New("access denied: not owner or collaborator")
	ErrLoginNameFormat = errors.New("login name must be <user>+<pod>")
)

// AuthError augments Authenticate's typed errors with the AuthPath
// partially populated up to the point of failure. Callers (notably
// the gateway audit path) can errors.As-recover it to surface
// AuthPath.LastSourceErr — otherwise an LDAP outage shows up as a
// bare "user_not_found" with no clue about the upstream failure.
//
// Wrap-policy: every Authenticate error return wraps via *AuthError.
// errors.Is(err, sentinel) still works because Unwrap is plumbed.
type AuthError struct {
	Err      error
	AuthPath AuthPath
}

func (e *AuthError) Error() string { return e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

// AuthResult is what Authenticate returns on success.
type AuthResult struct {
	User       string
	DevPodName string
	Endpoint   string
	AuthPath   AuthPath
	// Cluster the DevPod is in; "" where the gateway serves one cluster.
	Cluster string
	// Ambiguous, when set, replaces the rest: the key was good, but
	// several clusters have a DevPod of that name this user may log in
	// to. These are the logins that say which. Told only to someone whose
	// key was accepted.
	Ambiguous []string
}

// Authenticator validates SSH client public keys against an ordered
// chain of IdentitySources and resolves the target DevPod.
type Authenticator struct {
	dir       Directory
	dpNS      string
	proxyKeys map[string]string // SHA256 fingerprint → alias
	sources   []IdentitySource  // ordered; first match wins
}

// NewAuthenticator returns an Authenticator pre-loaded with a single
// crdSource. Use WithSources(...) to install a multi-source chain
// (e.g., crdSource then ldapSource).
func NewAuthenticator(c client.Reader, devpodNamespace string) *Authenticator {
	return &Authenticator{
		dir:     NewKubeDirectory(c, devpodNamespace),
		dpNS:    devpodNamespace,
		sources: []IdentitySource{NewCRDSource(c)},
	}
}

// WithDirectory replaces where DevPods are looked up (a gateway of
// several clusters asks the console).
func (a *Authenticator) WithDirectory(d Directory) *Authenticator {
	a.dir = d
	return a
}

// WithProxyKeys attaches the trusted-proxy index. Pass nil/empty to
// disable trusted-proxy auth.
func (a *Authenticator) WithProxyKeys(idx map[string]string) *Authenticator {
	a.proxyKeys = idx
	return a
}

// WithSources installs the ordered identity-source chain. Sources are
// tried in slice order; the first one to authorize the offered key
// wins.
func (a *Authenticator) WithSources(srcs []IdentitySource) *Authenticator {
	a.sources = srcs
	return a
}

// Authenticate validates the offered SSH key for loginName and
// resolves the target DevPod.
//
// On error, the returned value is always a *AuthError whose AuthPath
// carries whatever was populated up to the point of failure (User /
// Pod once the login string parses, LastSourceErr once a source has
// hard-erred, etc). errors.Is against the sentinels still works.
func (a *Authenticator) Authenticate(ctx context.Context, loginName string, key ssh.PublicKey) (*AuthResult, error) {
	var ap AuthPath

	user, pod, cluster, err := ParseLogin(loginName)
	if err != nil {
		return nil, &AuthError{Err: fmt.Errorf("%w: %v", ErrLoginNameFormat, err), AuthPath: ap}
	}
	ap.User = user
	ap.Pod = pod

	// Look the DevPod up first so a bad pod name fails fast regardless
	// of identity-source latency.
	//
	// The login is "<owner>+<pod>", but the DevPod resource is named
	// "<owner>-<pod>" so pod names only need to be unique per owner, not
	// cluster-wide (many users can each have a "day2"); the bare "<pod>"
	// name is for DevPods created before owner-scoped naming.
	found, getErr := a.dir.Lookup(ctx, user, pod, cluster)
	if getErr != nil {
		return nil, &AuthError{Err: fmt.Errorf("get devpod: %w", getErr), AuthPath: ap}
	}
	if len(found) == 0 {
		return nil, &AuthError{Err: fmt.Errorf("%w: %q (owner %q) in ns %q", ErrDevPodNotFound, pod, user, a.dpNS), AuthPath: ap}
	}

	// Trusted-proxy short-circuit — fingerprint-only, identity
	// sources not consulted.
	if alias, ok := a.proxyKeys[ssh.FingerprintSHA256(key)]; ok {
		ap.Kind = "trusted_proxy"
		ap.Alias = alias
		return a.finalize(found, user, pod, ap)
	}

	// Resolve via ordered sources.
	sources := a.sources

	ap.Kind = "direct"
	matched := false
	anyKnown := false
	for _, src := range sources {
		res, rerr := src.Resolve(ctx, user)
		if errors.Is(rerr, ErrIdentityNotFound) {
			continue
		}
		if rerr != nil {
			// Real failure on this source — record and continue.
			ap.LastSourceErr = src.Name() + ": " + rerr.Error()
			continue
		}
		anyKnown = true
		if matchesAnyParsed(key, res.Keys) {
			ap.Source = src.Name()
			ap.ServedStale = res.ServedStale
			matched = true
			break
		}
		// Source knew this user but the key didn't match — fall
		// through to the next source (the "static-first, LDAP-
		// fallback" contract).
	}
	if !matched {
		if anyKnown {
			return nil, &AuthError{Err: fmt.Errorf("%w: user %q", ErrPubkeyMismatch, user), AuthPath: ap}
		}
		return nil, &AuthError{Err: fmt.Errorf("%w: %q", ErrUserNotFound, user), AuthPath: ap}
	}

	return a.finalize(found, user, pod, ap)
}

// finalize runs the shared post-match checks (access + readiness) on
// what the login could mean. Among several clusters, what counts is what
// this user may log in to: one such DevPod is the answer; several are
// told apart by the caller (AuthResult.Ambiguous).
func (a *Authenticator) finalize(found []Found, user, pod string, ap AuthPath) (*AuthResult, error) {
	var mine []Found
	for _, f := range found {
		if accessAllowed(f.DevPod, user) {
			mine = append(mine, f)
		}
	}
	switch {
	case len(mine) == 0:
		return nil, &AuthError{Err: fmt.Errorf("%w: user %q on devpod %q", ErrAccessDenied, user, pod), AuthPath: ap}
	case len(mine) > 1:
		res := &AuthResult{User: user, AuthPath: ap}
		for _, f := range mine {
			res.Ambiguous = append(res.Ambiguous, user+"+"+pod+"+"+f.Cluster)
		}
		return res, nil
	}
	dp := mine[0].DevPod
	if dp.Status.Phase != devpodv1alpha1.DevPodRunning || dp.Status.Endpoint == "" {
		return nil, &AuthError{
			Err:      fmt.Errorf("%w: %q phase=%q endpoint=%q", ErrDevPodNotReady, pod, dp.Status.Phase, dp.Status.Endpoint),
			AuthPath: ap,
		}
	}
	return &AuthResult{
		User:       user,
		DevPodName: dp.Name,
		Endpoint:   dp.Status.Endpoint,
		AuthPath:   ap,
		Cluster:    mine[0].Cluster,
	}, nil
}

// accessAllowed returns true if user is the owner or a collaborator.
func accessAllowed(dp *devpodv1alpha1.DevPod, user string) bool {
	if dp.Spec.Owner == user {
		return true
	}
	for _, c := range dp.Spec.Collaborators {
		if c == user {
			return true
		}
	}
	return false
}

// matchesAnyParsed returns true if any pre-parsed pubkey in keys
// equals offered.
func matchesAnyParsed(offered ssh.PublicKey, keys []ssh.PublicKey) bool {
	want := offered.Marshal()
	for _, k := range keys {
		if bytes.Equal(k.Marshal(), want) {
			return true
		}
	}
	return false
}
