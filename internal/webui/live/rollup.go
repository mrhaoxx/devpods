// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"fmt"
	"sort"
	"time"
)

// Thresholds for the "needs attention" list (UI spec §5.3).
const (
	PendingLong  = 10 * time.Minute
	QuotaHigh    = 0.9
	RecentAlerts = time.Hour
)

var attentionOrder = map[string]int{"alert": 0, "idle_gpu": 1, "pending_long": 2, "quota_high": 3}

// Snapshot renders the state against the cluster's objects.
func (s *State) Snapshot(in Inputs) Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	snap := Snapshot{TS: now.UTC(), DevPods: map[string]DevPodLive{}, Nodes: []NodeLive{}, Users: []UserLive{}, Attention: []AttentionItem{}}

	nodes := map[string]*NodeLive{}
	for _, n := range in.Nodes {
		nl := &NodeLive{Name: n.Name, Ready: n.Ready, Labels: n.Labels, CPUAllocatable: n.CPUAllocatable,
			MemAllocatable: n.MemAllocatable, GPUTotal: n.GPUAllocatable, GPUModel: n.GPUModel}
		nodes[n.Name] = nl
		if n.Ready {
			snap.KPI.CPUAllocatable += n.CPUAllocatable
			snap.KPI.MemAllocatable += n.MemAllocatable
			snap.KPI.GPUTotal += n.GPUAllocatable
		}
	}
	users := map[string]*UserLive{}
	requested := map[string]Quota{} // owner → limits of running DevPods (quota is limit-based)
	sessionUsers := map[string]bool{}

	for _, ref := range in.DevPods {
		d := s.devpodLive(ref, now)
		if len(d.Sessions) == 0 {
			for _, u := range in.Presence[ref.UID] {
				d.Sessions = append(d.Sessions, SessionLive{User: u})
			}
		}
		snap.DevPods[ref.UID] = d
		running := ref.Phase == "Running"
		switch ref.Phase {
		case "Running":
			snap.KPI.Running++
		case "Pending":
			snap.KPI.Pending++
		case "Failed":
			snap.KPI.Failed++
		default:
			snap.KPI.Stopped++
		}
		snap.KPI.Sessions += len(d.Sessions)
		for _, se := range d.Sessions {
			sessionUsers[se.User] = true
		}
		u := users[ref.Owner]
		if u == nil {
			u = &UserLive{User: ref.Owner}
			users[ref.Owner] = u
		}
		u.DevPods++
		if d.CPU != nil {
			u.CPU += *d.CPU
			snap.KPI.CPUUsed += *d.CPU
		}
		if d.Mem != nil {
			u.Mem += *d.Mem
			snap.KPI.MemUsed += *d.Mem
		}
		if d.RDMARx != nil {
			snap.KPI.RDMA += *d.RDMARx + *d.RDMATx
		}
		if running {
			u.Running++
			u.GPU += ref.GPURequest
			snap.KPI.GPUUsed += ref.GPURequest
			q := requested[ref.Owner]
			q.CPU, q.Mem, q.GPU = q.CPU+ref.CPULimit, q.Mem+ref.MemLimit, q.GPU+ref.GPURequest
			requested[ref.Owner] = q
		}
		if n := nodes[ref.Node]; n != nil && ref.Node != "" {
			n.DevPods++
			if running {
				n.CPUAllocated += ref.CPURequest
				n.MemAllocated += ref.MemRequest
				n.GPUUsed += ref.GPURequest
			}
			if d.CPU != nil {
				n.CPUUsed += *d.CPU
			}
			if d.Mem != nil {
				n.MemUsed += *d.Mem
			}
			for _, g := range d.GPU {
				n.Cards = append(n.Cards, CardLive{Index: g.Index, UUID: g.UUID, DevPod: ref.Name, Owner: ref.Owner, Util: g.Util})
			}
		}

		// needs attention
		switch {
		case running && ref.GPURequest > 0 && in.Idle[ref.UID]:
			snap.Attention = append(snap.Attention, AttentionItem{Kind: "idle_gpu", UID: ref.UID, DevPod: ref.Name, Owner: ref.Owner,
				Detail: fmt.Sprintf("holds %d GPU, idle for 24h", ref.GPURequest)})
		case ref.Phase == "Pending" && !ref.PhaseSince.IsZero() && now.Sub(ref.PhaseSince) > PendingLong:
			snap.Attention = append(snap.Attention, AttentionItem{Kind: "pending_long", UID: ref.UID, DevPod: ref.Name, Owner: ref.Owner,
				Since: ref.PhaseSince, Detail: fmt.Sprintf("pending for %s", now.Sub(ref.PhaseSince).Truncate(time.Minute))})
		}
	}
	snap.KPI.SessionUsers = len(sessionUsers)

	for owner, used := range requested {
		q, ok := in.Quotas[owner]
		if !ok {
			continue
		}
		over := ""
		switch {
		case q.CPU > 0 && used.CPU >= QuotaHigh*q.CPU:
			over = fmt.Sprintf("CPU %.1f / %.1f", used.CPU, q.CPU)
		case q.Mem > 0 && used.Mem >= QuotaHigh*q.Mem:
			over = fmt.Sprintf("memory %.0f%%", 100*used.Mem/q.Mem)
		case q.GPU > 0 && float64(used.GPU) >= QuotaHigh*float64(q.GPU):
			over = fmt.Sprintf("GPU %d / %d", used.GPU, q.GPU)
		}
		if over != "" {
			snap.Attention = append(snap.Attention, AttentionItem{Kind: "quota_high", Owner: owner, Detail: over})
		}
	}
	names := map[string]DevPodRef{}
	for _, ref := range in.DevPods {
		names[ref.UID] = ref
	}
	for _, a := range in.Alerts {
		if now.Sub(a.TS) > RecentAlerts {
			continue
		}
		ref := names[a.DevPodUID]
		snap.Attention = append(snap.Attention, AttentionItem{Kind: "alert", UID: a.DevPodUID, DevPod: ref.Name, Owner: ref.Owner,
			Since: a.TS, Detail: a.Kind + ": " + a.Detail})
	}
	sort.SliceStable(snap.Attention, func(i, j int) bool {
		ai, aj := snap.Attention[i], snap.Attention[j]
		if attentionOrder[ai.Kind] != attentionOrder[aj.Kind] {
			return attentionOrder[ai.Kind] < attentionOrder[aj.Kind]
		}
		return ai.Since.Before(aj.Since)
	})

	for _, n := range nodes {
		sort.Slice(n.Cards, func(i, j int) bool { return n.Cards[i].Index < n.Cards[j].Index })
		snap.Nodes = append(snap.Nodes, *n)
	}
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })
	for _, u := range users {
		snap.Users = append(snap.Users, *u)
	}
	sort.Slice(snap.Users, func(i, j int) bool {
		if snap.Users[i].CPU != snap.Users[j].CPU {
			return snap.Users[i].CPU > snap.Users[j].CPU
		}
		return snap.Users[i].User < snap.Users[j].User
	})
	return snap
}
