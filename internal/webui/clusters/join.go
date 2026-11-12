// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
)

// A cluster joins with a token its admin was shown once. Only the hash is
// kept, on the Cluster object, with the moment it stops working; a join
// records the agent's public key and erases the token.
const (
	TokenHashAnnotation    = "devpod.io/join-token-sha256"
	TokenExpiresAnnotation = "devpod.io/join-token-expires"
	// TokenLifetime is how long a join token works.
	TokenLifetime = 24 * time.Hour
)

// ErrJoinRefused: the token is wrong, used or expired, or the cluster
// already has another key. Which of these is not said.
var ErrJoinRefused = errors.New("join refused")

// NewToken makes a join token and the hash to store.
func NewToken() (token, hash string) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = "dpj_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueToken puts a fresh token on c (in memory) and returns it. Any key
// the cluster joined with before is cleared: a new token is how an agent
// is replaced.
func IssueToken(c *devpodv1alpha1.Cluster, now time.Time) string {
	return issue(c, &c.Spec.AgentPublicKey, now)
}

// IssueGatewayToken is IssueToken for a gateway.
func IssueGatewayToken(g *devpodv1alpha1.Gateway, now time.Time) string {
	return issue(g, &g.Spec.PublicKey, now)
}

func issue(obj client.Object, key *string, now time.Time) string {
	token, hash := NewToken()
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[TokenHashAnnotation] = hash
	ann[TokenExpiresAnnotation] = now.Add(TokenLifetime).UTC().Format(time.RFC3339)
	obj.SetAnnotations(ann)
	*key = ""
	return token
}

// Join records publicKey as the cluster's agent, if token is the one the
// cluster was issued and still good. Joining again with the key already
// recorded is fine (an agent restarted with its token file still there).
func (r *Registry) Join(ctx context.Context, cluster, token, publicKey string) error {
	var c devpodv1alpha1.Cluster
	return r.join(ctx, cluster, &c, &c.Spec.AgentPublicKey, token, publicKey)
}

// JoinGateway is Join for a gateway.
func (r *Registry) JoinGateway(ctx context.Context, gateway, token, publicKey string) error {
	var g devpodv1alpha1.Gateway
	return r.join(ctx, gateway, &g, &g.Spec.PublicKey, token, publicKey)
}

// join reads the object name into obj and records publicKey in *key (a
// field of obj).
func (r *Registry) join(ctx context.Context, name string, obj client.Object, key *string, token, publicKey string) error {
	publicKey = strings.TrimSpace(publicKey)
	if err := agent.Verify(publicKey, nil, nil); err != nil && !strings.Contains(err.Error(), "proof") {
		return ErrJoinRefused // not an Ed25519 key
	}
	if err := r.o.Reader.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrJoinRefused
		}
		return err
	}
	if *key != "" {
		if *key == publicKey {
			return nil
		}
		return ErrJoinRefused
	}
	ann := obj.GetAnnotations()
	want := ann[TokenHashAnnotation]
	expires, perr := time.Parse(time.RFC3339, ann[TokenExpiresAnnotation])
	if want == "" || perr != nil || time.Now().After(expires) ||
		subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(want)) != 1 {
		return ErrJoinRefused
	}
	*key = publicKey
	delete(ann, TokenHashAnnotation)
	delete(ann, TokenExpiresAnnotation)
	obj.SetAnnotations(ann)
	// the update carries the version read above: of two joins with one
	// token, the second finds the object changed and is refused
	if err := r.o.Hub.Update(ctx, obj); err != nil {
		if apierrors.IsConflict(err) {
			return ErrJoinRefused
		}
		return err
	}
	return nil
}
