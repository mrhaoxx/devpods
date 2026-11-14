// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"strings"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// A session's record holds the fingerprint of the key it signed in with.
// Which key that is — "alice@laptop" — is what the user's keys say now:
// the console names it at the time of looking (a key since removed, or
// one only the directory knows, stays a fingerprint).

// keyNames maps each user's key fingerprints to the keys' comments (their
// type when they have none).
func (s *Server) keyNames(ctx context.Context, users []string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, name := range users {
		if _, done := out[name]; done || name == "" {
			continue
		}
		names := map[string]string{}
		out[name] = names
		var u devpodv1alpha1.User
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &u); err != nil {
			continue
		}
		for _, line := range u.Spec.Pubkeys {
			pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				continue
			}
			if comment = strings.TrimSpace(comment); comment == "" {
				comment = pk.Type()
			}
			names[ssh.FingerprintSHA256(pk)] = comment
		}
	}
	return out
}

// sessionItem is a listed session with the name of the key it used.
type sessionItem struct {
	store.SessionSummary
	PubkeyName string `json:"pubkey_name,omitempty"`
}

// sessionDetailView is a session's detail with the name of its key.
type sessionDetailView struct {
	*store.SessionDetail
	PubkeyName string `json:"pubkey_name,omitempty"`
}

func (s *Server) namedSessions(ctx context.Context, items []store.SessionSummary) []sessionItem {
	users := make([]string, 0, len(items))
	for _, it := range items {
		users = append(users, it.User)
	}
	names := s.keyNames(ctx, users)
	out := make([]sessionItem, len(items))
	for i, it := range items {
		out[i] = sessionItem{SessionSummary: it, PubkeyName: names[it.User][it.PubkeyFP]}
	}
	return out
}
