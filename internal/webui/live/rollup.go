// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Thresholds for the "needs attention" list (UI spec §5.3).
const (
	PendingLong  = 10 * time.Minute
	QuotaHigh    = 0.9
	RecentAlerts = time.Hour
)

var attentionOrder = map[string]int{"alert": 0, "idle_gpu": 1, "pending_long": 2, "quota_high": 3}

// nodeKey: a node in its cluster (a name can be in two clusters).
func nodeKey(cluster, name string) string { return cluster + "/" + name }

// tally is the numbers kept for the console and for each cluster.
type tally struct {
	kpi          KPI
	users        map[string]*UserLive
	sessionUsers map[string]bool
}

func newTally() *tally { return &tally{users: map[string]*UserLive{}, sessionUsers: map[string]bool{}} }

func (t *tally) node(n NodeRef) {
	if n.Ready {
		t.kpi.CPUAllocatable += n.CPUAllocatable
		t.kpi.MemAllocatable += n.MemAllocatable
		t.kpi.GPUTotal += n.GPUAllocatable
	}
}

func (t *tally) devpod(ref DevPodRef, d DevPodLive) {
	running := ref.Phase == "Running"
	switch ref.Phase {
	case "Running":
		t.kpi.Running++
	case "Pending":
		t.kpi.Pending++
	case "Failed":
		t.kpi.Failed++
	default:
		t.kpi.Stopped++
	}
	t.kpi.Sessions += len(d.Sessions)
	for _, se := range d.Sessions {
		t.sessionUsers[se.User] = true
	}
	u := t.users[ref.Owner]
	if u == nil {
		u = &UserLive{User: ref.Owner}
		t.users[ref.Owner] = u
	}
	u.DevPods++
	if d.CPU != nil {
		u.CPU += *d.CPU
		t.kpi.CPUUsed += *d.CPU
	}
	if d.Mem != nil {
		u.Mem += *d.Mem
		t.kpi.MemUsed += *d.Mem
	}
	if d.RDMARx != nil {
		t.kpi.RDMA += *d.RDMARx + *d.RDMATx
	}
	if running {
		u.Running++
		u.GPU += ref.GPURequest
		t.kpi.GPUUsed += ref.GPURequest
	}
}

// done: the KPI and the users, the busiest first.
func (t *tally) done() (KPI, []UserLive) {
	t.kpi.SessionUsers = len(t.sessionUsers)
	users := make([]UserLive, 0, len(t.users))
	for _, u := range t.users {
		users = append(users, *u)
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].CPU != users[j].CPU {
			return users[i].CPU > users[j].CPU
		}
		return users[i].User < users[j].User
	})
	return t.kpi, users
}

// Snapshot renders the state against the clusters' objects: the
// console's numbers, and in a console of several each cluster's too.
func (s *State) Snapshot(in Inputs) Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	snap := Snapshot{TS: now.UTC(), DevPods: map[string]DevPodLive{}, Nodes: []NodeLive{}, Users: []UserLive{}, Attention: []AttentionItem{}}

	all := newTally()
	parts := map[string]*tally{} // by cluster, in a console of several
	partOf := func(cluster string) *tally {
		if cluster == "" {
			return nil
		}
		if parts[cluster] == nil {
			parts[cluster] = newTally()
		}
		return parts[cluster]
	}
	nodes := map[string]*NodeLive{}
	for _, n := range in.Nodes {
		nl := &NodeLive{Cluster: n.Cluster, Name: n.Name, Ready: n.Ready, Labels: n.Labels, CPUAllocatable: n.CPUAllocatable,
			MemAllocatable: n.MemAllocatable, GPUTotal: n.GPUAllocatable, GPUModel: n.GPUModel,
			CPUAllocated: n.OtherCPU, MemAllocated: n.OtherMem, GPUUsed: n.OtherGPU}
		nodes[nodeKey(n.Cluster, n.Name)] = nl
		all.node(n)
		if p := partOf(n.Cluster); p != nil {
			p.node(n)
		}
	}
	requested := map[string]Quota{}  // QuotaKey(cluster, owner) → limits of running DevPods (quota is limit-based)
	clusterOf := map[string]string{} // QuotaKey → cluster

	for _, ref := range in.DevPods {
		d := s.devpodLive(ref, now)
		if len(d.Sessions) == 0 {
			for _, u := range in.Presence[ref.UID] {
				d.Sessions = append(d.Sessions, SessionLive{User: u})
			}
		}
		d.Idle = in.Idle[ref.UID]
		snap.DevPods[ref.UID] = d
		running := ref.Phase == "Running"
		all.devpod(ref, d)
		if p := partOf(ref.Cluster); p != nil {
			p.devpod(ref, d)
		}
		if running {
			k := QuotaKey(ref.Cluster, ref.Owner)
			q := requested[k]
			q.CPU, q.Mem, q.GPU = q.CPU+ref.CPULimit, q.Mem+ref.MemLimit, q.GPU+ref.GPURequest
			requested[k] = q
			clusterOf[k] = ref.Cluster
		}
		if n := nodes[nodeKey(ref.Cluster, ref.Node)]; n != nil && ref.Node != "" {
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
			snap.Attention = append(snap.Attention, AttentionItem{Cluster: ref.Cluster, Kind: "idle_gpu", UID: ref.UID, DevPod: ref.Name, Owner: ref.Owner,
				Detail: fmt.Sprintf("holds %d GPU, idle for 24h", ref.GPURequest)})
		case ref.Phase == "Pending" && !ref.PhaseSince.IsZero() && now.Sub(ref.PhaseSince) > PendingLong:
			snap.Attention = append(snap.Attention, AttentionItem{Cluster: ref.Cluster, Kind: "pending_long", UID: ref.UID, DevPod: ref.Name, Owner: ref.Owner,
				Since: ref.PhaseSince, Detail: fmt.Sprintf("pending for %s", now.Sub(ref.PhaseSince).Truncate(time.Minute))})
		}
	}
	for key, used := range requested {
		q, ok := in.Quotas[key]
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
			owner := strings.TrimPrefix(key, clusterOf[key]+"/")
			snap.Attention = append(snap.Attention, AttentionItem{Cluster: clusterOf[key], Kind: "quota_high", Owner: owner, Detail: over})
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
		snap.Attention = append(snap.Attention, AttentionItem{Cluster: ref.Cluster, Kind: "alert", UID: a.DevPodUID, DevPod: ref.Name, Owner: ref.Owner,
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
	sort.Slice(snap.Nodes, func(i, j int) bool {
		if snap.Nodes[i].Cluster != snap.Nodes[j].Cluster {
			return snap.Nodes[i].Cluster < snap.Nodes[j].Cluster
		}
		return snap.Nodes[i].Name < snap.Nodes[j].Name
	})
	snap.KPI, snap.Users = all.done()
	if len(parts) > 0 {
		snap.Clusters = map[string]ClusterLive{}
		for c, p := range parts {
			kpi, users := p.done()
			snap.Clusters[c] = ClusterLive{KPI: kpi, Users: users}
		}
	}
	return snap
}
