// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/gwlink"
)

// A gateway of several clusters has no cluster to read: it asks the
// console (gwlink), which answers for every cluster it manages.

// NewConsoleDirectory looks DevPods up at the console.
func NewConsoleDirectory(c *gwlink.Client) Directory { return consoleDirectory{c} }

type consoleDirectory struct{ c *gwlink.Client }

func (d consoleDirectory) Lookup(ctx context.Context, user, pod, cluster string) ([]Found, error) {
	found, _, err := d.c.Lookup(ctx, user, pod, cluster)
	if err != nil {
		return nil, err
	}
	out := make([]Found, 0, len(found))
	for _, f := range found {
		if f.DevPod != nil {
			out = append(out, Found{Cluster: f.Cluster, DevPod: f.DevPod})
		}
	}
	return out, nil
}

func (d consoleDirectory) ByHTTPID(ctx context.Context, id string) (*Found, error) {
	f, _, err := d.c.ByHTTPID(ctx, id)
	if errors.Is(err, gwlink.ErrNotFound) {
		return nil, nil
	}
	if err != nil || f.DevPod == nil {
		return nil, err
	}
	return &Found{Cluster: f.Cluster, DevPod: f.DevPod}, nil
}

// NewConsoleSource resolves a user's keys from those registered at the
// console.
func NewConsoleSource(c *gwlink.Client) IdentitySource { return consoleSource{c} }

type consoleSource struct{ c *gwlink.Client }

func (s consoleSource) Name() string { return "console" }

func (s consoleSource) Resolve(ctx context.Context, username string) (ResolveResult, error) {
	lines, stale, err := s.c.Keys(ctx, username)
	if errors.Is(err, gwlink.ErrNotFound) {
		return ResolveResult{}, ErrIdentityNotFound
	}
	if err != nil {
		return ResolveResult{}, fmt.Errorf("console keys of %q: %w", username, err)
	}
	keys := make([]ssh.PublicKey, 0, len(lines))
	for _, line := range lines {
		parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
		if perr != nil {
			PubkeyParseErrorsTotal.WithLabelValues("console").Inc()
			continue
		}
		keys = append(keys, parsed)
	}
	return ResolveResult{Keys: keys, ServedStale: stale}, nil
}
