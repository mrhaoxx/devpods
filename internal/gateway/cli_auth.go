// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// CLI logins (ssh devpod@gateway <command>) name no DevPod, so who is
// calling has to come from somewhere else than "<user>+<pod>":
//
//	devpod          the User whose pubkeys (User CRD) contain the offered key
//	devpod+<user>   that user, proven by the key through the usual identity
//	                sources (CRD, then LDAP); a trusted proxy must use this
//	                form — it is how a proxy says whom it acts for
var (
	ErrCLINeedsUser    = errors.New("log in as <cli-user>+<user>: the key alone does not say who you are")
	ErrCLIAmbiguousKey = errors.New("this key belongs to more than one user; log in as <cli-user>+<user>")
)

// ParseCLILogin reports whether login addresses the CLI: exactly cliUser,
// or cliUser+<user>. An empty cliUser disables the CLI.
func ParseCLILogin(cliUser, login string) (user string, ok bool) {
	if cliUser == "" {
		return "", false
	}
	if login == cliUser {
		return "", true
	}
	if rest, found := strings.CutPrefix(login, cliUser+"+"); found && userRE.MatchString(rest) {
		return rest, true
	}
	return "", false
}

var userRE = loginUserRE()

// AuthenticateCLI resolves who a CLI login is. user is "" for the bare
// form. Errors wrap the same sentinels as Authenticate.
func (a *Authenticator) AuthenticateCLI(ctx context.Context, user string, key ssh.PublicKey) (string, AuthPath, error) {
	ap := AuthPath{User: user}
	if alias, ok := a.proxyKeys[ssh.FingerprintSHA256(key)]; ok {
		ap.Kind, ap.Alias = "trusted_proxy", alias
		if user == "" {
			return "", ap, &AuthError{Err: ErrCLINeedsUser, AuthPath: ap}
		}
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
	if user == "" {
		var list devpodv1alpha1.UserList
		if err := a.c.List(ctx, &list); err != nil {
			return "", ap, &AuthError{Err: fmt.Errorf("list users: %w", err), AuthPath: ap}
		}
		want := string(key.Marshal())
		var owners []string
		for i := range list.Items {
			for _, line := range list.Items[i].Spec.Pubkeys {
				if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil && string(pk.Marshal()) == want {
					owners = append(owners, list.Items[i].Name)
					break
				}
			}
		}
		sort.Strings(owners)
		switch len(owners) {
		case 1:
			ap.User, ap.Source = owners[0], "crd"
			return owners[0], ap, nil
		case 0:
			// not a User CRD key: an LDAP user has to say who they are
			return "", ap, &AuthError{Err: fmt.Errorf("%w: %w", ErrPubkeyMismatch, ErrCLINeedsUser), AuthPath: ap}
		default:
			return "", ap, &AuthError{Err: ErrCLIAmbiguousKey, AuthPath: ap}
		}
	}

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
