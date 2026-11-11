// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// The CLI over SSH is addressed like a DevPod: the login names the user
// first, then the CLI's reserved name where a DevPod's name would be:
//
//	<user>+devpod   that user, proven by the offered key through the
//	                usual identity sources (User, then LDAP) — the same
//	                check as a DevPod login; a trusted proxy says whom it
//	                acts for the same way
//
// The key is never used to find out who the caller is.

// ParseCLILogin reports whether login addresses the CLI, <user>+cliUser,
// and for which user. An empty cliUser disables the CLI. A DevPod whose
// short name equals cliUser cannot be reached by SSH while the CLI is on.
func ParseCLILogin(cliUser, login string) (user string, ok bool) {
	if cliUser == "" {
		return "", false
	}
	if u, found := strings.CutSuffix(login, "+"+cliUser); found && userRE.MatchString(u) {
		return u, true
	}
	return "", false
}

var userRE = loginUserRE()

// AuthenticateCLI checks a CLI login: the key must belong to user (or be
// a trusted proxy's, acting for a user that exists). Errors wrap the same
// sentinels as Authenticate.
func (a *Authenticator) AuthenticateCLI(ctx context.Context, user string, key ssh.PublicKey) (string, AuthPath, error) {
	ap := AuthPath{User: user}
	if alias, ok := a.proxyKeys[ssh.FingerprintSHA256(key)]; ok {
		ap.Kind, ap.Alias = "trusted_proxy", alias
		// the proxy vouches for the user, but the user must exist
		known := false
		for _, src := range a.sources {
			_, err := src.Resolve(ctx, user)
			if err == nil {
				known = true
				break
			}
			if !errors.Is(err, ErrIdentityNotFound) {
				ap.LastSourceErr = src.Name() + ": " + err.Error()
			}
		}
		if !known {
			return "", ap, &AuthError{Err: fmt.Errorf("%w: %q", ErrUserNotFound, user), AuthPath: ap}
		}
		return user, ap, nil
	}

	ap.Kind = "direct"
	anyKnown := false
	for _, src := range a.sources {
		res, err := src.Resolve(ctx, user)
		if errors.Is(err, ErrIdentityNotFound) {
			continue
		}
		if err != nil {
			ap.LastSourceErr = src.Name() + ": " + err.Error()
			continue
		}
		anyKnown = true
		if matchesAnyParsed(key, res.Keys) {
			ap.Source, ap.ServedStale = src.Name(), res.ServedStale
			return user, ap, nil
		}
	}
	if anyKnown {
		return "", ap, &AuthError{Err: fmt.Errorf("%w: user %q", ErrPubkeyMismatch, user), AuthPath: ap}
	}
	return "", ap, &AuthError{Err: fmt.Errorf("%w: %q", ErrUserNotFound, user), AuthPath: ap}
}
