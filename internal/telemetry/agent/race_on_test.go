// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build race

package agent

// raceEnabled: the race detector inflates memory; heap-size tests skip.
const raceEnabled = true
