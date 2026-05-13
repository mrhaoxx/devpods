// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

func TestMetricsRegisterAndCount(t *testing.T) {
	reg := prometheus.NewRegistry()
	gateway.MustRegisterMetrics(reg)

	gateway.SessionsTotal.WithLabelValues("alice", "demo", "direct", "ok").Inc()
	gateway.DialFailuresTotal.WithLabelValues("demo", "timeout").Inc()
	gateway.AuthFailuresTotal.WithLabelValues("user_not_found", "direct").Inc()
	gateway.SessionDurationSeconds.WithLabelValues("alice", "demo", "direct").Observe(1.5)

	// Each metric name should produce at least one series in the gather.
	got, err := testutil.GatherAndCount(reg,
		"devpod_gateway_sessions_total",
		"devpod_gateway_dial_failures_total",
		"devpod_gateway_auth_failures_total",
		"devpod_gateway_session_duration_seconds")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got < 4 {
		t.Errorf("expected at least 4 series, got %d", got)
	}

	// Dump the sessions counter and confirm the auth_path label shows up.
	dump, err := testutil.CollectAndFormat(gateway.SessionsTotal, expfmt.TypeTextPlain, "devpod_gateway_sessions_total")
	if err != nil {
		t.Fatalf("collect and format: %v", err)
	}
	if !strings.Contains(string(dump), "auth_path") {
		t.Errorf("auth_path label missing in dump:\n%s", dump)
	}
}

func TestNewMetrics_LDAPCountersRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	gateway.MustRegisterMetrics(reg)

	// Touch each labeled metric with a sentinel child so the registry
	// produces a metric family at gather time. Empty CounterVec /
	// GaugeVec collectors return no families until at least one child
	// exists; what we want to assert here is that all five names are
	// known to the registry, so creating a child is fine.
	gateway.LDAPLookupsTotal.WithLabelValues("hit")
	gateway.LDAPCacheEntries.WithLabelValues("positive")
	gateway.PubkeyParseErrorsTotal.WithLabelValues("crd")

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"devpod_gateway_ldap_lookups_total",
		"devpod_gateway_ldap_lookup_duration_seconds",
		"devpod_gateway_ldap_cache_entries",
		"devpod_gateway_ldap_connection_state",
		"devpod_gateway_ldap_pubkey_parse_errors_total",
	} {
		if !names[want] {
			t.Errorf("metric %q not registered", want)
		}
	}
}

func TestPubkeyParseErrorsTotal_IncBySource(t *testing.T) {
	reg := prometheus.NewRegistry()
	gateway.MustRegisterMetrics(reg)
	gateway.PubkeyParseErrorsTotal.Reset()
	gateway.PubkeyParseErrorsTotal.WithLabelValues("crd").Inc()
	gateway.PubkeyParseErrorsTotal.WithLabelValues("ldap").Inc()
	gateway.PubkeyParseErrorsTotal.WithLabelValues("ldap").Inc()
	count := func(label string) float64 {
		return testutil.ToFloat64(gateway.PubkeyParseErrorsTotal.WithLabelValues(label))
	}
	if got := count("crd"); got != 1 {
		t.Errorf("crd = %v, want 1", got)
	}
	if got := count("ldap"); got != 2 {
		t.Errorf("ldap = %v, want 2", got)
	}
}
