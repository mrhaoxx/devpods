// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

// A job's ranks and how many go on each machine, in each of its shapes —
// an old job (nodes alone) keeping one rank on each of its machines.
func TestJobSpecRanks(t *testing.T) {
	for _, c := range []struct {
		s          JobSpec
		ranks, per int32
	}{
		{JobSpec{Nodes: 2}, 2, 1},                            // an old job
		{JobSpec{Nodes: 2, TasksPerNode: 3, Tasks: 6}, 6, 3}, // -N 2 --ntasks-per-node 3
		{JobSpec{Tasks: 5}, 5, 0},                            // -n 5, packed
		{JobSpec{Tasks: 5, TasksPerNode: 2}, 5, 2},           // -n 5 --ntasks-per-node 2
	} {
		if c.s.Ranks() != c.ranks || c.s.PerMachine() != c.per {
			t.Errorf("%+v: ranks %d, per machine %d", c.s, c.s.Ranks(), c.s.PerMachine())
		}
	}
}
