// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

func newCapture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestSessionOpenEmitsRequiredFields(t *testing.T) {
	logger, buf := newCapture()
	ap := gateway.AuthPath{Kind: "direct", User: "alice", Pod: "demo"}
	gateway.SessionOpen(logger, "sid-1", ap, "10.0.0.1:55001", "SHA256:abcd")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"msg", "session_id", "auth_path", "user", "devpod", "client_ip", "pubkey_fp"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
	if rec["auth_path"] != "direct" {
		t.Errorf("auth_path = %v, want direct", rec["auth_path"])
	}
	if _, has := rec["proxy_alias"]; has {
		t.Errorf("proxy_alias should be absent for direct auth")
	}
}

func TestSessionOpenTrustedProxyIncludesAlias(t *testing.T) {
	logger, buf := newCapture()
	ap := gateway.AuthPath{Kind: "trusted_proxy", Alias: "corp", User: "alice", Pod: "demo"}
	gateway.SessionOpen(logger, "sid-2", ap, "1.2.3.4:443", "SHA256:abcd")

	var rec map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec)
	if rec["auth_path"] != "trusted_proxy" {
		t.Errorf("auth_path = %v", rec["auth_path"])
	}
	if rec["proxy_alias"] != "corp" {
		t.Errorf("proxy_alias = %v, want corp", rec["proxy_alias"])
	}
}

func TestSessionCloseRecordsByteCountsAndDuration(t *testing.T) {
	logger, buf := newCapture()
	gateway.SessionClose(logger, "sid-3", 750*time.Millisecond, 1024, 2048, "client_disconnect")

	var rec map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec)
	if rec["session_id"] != "sid-3" {
		t.Errorf("session_id wrong: %v", rec["session_id"])
	}
	if rec["bytes_in"] != float64(1024) || rec["bytes_out"] != float64(2048) {
		t.Errorf("byte counts wrong: %v / %v", rec["bytes_in"], rec["bytes_out"])
	}
	if rec["close_reason"] != "client_disconnect" {
		t.Errorf("close_reason wrong: %v", rec["close_reason"])
	}
	if rec["duration_seconds"] != 0.75 {
		t.Errorf("duration_seconds wrong: %v", rec["duration_seconds"])
	}
}

func TestAuthFailureEmitsReasonAndPath(t *testing.T) {
	logger, buf := newCapture()
	gateway.AuthFailure(logger, "user_not_found", "trusted_proxy", "corp", "SHA256:xy", "alice", "demo", "")

	var rec map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec)
	if rec["reason"] != "user_not_found" || rec["auth_path"] != "trusted_proxy" || rec["proxy_alias"] != "corp" {
		t.Errorf("auth_failure record wrong: %#v", rec)
	}
	if _, ok := rec["last_source_err"]; ok {
		t.Errorf("last_source_err should be absent when empty")
	}
}

func TestAuthFailureIncludesLastSourceErr(t *testing.T) {
	logger, buf := newCapture()
	gateway.AuthFailure(logger, "user_not_found", "direct", "", "SHA256:xy", "alice", "demo", "ldap: dial timeout")

	var rec map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec)
	if rec["last_source_err"] != "ldap: dial timeout" {
		t.Errorf("last_source_err = %v, want %q", rec["last_source_err"], "ldap: dial timeout")
	}
}

func TestSessionOpen_EmitsSourceFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	ap := gateway.AuthPath{
		Kind:        "direct",
		User:        "alice",
		Pod:         "smoke",
		Source:      "ldap",
		ServedStale: true,
	}
	gateway.SessionOpen(logger, "sess-1", ap, "1.2.3.4:5678", "SHA256:abc")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if got["source"] != "ldap" {
		t.Errorf("source = %v, want %q", got["source"], "ldap")
	}
	if got["served_stale"] != true {
		t.Errorf("served_stale = %v, want true", got["served_stale"])
	}
}

func TestSessionOpen_OmitsSourceForTrustedProxy(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	ap := gateway.AuthPath{
		Kind:  "trusted_proxy",
		User:  "alice",
		Pod:   "smoke",
		Alias: "fw1",
	}
	gateway.SessionOpen(logger, "sess-2", ap, "1.2.3.4:5678", "SHA256:abc")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if _, ok := got["source"]; ok {
		t.Errorf("source unexpectedly present: %v", got["source"])
	}
	if v, ok := got["served_stale"]; ok && v != false {
		t.Errorf("served_stale unexpectedly true: %v", v)
	}
}
