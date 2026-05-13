// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"testing"
	"time"
)

func TestRoundUpSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},
		{1 * time.Millisecond, 1},
		{500 * time.Millisecond, 1},
		{1 * time.Second, 1},
		{1001 * time.Millisecond, 2},
		{1500 * time.Millisecond, 2},
		{5 * time.Second, 5},
		{5500 * time.Millisecond, 6},
		{30 * time.Second, 30},
	}
	for _, tc := range cases {
		if got := roundUpSeconds(tc.in); got != tc.want {
			t.Errorf("roundUpSeconds(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
