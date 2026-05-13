// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"
)

// AuthPath captures how a session was authenticated.
type AuthPath struct {
	Kind        string `json:"kind"`                   // "direct" | "trusted_proxy"
	Alias       string `json:"alias,omitempty"`        // proxy alias when Kind=="trusted_proxy"
	User        string `json:"user,omitempty"`         // authenticated User name
	Pod         string `json:"pod,omitempty"`          // target DevPod
	Source      string `json:"source,omitempty"`       // "crd" | "ldap" | "" (proxy)
	ServedStale bool   `json:"served_stale,omitempty"` // true only when Source served from stale cache
	// LastSourceErr surfaces the most recent IdentitySource hard
	// error encountered before a successful or terminal verdict.
	// Implementations MUST scrub bind credentials, host details, and
	// pubkey bytes from the error before returning — this string
	// lands verbatim in audit logs.
	LastSourceErr string `json:"last_source_err,omitempty"`
}

// SessionOpen emits an audit event for a successfully authenticated
// session. Fields match the umbrella spec §8 audit shape.
func SessionOpen(logger *slog.Logger, sessionID string, ap AuthPath, clientIP, pubkeyFP string) {
	attrs := []any{
		"session_id", sessionID,
		"auth_path", ap.Kind,
		"user", ap.User,
		"devpod", ap.Pod,
		"client_ip", clientIP,
		"pubkey_fp", pubkeyFP,
	}
	if ap.Alias != "" {
		attrs = append(attrs, "proxy_alias", ap.Alias)
	}
	if ap.Source != "" {
		attrs = append(attrs, "source", ap.Source)
		// served_stale is only meaningful when a source actually ran
		// (i.e., not trusted-proxy auth).
		attrs = append(attrs, "served_stale", ap.ServedStale)
	}
	if ap.LastSourceErr != "" {
		attrs = append(attrs, "last_source_err", ap.LastSourceErr)
	}
	logger.Info("session_open", attrs...)
}

// SessionClose emits an audit event when a proxied session ends.
func SessionClose(logger *slog.Logger, sessionID string, duration time.Duration, bytesIn, bytesOut int64, reason string) {
	logger.Info("session_close",
		"session_id", sessionID,
		"duration_seconds", duration.Seconds(),
		"bytes_in", bytesIn,
		"bytes_out", bytesOut,
		"close_reason", reason,
	)
}

// AuthFailure emits an audit event for a rejected auth attempt.
// alias / pubkeyFP / user / pod / lastSourceErr may be empty when
// unknown at the failure site.
//
// lastSourceErr is the IdentitySource hard-error string the
// Authenticator recorded into AuthPath.LastSourceErr up to the
// failure point. It is critical for diagnosing an LDAP outage
// surfacing as "user_not_found" — without it, the audit row hides
// the real cause.
func AuthFailure(logger *slog.Logger, reason, authPath, alias, pubkeyFP, user, pod, lastSourceErr string) {
	attrs := []any{
		"reason", reason,
		"auth_path", authPath,
	}
	if pubkeyFP != "" {
		attrs = append(attrs, "pubkey_fp", pubkeyFP)
	}
	if alias != "" {
		attrs = append(attrs, "proxy_alias", alias)
	}
	if user != "" {
		attrs = append(attrs, "user", user)
	}
	if pod != "" {
		attrs = append(attrs, "devpod", pod)
	}
	if lastSourceErr != "" {
		attrs = append(attrs, "last_source_err", lastSourceErr)
	}
	logger.Info("auth_failure", attrs...)
}

// FingerprintOf returns the SHA256 fingerprint of an SSH public key.
func FingerprintOf(key ssh.PublicKey) string { return ssh.FingerprintSHA256(key) }
