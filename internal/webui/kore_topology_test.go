// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"net/http"
	"testing"

	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestKoreTopologyGating(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	admin := forge(sm, "gl-root", true)
	alice := forge(sm, "gl-alice", false)

	t.Run("kore off → 404", func(t *testing.T) {
		s.KoreEnabled = false
		defer func() { s.KoreEnabled = true }()
		rec := doJSON(t, s.HandleKoreTopologyForTest(), "GET", "/api/kore/topology", nil, admin, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("non-admin → 403", func(t *testing.T) {
		// KoreEnabled true (default from newServer); the CRD isn't in
		// envtest so an admin call would 500 on List — but the non-admin
		// gate runs before the List, so this asserts 403 cleanly.
		rec := doJSON(t, s.HandleKoreTopologyForTest(), "GET", "/api/kore/topology", nil, alice, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

// The transform is the interesting logic; exercise it on a synthetic CR
// so no live Kore is needed.
func TestKoreTopologyTransform(t *testing.T) {
	item := map[string]any{
		"apiVersion": "kore.zjusct.io/v1alpha1",
		"kind":       "KoreNodeTopology",
		"metadata":   map[string]any{"name": "node-a"},
		"status": map[string]any{
			"reservedSystemCpus": "0-1",
			"zones": []any{
				map[string]any{
					"id":          float64(0),
					"cpus":        "0-7",
					"freeCpus":    "4-7",
					"smtSiblings": []any{[]any{float64(0), float64(4)}, []any{float64(1), float64(5)}},
					"memoryTotal": "128Gi",
				},
			},
			"allocations": []any{
				map[string]any{"pod": "star-a100", "container": "dev", "cpuset": "2-3"},
			},
			"pools": []any{
				map[string]any{"name": "team-hpl", "cpuset": "8-15", "numa": []any{float64(0)}, "members": []any{"h32-1", "h32-2"}},
			},
		},
	}
	nodes := webui.KoreTransformForTest([]map[string]any{item})
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(nodes))
	}
	n := nodes[0]
	if n.Node != "node-a" || n.ReservedCpus != "0-1" {
		t.Fatalf("node = %+v", n)
	}
	if len(n.Zones) != 1 || n.Zones[0].Cpus != "0-7" || n.Zones[0].Memory != "128Gi" {
		t.Fatalf("zones = %+v", n.Zones)
	}
	if len(n.Zones[0].SMTSiblings) != 2 || n.Zones[0].SMTSiblings[0][1] != 4 {
		t.Fatalf("smt = %+v", n.Zones[0].SMTSiblings)
	}
	if len(n.Allocations) != 1 || n.Allocations[0].Pod != "star-a100" || n.Allocations[0].Cpuset != "2-3" {
		t.Fatalf("alloc = %+v", n.Allocations)
	}
	if len(n.Pools) != 1 || n.Pools[0].Name != "team-hpl" || len(n.Pools[0].Members) != 2 {
		t.Fatalf("pools = %+v", n.Pools)
	}
}
