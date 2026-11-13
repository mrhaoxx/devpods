// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func TestKeys(t *testing.T) {
	utc := time.Date(2026, 11, 9, 23, 59, 0, 0, time.UTC)
	if got := RawKey(utc, "01JC9ABC", 1); got != "raw/2026/11/09/01JC9ABC/000001.seg.zst" {
		t.Errorf("RawKey = %q", got)
	}
	if got := FileKey(utc, "01JC9ABC", 3, 1); got != "files/2026/11/09/01JC9ABC/3-1" {
		t.Errorf("FileKey = %q", got)
	}
	// 2026-11-10 07:59 in UTC+8 is still 2026-11-09 in UTC.
	cst := time.FixedZone("CST", 8*3600)
	local := time.Date(2026, 11, 10, 7, 59, 0, 0, cst)
	if got := RawKey(local, "S", 12); got != "raw/2026/11/09/S/000012.seg.zst" {
		t.Errorf("RawKey(local) = %q", got)
	}
}

func TestBatchEmpty(t *testing.T) {
	if !(Batch{}).Empty() {
		t.Fatal("zero batch must be empty")
	}
	if (Batch{Alerts: []AlertRow{{Kind: "gap"}}}).Empty() {
		t.Fatal("batch with an alert is not empty")
	}
}

// A ClickHouse address the client will never take is said when the
// gateway starts, not at its first insert (an https address needs
// secure=true).
func TestCheckCHURL(t *testing.T) {
	for url, ok := range map[string]bool{
		"clickhouse://ch.example.org:9000":       true,
		"https://ch.example.org:443?secure=true": true,
		"http://ch.example.org:8123":             true,
		"https://ch.example.org:443":             false,
		"clickhouse://":                          false,
	} {
		if err := CheckCHURL(url); (err == nil) != ok {
			t.Errorf("CheckCHURL(%q) = %v, want ok=%v", url, err, ok)
		}
	}
}
