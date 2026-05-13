// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import "github.com/prometheus/client_golang/prometheus"

// SessionsTotal counts authenticated SSH sessions handled by the
// gateway, labeled by user, devpod, auth_path ("direct" |
// "trusted_proxy"), and result ("ok" | "error").
var SessionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "devpod_gateway_sessions_total",
	Help: "Number of authenticated SSH sessions handled by the gateway.",
}, []string{"user", "devpod", "auth_path", "result"})

// DialFailuresTotal counts failures dialing the backend sshd, labeled
// by devpod name and a short reason (e.g. "timeout", "refused",
// "other").
var DialFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "devpod_gateway_dial_failures_total",
	Help: "Failures dialing the backend sshd.",
}, []string{"devpod", "reason"})

// AuthFailuresTotal counts SSH authentication failures, labeled by
// reason and auth_path.
var AuthFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "devpod_gateway_auth_failures_total",
	Help: "SSH authentication failures.",
}, []string{"reason", "auth_path"})

// SessionDurationSeconds is a histogram of authenticated session
// lifetimes, labeled by user, devpod, auth_path.
var SessionDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "devpod_gateway_session_duration_seconds",
	Help:    "Duration of authenticated SSH sessions.",
	Buckets: prometheus.ExponentialBuckets(1, 4, 8),
}, []string{"user", "devpod", "auth_path"})

// LDAPLookupsTotal counts LDAP source lookups by outcome.
//
//	outcome ∈ {hit, miss, error, cache_fresh, cache_stale}
var LDAPLookupsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "devpod_gateway_ldap_lookups_total",
	Help: "LDAP identity-source lookups, grouped by outcome.",
}, []string{"outcome"})

// LDAPLookupDuration is the wire-time histogram for live LDAP
// lookups (cache hits are not recorded).
var LDAPLookupDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "devpod_gateway_ldap_lookup_duration_seconds",
	Help:    "Wall-clock duration of live LDAP lookups (cache misses).",
	Buckets: prometheus.ExponentialBuckets(0.001, 4, 7), // 1ms .. ~4s
})

// LDAPCacheEntries is the current cache size by kind (positive vs
// negative). The gauge is set in bulk on every Resolve as a
// best-effort snapshot.
var LDAPCacheEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "devpod_gateway_ldap_cache_entries",
	Help: "Number of entries in the LDAP source cache.",
}, []string{"kind"})

// LDAPConnectionState is 1 while a bound LDAP connection is held,
// 0 when nil.
var LDAPConnectionState = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "devpod_gateway_ldap_connection_state",
	Help: "1 if an LDAP bind is held, 0 otherwise.",
})

// PubkeyParseErrorsTotal counts authorized_keys lines we couldn't
// parse, labeled by source ("crd"|"ldap").
var PubkeyParseErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "devpod_gateway_ldap_pubkey_parse_errors_total",
	Help: "authorized_keys lines that failed to parse, by source.",
}, []string{"source"})

// MustRegisterMetrics registers all gateway metrics with the given
// registry. Intended to be called once during process startup.
func MustRegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		SessionsTotal,
		DialFailuresTotal,
		AuthFailuresTotal,
		SessionDurationSeconds,
		LDAPLookupsTotal,
		LDAPLookupDuration,
		LDAPCacheEntries,
		LDAPConnectionState,
		PubkeyParseErrorsTotal,
	)
}
