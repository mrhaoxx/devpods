// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/mrhaoxx/devpod/internal/webui"
)

// reservationFlags: node reservations (reservation spec §3.1). The
// controller takes the same rule flags.
type reservationFlags struct {
	enabled                                       bool
	granularity, maxDuration, maxAdvance, clearBy time.Duration
}

func addReservationFlags(fs *flag.FlagSet) *reservationFlags {
	f := &reservationFlags{}
	fs.BoolVar(&f.enabled, "reservations", false, "enable node reservations (needs the Reservation CRD and the controller's reservation reconciler)")
	fs.DurationVar(&f.granularity, "reservation-granularity", 30*time.Minute, "reservation starts and ends align to this")
	fs.DurationVar(&f.maxDuration, "reservation-max-duration", 24*time.Hour, "longest single reservation; 0 = unlimited")
	fs.DurationVar(&f.maxAdvance, "reservation-max-advance", 336*time.Hour, "how far ahead a reservation may start; 0 = unlimited")
	fs.DurationVar(&f.clearBy, "reservation-clear-notice", 30*time.Minute, "how long before a start the node's other DevPods are warned")
	return f
}

// build returns nil when reservations are off.
func (f *reservationFlags) build() (*webui.ReservationRules, error) {
	if !f.enabled {
		return nil, nil
	}
	if f.granularity < time.Second || f.granularity%time.Second != 0 || f.maxDuration < 0 || f.maxAdvance < 0 {
		return nil, fmt.Errorf("invalid reservation rules: granularity %s, max duration %s, max advance %s", f.granularity, f.maxDuration, f.maxAdvance)
	}
	return &webui.ReservationRules{Granularity: f.granularity, MaxDuration: f.maxDuration, MaxAdvance: f.maxAdvance, ClearNotice: f.clearBy}, nil
}
