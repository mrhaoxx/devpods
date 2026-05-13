// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// ErrIdentityNotFound — the source does not know this username.
// Callers should try the next source in the chain. This is not an
// error in the log-it-and-page sense; it's the normal "I have nothing
// for this name" signal.
var ErrIdentityNotFound = errors.New("identity not found in this source")

// IdentitySource resolves a username to a set of authorized SSH
// public keys plus per-call metadata. Implementations do NOT compare
// keys; that stays in Authenticator so the comparison and the
// cross-source ordering live in one place.
//
// Error strings returned from Resolve appear verbatim in audit logs
// (audit.go:LastSourceErr); implementations MUST NOT include bind
// credentials, host details, or pubkey bytes in error messages.
type IdentitySource interface {
	Resolve(ctx context.Context, username string) (ResolveResult, error)
	Name() string
}

// ResolveResult is the success return of IdentitySource.Resolve.
// It is per-call (no shared state), so concurrent calls cannot
// observe each other's metadata.
type ResolveResult struct {
	// Keys are the authorized SSH pubkeys for the username.
	Keys []ssh.PublicKey
	// ServedStale is true when the source returned the keys from a
	// stale-while-error cache during an outage of its upstream.
	// Carried through to AuthPath.ServedStale for the audit row.
	ServedStale bool
}

// NewCRDSource returns an IdentitySource backed by the User CRD.
// r is typically the controller-runtime cache client; the User CRD
// is cluster-scoped so no namespace argument is needed.
func NewCRDSource(r client.Reader) IdentitySource {
	return &crdSource{r: r}
}

type crdSource struct {
	r client.Reader
}

func (s *crdSource) Name() string { return "crd" }

func (s *crdSource) Resolve(ctx context.Context, username string) (ResolveResult, error) {
	var u devpodv1alpha1.User
	if err := s.r.Get(ctx, types.NamespacedName{Name: username}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return ResolveResult{}, ErrIdentityNotFound
		}
		return ResolveResult{}, fmt.Errorf("crd get user %q: %w", username, err)
	}
	keys := make([]ssh.PublicKey, 0, len(u.Spec.Pubkeys))
	for _, line := range u.Spec.Pubkeys {
		parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
		if perr != nil {
			// Drop and continue: a single bad line shouldn't lock a
			// User out. The metric is the operator-visible signal.
			PubkeyParseErrorsTotal.WithLabelValues("crd").Inc()
			continue
		}
		keys = append(keys, parsed)
	}
	return ResolveResult{Keys: keys}, nil
}
