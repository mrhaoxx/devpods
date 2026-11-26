// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"maps"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// The scheduler (design 2026-11-26), on whichever replica leads: every
// Tick, for each cluster that takes jobs, it follows the jobs running
// there by their rank DevPods (found by label: nothing it made is
// forgotten across restarts), ends those that ended, plans the queue and
// carries the plan out. What it knows of running jobs is their Job
// objects, read afresh each round; ranks are made under fixed names, so
// a rank made twice is found, not doubled.

// jobRankGrace: how long a starting job's rank may be missing (not yet
// seen) before it counts as stopped.
const jobRankGrace = time.Minute

// RunQueue schedules every Tick while leads says this replica leads (nil:
// it always does), until ctx ends. Without the queue it returns at once.
func (s *Server) RunQueue(ctx context.Context, leads func() bool) {
	if s.Queue == nil {
		return
	}
	t := time.NewTicker(s.Queue.Tick)
	defer t.Stop()
	for {
		if leads == nil || leads() {
			s.QueueRound(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// QueueRound is one round over every cluster that takes jobs; ended jobs
// kept longer than KeepEnded go.
func (s *Server) QueueRound(ctx context.Context) {
	if s.Queue == nil {
		return
	}
	now := s.Queue.now()
	var all devpodv1alpha1.JobList
	if err := s.Reader.List(ctx, &all); err != nil {
		slog.Warn("queue", "err", err)
		return
	}
	byCluster := map[string][]*devpodv1alpha1.Job{}
	for i := range all.Items {
		j := &all.Items[i]
		if j.DeletionTimestamp != nil {
			continue
		}
		if jobEnded(j.Status.Phase) {
			if j.Status.FinishedAt != nil && now.Sub(j.Status.FinishedAt.Time) > s.Queue.KeepEnded {
				if err := s.Client.Delete(ctx, j); err != nil && !apierrors.IsNotFound(err) {
					slog.Warn("queue_prune", "job", j.Name, "err", err)
				}
			}
			continue
		}
		byCluster[j.Spec.Cluster] = append(byCluster[j.Spec.Cluster], j)
	}
	for cluster, jobs := range byCluster {
		if s.takesJobs(cluster) {
			s.queueIn(ctx, cluster, jobs, now)
		}
	}
}

// queueIn is one round in cluster (away: skipped — its jobs keep as they
// are, and are judged when it is back).
func (s *Server) queueIn(ctx context.Context, cluster string, jobs []*devpodv1alpha1.Job, now time.Time) {
	cc := s.clientsOf(cluster)
	if cc == nil {
		return
	}
	ctx = s.in(ctx, cc)
	ranksOf, err := s.rankDevPods(ctx, cc)
	if err != nil {
		slog.Warn("queue", "cluster", cluster, "err", err)
		return
	}
	var active, queued []*devpodv1alpha1.Job
	for _, j := range jobs {
		switch j.Status.Phase {
		case devpodv1alpha1.JobStarting, devpodv1alpha1.JobRunning:
			s.followJob(ctx, cc, j, ranksOf[j.Name], now)
			if j.Status.Phase == devpodv1alpha1.JobStarting || j.Status.Phase == devpodv1alpha1.JobRunning {
				active = append(active, j)
			}
		default:
			if j.Spec.Cancelled != nil {
				s.endJob(ctx, cc, j, devpodv1alpha1.JobCancelled, "cancelled", nil, now)
				continue
			}
			queued = append(queued, j)
		}
	}
	if len(queued) == 0 {
		return
	}
	v, nodes, err := s.queueViewFor(ctx, cc, active, queued, now)
	if err != nil {
		slog.Warn("queue", "cluster", cluster, "err", err)
		return
	}
	plan := planQueue(v)
	for _, victim := range plan.Preempt {
		s.requeueJob(ctx, cc, victim, "preempted")
	}
	for _, st := range plan.Starts {
		s.startJob(ctx, cc, st.Job, st.Machines, nodes, now)
	}
	for _, j := range queued {
		w, ok := plan.Waits[j.Name]
		if !ok {
			continue
		}
		var exp *metav1.Time
		if w.Expected != nil {
			t := metav1.NewTime(*w.Expected)
			exp = &t
		}
		if j.Status.Phase == devpodv1alpha1.JobQueued && j.Status.Position == w.Position && j.Status.Reason == w.Reason &&
			maps.Equal(j.Status.Detail, w.Detail) && reflect.DeepEqual(timeOf(j.Status.ExpectedStart), timeOf(exp)) {
			continue
		}
		j.Status.Phase, j.Status.Position, j.Status.Reason, j.Status.Detail, j.Status.ExpectedStart = devpodv1alpha1.JobQueued, w.Position, w.Reason, w.Detail, exp
		s.writeJobStatus(ctx, j)
	}
}

// rankDevPods: cluster's job ranks, by job and rank.
func (s *Server) rankDevPods(ctx context.Context, cc *clusters.ClusterClients) (map[string]map[int32]*devpodv1alpha1.DevPod, error) {
	var l devpodv1alpha1.DevPodList
	if err := cc.Client.List(ctx, &l, client.InNamespace(s.NS), client.HasLabels{"devpod.io/job"}); err != nil {
		return nil, err
	}
	out := map[string]map[int32]*devpodv1alpha1.DevPod{}
	for i := range l.Items {
		d := &l.Items[i]
		if d.Spec.Job == nil || d.DeletionTimestamp != nil {
			continue
		}
		if out[d.Spec.Job.Name] == nil {
			out[d.Spec.Job.Name] = map[int32]*devpodv1alpha1.DevPod{}
		}
		out[d.Spec.Job.Name][d.Spec.Job.Rank] = d
	}
	return out, nil
}

// followJob brings a starting or running job up to date with its ranks:
// all done — Succeeded; one failed — Failed; one stopped or gone —
// Failed; all running — Running; ranks not all running within the gang
// timeout — queued again; out of time — TimedOut; asked to — Cancelled.
func (s *Server) followJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, ranks map[int32]*devpodv1alpha1.DevPod, now time.Time) {
	if j.Spec.Cancelled != nil {
		s.endJob(ctx, cc, j, devpodv1alpha1.JobCancelled, "cancelled", nil, now)
		return
	}
	n := j.Spec.Nodes
	graceOver := j.Status.Phase == devpodv1alpha1.JobRunning || j.Status.AttemptAt == nil || now.Sub(j.Status.AttemptAt.Time) > jobRankGrace
	status := make([]devpodv1alpha1.JobRank, n)
	for _, r := range j.Status.Ranks {
		if r.Rank >= 0 && r.Rank < n {
			status[r.Rank] = r
		}
	}
	allStarted, allDone := true, true
	var failed *devpodv1alpha1.JobRank
	for r := int32(0); r < n; r++ {
		rk := &status[r]
		rk.Rank = r
		d := ranks[r]
		if d == nil || !d.Spec.Running {
			if graceOver {
				s.endJob(ctx, cc, j, devpodv1alpha1.JobFailed, "rank-stopped", map[string]string{"rank": itoa(r)}, now)
				return
			}
			allStarted, allDone = false, false
			continue
		}
		rk.DevPod, rk.Phase, rk.ExitCode = d.Name, string(d.Status.Phase), d.Status.ExitCode
		if node := pinnedNode(d); node != "" {
			rk.Node = node
		}
		switch d.Status.Phase {
		case devpodv1alpha1.DevPodFailed:
			if failed == nil {
				failed = rk
			}
		case devpodv1alpha1.DevPodSucceeded, devpodv1alpha1.DevPodRunning:
		default:
			allStarted = false
		}
		if d.Status.Phase != devpodv1alpha1.DevPodSucceeded {
			allDone = false
		}
	}
	changed := !reflect.DeepEqual(j.Status.Ranks, status)
	j.Status.Ranks = status
	limit := j.Spec.TimeLimit.Duration
	switch {
	case failed != nil:
		detail := map[string]string{"rank": itoa(failed.Rank)}
		if failed.ExitCode != nil {
			detail["exit"] = itoa(*failed.ExitCode)
		}
		s.endJob(ctx, cc, j, devpodv1alpha1.JobFailed, "rank-failed", detail, now)
	case allDone:
		s.endJob(ctx, cc, j, devpodv1alpha1.JobSucceeded, "", nil, now)
	case j.Status.Phase == devpodv1alpha1.JobStarting && allStarted:
		t := metav1.NewTime(now)
		j.Status.Phase, j.Status.StartedAt = devpodv1alpha1.JobRunning, &t
		s.writeJobStatus(ctx, j)
	case j.Status.Phase == devpodv1alpha1.JobStarting && j.Status.AttemptAt != nil && now.Sub(j.Status.AttemptAt.Time) > s.Queue.GangTimeout:
		s.requeueJob(ctx, cc, j, "gang-timeout")
	case j.Status.Phase == devpodv1alpha1.JobRunning && j.Status.StartedAt != nil && !now.Before(j.Status.StartedAt.Add(limit)):
		s.endJob(ctx, cc, j, devpodv1alpha1.JobTimedOut, "time-limit", nil, now)
	case changed:
		s.writeJobStatus(ctx, j)
	}
}

// deleteRanks deletes every rank DevPod of j; false when one could not be.
func (s *Server) deleteRanks(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job) bool {
	for r := int32(0); r < j.Spec.Nodes; r++ {
		dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: rankName(j.Spec.Owner, j.Name, r), Namespace: s.NS}}
		if err := cc.Client.Delete(ctx, dp); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("queue_delete_rank", "job", j.Name, "devpod", dp.Name, "err", err)
			return false
		}
	}
	return true
}

// endJob ends j as phase (reason, detail): its ranks' logs kept, its ranks
// deleted (a rank still running is told to stop and has its grace).
func (s *Server) endJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, phase devpodv1alpha1.JobPhase, reason string, detail map[string]string, now time.Time) {
	s.saveRankLogs(ctx, cc, j)
	if !s.deleteRanks(ctx, cc, j) {
		return // the next round tries again
	}
	t := metav1.NewTime(now)
	j.Status.Phase, j.Status.Reason, j.Status.Detail, j.Status.FinishedAt = phase, reason, detail, &t
	j.Status.Position, j.Status.ExpectedStart = 0, nil
	s.writeJobStatus(ctx, j)
	slog.Info("queue_job_end", "job", j.Name, "owner", j.Spec.Owner, "phase", phase, "reason", reason)
}

// requeueJob stops j's ranks and puts it back in the queue, keeping its
// place (its submission time).
func (s *Server) requeueJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, reason string) {
	if !s.deleteRanks(ctx, cc, j) {
		return
	}
	j.Status.Phase, j.Status.Reason, j.Status.Detail = devpodv1alpha1.JobQueued, reason, nil
	j.Status.Ranks, j.Status.AttemptAt, j.Status.StartedAt = nil, nil, nil
	if reason == "preempted" {
		j.Status.Preemptions++
	}
	s.writeJobStatus(ctx, j)
	slog.Info("queue_job_requeued", "job", j.Name, "owner", j.Spec.Owner, "reason", reason)
}

// startJob makes j's ranks on machines and marks it Starting. A rank
// found made already (a round that did not get to say so) is taken as
// it is; any other failure takes back what was made.
func (s *Server) startJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, machines []string, nodes map[string]*corev1.Node, now time.Time) {
	rank0 := rankName(j.Spec.Owner, j.Name, 0)
	master := masterAddr(j, rank0, s.NS, j.Spec.Template.Spec.HostNetwork, internalIP(nodes[machines[0]]))
	ranks := make([]devpodv1alpha1.JobRank, 0, len(machines))
	for r, m := range machines {
		dp := rankDevPod(j, int32(r), m, s.NS, master)
		if err := cc.Client.Create(ctx, dp); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				slog.Warn("queue_start", "job", j.Name, "rank", r, "err", err)
				s.deleteRanks(ctx, cc, j)
				return
			}
			var cur devpodv1alpha1.DevPod
			if cc.Client.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: dp.Name}, &cur) == nil && pinnedNode(&cur) != "" {
				m = pinnedNode(&cur)
			}
		}
		ranks = append(ranks, devpodv1alpha1.JobRank{Rank: int32(r), Node: m, DevPod: dp.Name})
	}
	t := metav1.NewTime(now)
	j.Status.Phase, j.Status.AttemptAt, j.Status.Ranks = devpodv1alpha1.JobStarting, &t, ranks
	j.Status.Attempts++
	j.Status.Reason, j.Status.Detail, j.Status.Position, j.Status.ExpectedStart, j.Status.StartedAt = "", nil, 0, nil, nil
	s.writeJobStatus(ctx, j)
	slog.Info("queue_job_start", "job", j.Name, "owner", j.Spec.Owner, "machines", machines)
}

// writeJobStatus writes j's status (the scheduler's alone), retried on a
// conflict with a write to its spec (a cancel).
func (s *Server) writeJobStatus(ctx context.Context, j *devpodv1alpha1.Job) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur devpodv1alpha1.Job
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: j.Name}, &cur); err != nil {
			return err
		}
		cur.Status = j.Status
		return s.Client.Status().Update(ctx, &cur)
	})
	if err != nil {
		slog.Warn("queue_status", "job", j.Name, "err", err)
	}
}

// internalIP: a machine's InternalIP ("" unknown).
func internalIP(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// queueViewFor: what the planner plans cluster's queue from — its
// reservable, ready machines with their reservations and what
// interactive DevPods take outside reservations, the jobs running, the
// jobs waiting; who may run where (the access rules, with the jobs'
// privileges) and whose quota has room. Also the machines by name.
func (s *Server) queueViewFor(ctx context.Context, cc *clusters.ClusterClients, active, queued []*devpodv1alpha1.Job, now time.Time) (queueView, map[string]*corev1.Node, error) {
	var nodes corev1.NodeList
	if err := cc.Client.List(ctx, &nodes); err != nil {
		return queueView{}, nil, err
	}
	all, err := s.nodeReservations(ctx)
	if err != nil {
		return queueView{}, nil, err
	}
	on, _, err := s.devPodsOn(ctx, cc)
	if err != nil {
		return queueView{}, nil, err
	}
	cluster := s.resCluster(ctx)
	byName := map[string]*corev1.Node{}
	var machines []qMachine
	for i := range nodes.Items {
		n := &nodes.Items[i]
		byName[n.Name] = n
		if n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" || !readyOrUnknown(n) {
			continue
		}
		var rs []*devpodv1alpha1.NodeReservation
		for k := range all {
			if all[k].Spec.Cluster == cluster && all[k].Spec.Node == n.Name {
				rs = append(rs, &all[k])
			}
		}
		var interactive []placed
		for _, p := range on[n.Name] {
			if p.DP.Spec.Job == nil {
				interactive = append(interactive, p)
			}
		}
		c := capacityOf(n)
		machines = append(machines, qMachine{Name: n.Name, Cap: c, Interactive: loadAt(c, rs, interactive, now, "").outside(), Rs: rs})
	}
	var running []qRun
	for _, j := range active {
		var ms []string
		for _, r := range j.Status.Ranks {
			if r.Node != "" {
				ms = append(ms, r.Node)
			}
		}
		since := now
		switch {
		case j.Status.StartedAt != nil:
			since = j.Status.StartedAt.Time
		case j.Status.AttemptAt != nil:
			since = j.Status.AttemptAt.Time
		}
		running = append(running, qRun{Job: j, Machines: ms, Ask: jobAsk(j), Until: since.Add(j.Spec.TimeLimit.Duration),
			Preemptible: j.Spec.Preemptible, Priority: j.Spec.Priority, Started: since})
	}
	allowed := map[string]bool{}
	mayRun := func(j *devpodv1alpha1.Job, machine string) bool {
		key := j.Name + "/" + machine
		if v, seen := allowed[key]; seen {
			return v
		}
		n := byName[machine]
		ok := false
		if n != nil {
			d, admin, err := s.decide(ctx, j.Spec.Owner, cluster, access.Node{Name: n.Name, Labels: n.Labels})
			ok = err == nil && (admin || (d.Allow && d.Admits(access.NeedsOf(&j.Spec.Template.Spec)) == nil))
		}
		allowed[key] = ok
		return ok
	}
	var dps devpodv1alpha1.DevPodList
	if err := cc.Client.List(ctx, &dps, client.InNamespace(s.NS)); err != nil {
		return queueView{}, nil, err
	}
	quotaOK := func(j *devpodv1alpha1.Job) bool {
		if s.ownerIsAdmin(ctx, j.Spec.Owner) {
			return true
		}
		var u devpodv1alpha1.User
		var up *devpodv1alpha1.User
		if s.Client.Get(ctx, types.NamespacedName{Name: j.Spec.Owner}, &u) == nil {
			up = &u
		}
		var existing []devpodv1alpha1.DevPod
		for _, d := range dps.Items {
			if d.Spec.Owner == j.Spec.Owner && d.DeletionTimestamp == nil {
				existing = append(existing, d)
			}
		}
		for r := int32(0); r < j.Spec.Nodes-1; r++ {
			existing = append(existing, *rankDevPod(j, r, "x", s.NS, ""))
		}
		return CheckQuota(EffectiveQuota(up, cc.Name, s.clusterQuota(ctx, cc)), existing, rankDevPod(j, j.Spec.Nodes-1, "x", s.NS, "")) == nil
	}
	return queueView{Now: now, Machines: machines, Running: running, Queued: queued, Allowed: mayRun, QuotaOK: quotaOK, Horizon: 14 * 24 * time.Hour}, byName, nil
}

// saveRankLogs keeps j's ranks' output (queue_logs.go).
func (s *Server) saveRankLogs(_ context.Context, _ *clusters.ClusterClients, _ *devpodv1alpha1.Job) {}
