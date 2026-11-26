// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/render"
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

// maxGangTimeouts: how many times a job's ranks may fail to all start in
// time before the job fails.
const maxGangTimeouts = 3

// jobCleanupTimeout bounds keeping an ended job's output and deleting
// its ranks (in the background, off the round).
const jobCleanupTimeout = 10 * time.Minute

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
	endedIn := map[string][]*devpodv1alpha1.Job{}
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
				continue
			}
			endedIn[j.Spec.Cluster] = append(endedIn[j.Spec.Cluster], j)
			continue
		}
		byCluster[j.Spec.Cluster] = append(byCluster[j.Spec.Cluster], j)
	}
	for cluster := range endedIn {
		if _, ok := byCluster[cluster]; !ok {
			byCluster[cluster] = nil
		}
	}
	for cluster, jobs := range byCluster {
		if s.takesJobs(cluster) {
			s.queueIn(ctx, cluster, jobs, endedIn[cluster], now)
		}
	}
}

// queueIn is one round in cluster (away: skipped — its jobs keep as they
// are, and are judged when it is back). ended are its jobs that have
// ended: ranks of theirs still there are cleaned up.
func (s *Server) queueIn(ctx context.Context, cluster string, jobs, ended []*devpodv1alpha1.Job, now time.Time) {
	cc := s.clientsOf(cluster)
	if cc == nil {
		return
	}
	ctx = s.in(ctx, cc)
	byID := map[string]*devpodv1alpha1.Job{}
	for _, j := range append(slices.Clone(jobs), ended...) {
		byID[j.Name] = j
	}
	live, all, err := s.rankDevPods(ctx, cc, byID)
	if err != nil {
		slog.Warn("queue", "cluster", cluster, "err", err)
		return
	}
	for _, j := range ended {
		if len(all[j.Name]) > 0 {
			s.cleanUpJob(ctx, cc, j)
		}
	}
	var active, queued []*devpodv1alpha1.Job
	for _, j := range jobs {
		switch j.Status.Phase {
		case devpodv1alpha1.JobStarting, devpodv1alpha1.JobRunning:
			s.followJob(ctx, cc, j, live[j.Name], now)
			if j.Status.Phase == devpodv1alpha1.JobStarting || j.Status.Phase == devpodv1alpha1.JobRunning {
				active = append(active, j)
			}
		default:
			if j.Spec.Cancelled != nil {
				s.endJob(ctx, cc, j, devpodv1alpha1.JobCancelled, "cancelled", nil, now)
				continue
			}
			// a former start's ranks still going (being deleted, their
			// Pods in their grace): it waits for them to be gone, or its
			// new ranks would be taken for them
			if leftover(j, all[j.Name]) {
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
	// the port blocks running jobs have, by machine
	ports := map[string]map[int32]bool{}
	for _, j := range active {
		lo, _ := jobPortsOf(j)
		for _, r := range j.Status.Ranks {
			if ports[r.Node] == nil {
				ports[r.Node] = map[int32]bool{}
			}
			ports[r.Node][lo] = true
		}
	}
	if s.queueBeforeStarts != nil {
		s.queueBeforeStarts()
	}
	// what runs while the jobs start: not the jobs stopped for others, and
	// each started one from its start on
	runs := slices.DeleteFunc(slices.Clone(v.Running), func(r qRun) bool {
		return slices.ContainsFunc(plan.Preempt, func(p *devpodv1alpha1.Job) bool { return p.Name == r.Job.Name })
	})
	for _, st := range plan.Starts {
		if !s.startJob(ctx, cc, st.Job, st.Machines, nodes, ports, now) {
			continue
		}
		// a job that may not be stopped, once it has said it starts, looks
		// at the reservations afresh: one made since the plan was read
		// (whose own look did not see this job yet) is given way to
		if !st.Job.Spec.Preemptible && s.Reservations != nil {
			if fresh, err := s.nodeReservations(ctx); err == nil && !keptBesideReservations(v, st, fresh, s.resCluster(ctx), runs) {
				s.requeueJob(ctx, cc, st.Job, "reserved")
				continue
			}
		}
		runs = append(runs, qRun{Job: st.Job, Machines: st.Machines, Ask: jobAsk(st.Job), Until: now.Add(st.Job.Spec.TimeLimit.Duration),
			Preemptible: st.Job.Spec.Preemptible, Priority: st.Job.Spec.Priority, Started: now})
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

// rankDevPods: cluster's ranks of jobs (by id), by job — live: by rank,
// not being deleted; all: any, being deleted too. A DevPod that names a
// job but is not a rank the queue made of it (its name, owner) is not
// one: as any other DevPod.
func (s *Server) rankDevPods(ctx context.Context, cc *clusters.ClusterClients, jobs map[string]*devpodv1alpha1.Job) (live map[string]map[int32]*devpodv1alpha1.DevPod, all map[string][]*devpodv1alpha1.DevPod, err error) {
	var l devpodv1alpha1.DevPodList
	if err := cc.Client.List(ctx, &l, client.InNamespace(s.NS), client.HasLabels{jobLabel}); err != nil {
		return nil, nil, err
	}
	live, all = map[string]map[int32]*devpodv1alpha1.DevPod{}, map[string][]*devpodv1alpha1.DevPod{}
	for i := range l.Items {
		d := &l.Items[i]
		if d.Spec.Job == nil {
			continue
		}
		j := jobs[d.Spec.Job.Name]
		if j == nil || !isRankOf(d, j) {
			continue
		}
		all[j.Name] = append(all[j.Name], d)
		if d.DeletionTimestamp != nil {
			continue
		}
		if live[j.Name] == nil {
			live[j.Name] = map[int32]*devpodv1alpha1.DevPod{}
		}
		live[j.Name][d.Spec.Job.Rank] = d
	}
	return live, all, nil
}

// leftover: of a waiting job j's ranks ds, one of a former start (or
// being deleted) is still there. Ranks of the start it is making (made,
// but the round making them did not get to say so) are not.
func leftover(j *devpodv1alpha1.Job, ds []*devpodv1alpha1.DevPod) bool {
	return slices.ContainsFunc(ds, func(d *devpodv1alpha1.DevPod) bool {
		return d.DeletionTimestamp != nil || attemptOf(d) != j.Status.Attempts+1
	})
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
		j.Status.GangTimeouts++
		if j.Status.GangTimeouts >= maxGangTimeouts {
			// it is not going to start: say what kept it
			s.endJob(ctx, cc, j, devpodv1alpha1.JobFailed, "gang-timeout", s.whyNotStarted(ctx, cc, j, ranks), now)
			return
		}
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

// endJob ends j as phase (reason, detail); then, off the round, its
// ranks' output is kept (where its status says) and its ranks are
// deleted (a rank still running is told to stop and has its grace).
func (s *Server) endJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, phase devpodv1alpha1.JobPhase, reason string, detail map[string]string, now time.Time) {
	t := metav1.NewTime(now)
	j.Status.Phase, j.Status.Reason, j.Status.Detail, j.Status.FinishedAt = phase, reason, detail, &t
	j.Status.Position, j.Status.ExpectedStart = 0, nil
	if s.keepsRankLogs() {
		for i := range j.Status.Ranks {
			if rk := &j.Status.Ranks[i]; rk.Log == "" {
				rk.Log = store.JobLogKey(j.Spec.Cluster, j.Name, rk.Rank)
			}
		}
	}
	if !s.writeJobStatus(ctx, j) {
		return // it changed meanwhile: the next round sees it as it is
	}
	slog.Info("queue_job_end", "job", j.Name, "owner", j.Spec.Owner, "phase", phase, "reason", reason)
	s.cleanUpJob(ctx, cc, j)
}

// cleanUpJob deletes the ended job j's ranks, keeping their output to its
// end, in the background (bounded by jobCleanupTimeout), once at a time
// per job; a round that finds its ranks still there asks again.
func (s *Server) cleanUpJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job) {
	s.stopInBackground(ctx, cc, j, func(r int32) string { return store.JobLogKey(j.Spec.Cluster, j.Name, r) })
}

// stopInBackground: keepAndStop(j, key), off the round, once at a time
// per job.
func (s *Server) stopInBackground(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, key func(rank int32) string) {
	if _, busy := s.queueCleaning.LoadOrStore(j.Name, true); busy {
		return
	}
	j = j.DeepCopy()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobCleanupTimeout)
	go func() {
		defer s.queueCleaning.Delete(j.Name)
		defer cancel()
		s.keepAndStop(ctx, cc, j, key)
	}()
}

// requeueJob puts j back in the queue, keeping its place (its submission
// time), and stops its ranks; it starts again once they are gone.
func (s *Server) requeueJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, reason string) {
	j.Status.Phase, j.Status.Reason, j.Status.Detail = devpodv1alpha1.JobQueued, reason, nil
	j.Status.Ranks, j.Status.AttemptAt, j.Status.StartedAt = nil, nil, nil
	if reason == "preempted" {
		j.Status.Preemptions++
	}
	if !s.writeJobStatus(ctx, j) {
		return // it changed meanwhile: the next round sees it as it is
	}
	// its ranks go, their output kept as this start's; the job waits for
	// them to be gone
	attempt := j.Status.Attempts
	s.stopInBackground(ctx, cc, j, func(r int32) string { return store.JobAttemptLogKey(j.Spec.Cluster, j.Name, attempt, r) })
	slog.Info("queue_job_requeued", "job", j.Name, "owner", j.Spec.Owner, "reason", reason)
}

// startJob makes j's ranks on machines and marks it Starting. A rank
// found made already (a round that did not get to say so) is taken as
// it is; any other failure takes back what was made.
func (s *Server) startJob(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, machines []string, nodes map[string]*corev1.Node, ports map[string]map[int32]bool, now time.Time) bool {
	// a port block no job on these machines has
	j.Status.PortBase = pickPorts(j, machines, ports)
	rank0 := rankName(j.Spec.Owner, j.Name, 0)
	master := masterAddr(j, rank0, s.NS, j.Spec.Template.Spec.HostNetwork, internalIP(nodes[machines[0]]))
	ranks := make([]devpodv1alpha1.JobRank, 0, len(machines))
	var made []*devpodv1alpha1.DevPod
	undo := func() {
		for _, dp := range made {
			if err := cc.Client.Delete(ctx, dp); err != nil && !apierrors.IsNotFound(err) {
				slog.Warn("queue_start_undo", "job", j.Name, "devpod", dp.Name, "err", err)
			}
		}
	}
	for r, m := range machines {
		dp := rankDevPod(j, int32(r), m, s.NS, master)
		if err := cc.Client.Create(ctx, dp); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				slog.Warn("queue_start", "job", j.Name, "rank", r, "err", err)
				undo()
				return false
			}
			// made already: taken as it is only if it is this start's
			// (one of a former start is going: this start waits for it)
			var cur devpodv1alpha1.DevPod
			if err := cc.Reader.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: dp.Name}, &cur); err != nil ||
				!isRankOf(&cur, j) || cur.DeletionTimestamp != nil || attemptOf(&cur) != j.Status.Attempts+1 {
				slog.Info("queue_start_waits", "job", j.Name, "devpod", dp.Name)
				undo()
				return false
			}
			if pinnedNode(&cur) != "" {
				m = pinnedNode(&cur)
			}
			if r == 0 { // made by a round that did not get to say so: its block is the job's
				for _, e := range cur.Spec.Pod.Spec.Containers[0].Env {
					if b, err := strconv.Atoi(e.Value); e.Name == "MASTER_PORT" && err == nil {
						j.Status.PortBase = int32(b)
					}
				}
			}
		} else {
			made = append(made, dp)
		}
		ranks = append(ranks, devpodv1alpha1.JobRank{Rank: int32(r), Node: m, DevPod: dp.Name})
	}
	t := metav1.NewTime(now)
	j.Status.Phase, j.Status.AttemptAt, j.Status.Ranks = devpodv1alpha1.JobStarting, &t, ranks
	j.Status.Attempts++
	j.Status.Reason, j.Status.Detail, j.Status.Position, j.Status.ExpectedStart, j.Status.StartedAt = "", nil, 0, nil, nil
	written := s.writeJobStatus(ctx, j)
	if written {
		slog.Info("queue_job_start", "job", j.Name, "owner", j.Spec.Owner, "machines", machines)
	}
	for _, m := range ranksMachines(ranks) {
		if ports[m] == nil {
			ports[m] = map[int32]bool{}
		}
		ports[m][j.Status.PortBase] = true
	}
	// not written (cancelled meanwhile): its ranks are cleaned up by the
	// rounds that find them, as an ended job's
	return written
}

// writeJobStatus writes j's status, worked out from j as it was read:
// if j has changed since (cancelled, or ended or queued again by the
// reservations' loop), nothing is written and it says false — the next
// round works from j as it is then, so no change is undone.
func (s *Server) writeJobStatus(ctx context.Context, j *devpodv1alpha1.Job) bool {
	if err := s.Client.Status().Update(ctx, j); err != nil {
		if apierrors.IsConflict(err) {
			slog.Info("queue_status_stale", "job", j.Name)
		} else {
			slog.Warn("queue_status", "job", j.Name, "err", err)
		}
		return false
	}
	return true
}

// keepsRankLogs: ended jobs' ranks' output is kept.
func (s *Server) keepsRankLogs() bool {
	return s.PodLogs != nil && s.Audit != nil && s.Audit.Objects != nil
}

// whyNotStarted: what kept j's first rank not running from running —
// for a job failed for not starting.
func (s *Server) whyNotStarted(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, ranks map[int32]*devpodv1alpha1.DevPod) map[string]string {
	for r := int32(0); r < j.Spec.Nodes; r++ {
		d := ranks[r]
		if d != nil && d.Status.Phase == devpodv1alpha1.DevPodRunning {
			continue
		}
		why := "its DevPod is not there"
		if d != nil {
			why = d.Status.Message
			var pod corev1.Pod
			if cc.Client.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: render.PodName(d)}, &pod) == nil {
				if w := podWaitingFor(&pod); w != "" {
					why = w
				}
			}
			if why == "" {
				why = "its Pod did not start"
			}
		}
		return map[string]string{"rank": itoa(r), "why": why}
	}
	return nil
}

// podWaitingFor: why a Pod is not running — not placed, or a container
// waiting (an image not pulled).
func podWaitingFor(pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
			return c.Message
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			if w.Message != "" {
				return w.Reason + ": " + w.Message
			}
			return w.Reason
		}
	}
	return ""
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
	activeByID := map[string]*devpodv1alpha1.Job{}
	for _, j := range active {
		activeByID[j.Name] = j
	}
	byName := map[string]*corev1.Node{}
	var machines []qMachine
	for i := range nodes.Items {
		n := &nodes.Items[i]
		byName[n.Name] = n
		if n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" || !readyOrUnknown(n) || n.Spec.Unschedulable {
			continue
		}
		var rs []*devpodv1alpha1.NodeReservation
		for k := range all {
			if all[k].Spec.Cluster == cluster && all[k].Spec.Node == n.Name {
				rs = append(rs, &all[k])
			}
		}
		// what DevPods take here — but a running job's ranks, which the
		// jobs' own accounts hold
		var interactive []placed
		for _, p := range on[n.Name] {
			if p.Job != nil {
				if j := activeByID[p.Job.ID]; j != nil && isRankOf(p.DP, j) {
					continue
				}
			}
			interactive = append(interactive, p)
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
	// the access rules: each owner's read once, each job's judged per
	// machine in memory
	owners := map[string]*ownerAccessOf{}
	allowed := map[string]bool{}
	mayRun := func(j *devpodv1alpha1.Job, machine string) bool {
		key := j.Name + "/" + machine
		if v, seen := allowed[key]; seen {
			return v
		}
		oa, seen := owners[j.Spec.Owner]
		if !seen {
			var err error
			if oa, err = s.ownerAccess(ctx, j.Spec.Owner); err != nil {
				slog.Warn("queue_access", "owner", j.Spec.Owner, "err", err)
			}
			owners[j.Spec.Owner] = oa
		}
		n := byName[machine]
		ok := false
		if n != nil && oa != nil && machineFits(j, n) {
			d, admin := oa.decide(cluster, access.Node{Name: n.Name, Labels: n.Labels})
			ok = admin || (d.Allow && d.Admits(access.NeedsOf(&j.Spec.Template.Spec)) == nil)
		}
		allowed[key] = ok
		return ok
	}
	var dps devpodv1alpha1.DevPodList
	if err := cc.Client.List(ctx, &dps, client.InNamespace(s.NS)); err != nil {
		return queueView{}, nil, err
	}
	quotaOK := func(j *devpodv1alpha1.Job, planned []*devpodv1alpha1.Job) bool {
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
		// the owner's jobs this round starts before it count as running
		for _, p := range planned {
			if p.Spec.Owner == j.Spec.Owner {
				for r := int32(0); r < p.Spec.Nodes; r++ {
					existing = append(existing, *rankDevPod(p, r, "x", s.NS, ""))
				}
			}
		}
		for r := int32(0); r < j.Spec.Nodes-1; r++ {
			existing = append(existing, *rankDevPod(j, r, "x", s.NS, ""))
		}
		return CheckQuota(EffectiveQuota(up, cc.Name, s.clusterQuota(ctx, cc)), existing, rankDevPod(j, j.Spec.Nodes-1, "x", s.NS, "")) == nil
	}
	return queueView{Now: now, Machines: machines, Running: running, Queued: queued, Allowed: mayRun, QuotaOK: quotaOK, Horizon: 14 * 24 * time.Hour}, byName, nil
}

// jobsByID: the queue's jobs not ended, by id (none with the queue off).
func (s *Server) jobsByID(ctx context.Context) map[string]*devpodv1alpha1.Job {
	out := map[string]*devpodv1alpha1.Job{}
	if s.Queue == nil {
		return out
	}
	var all devpodv1alpha1.JobList
	if err := s.Reader.List(ctx, &all); err != nil {
		slog.Warn("queue", "err", err)
		return out
	}
	for i := range all.Items {
		if !jobEnded(all.Items[i].Status.Phase) {
			out[all.Items[i].Name] = &all.Items[i]
		}
	}
	return out
}

// jobHolds: in-memory reservations for what cluster's running (or
// starting) jobs that may not be stopped hold on node, until their time
// limit — a reservation there and then must leave them room.
func (s *Server) jobHolds(ctx context.Context, cluster, node string, now time.Time) []*devpodv1alpha1.NodeReservation {
	var out []*devpodv1alpha1.NodeReservation
	for _, j := range s.jobsByID(ctx) {
		if j.Spec.Cluster != cluster || j.Spec.Preemptible || (j.Status.Phase != devpodv1alpha1.JobStarting && j.Status.Phase != devpodv1alpha1.JobRunning) {
			continue
		}
		since := now
		switch {
		case j.Status.StartedAt != nil:
			since = j.Status.StartedAt.Time
		case j.Status.AttemptAt != nil:
			since = j.Status.AttemptAt.Time
		}
		for _, r := range j.Status.Ranks {
			if r.Node != node {
				continue
			}
			gpu, cpu := j.Spec.GPUsPerNode, j.Spec.CPUsPerNode.DeepCopy()
			out = append(out, &devpodv1alpha1.NodeReservation{
				ObjectMeta: metav1.ObjectMeta{Name: "job-" + j.Name + "-" + itoa(r.Rank)},
				Spec: devpodv1alpha1.NodeReservationSpec{Cluster: cluster, Node: node, User: j.Spec.Owner,
					Start: metav1.NewTime(since), End: metav1.NewTime(since.Add(j.Spec.TimeLimit.Duration)), GPU: &gpu, CPU: &cpu},
			})
		}
	}
	return out
}

// queueFree: what the queue can use in one cluster now — free cards and
// cores on its reservable machines (outside the reservations in force,
// what DevPods and running jobs take), and the most cards one machine has.
type queueFree struct {
	Cluster  string  `json:"cluster,omitempty"`
	GPU      int64   `json:"gpu"`
	CPU      float64 `json:"cpu"`
	MaxGPU   int64   `json:"maxGPU"`
	Machines int     `json:"machines"`
}

// handleJobsFree serves GET /api/jobs/free: queueFree of each cluster
// shown to the caller that takes jobs.
func (s *Server) handleJobsFree(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	if s.Queue == nil {
		s.writeErr(w, http.StatusNotFound, "QUEUE_OFF", "the queue is not on in this console", nil)
		return
	}
	now := s.Queue.now()
	jobs := s.jobsByID(r.Context())
	shown := s.clusterGate(r.Context(), sess)
	items := []queueFree{}
	for _, cc := range s.ks(r) {
		ctx := s.in(r.Context(), cc)
		cluster := s.resCluster(ctx)
		if !shown(cc.Name) || !s.takesJobs(cluster) {
			continue
		}
		var active []*devpodv1alpha1.Job
		for _, j := range jobs {
			if j.Spec.Cluster == cluster && (j.Status.Phase == devpodv1alpha1.JobStarting || j.Status.Phase == devpodv1alpha1.JobRunning) {
				active = append(active, j)
			}
		}
		v, _, err := s.queueViewFor(ctx, cc, active, nil, now)
		if err != nil {
			slog.Warn("queue_free", "cluster", cluster, "err", err)
			continue
		}
		f := queueFree{Cluster: s.clusterLabel(cc), Machines: len(v.Machines)}
		var sum amount
		for _, m := range v.Machines {
			free := freeFor(m, v.Running, now, time.Minute).clamped()
			sum = sum.add(free)
			f.MaxGPU = max(f.MaxGPU, free.GPU)
		}
		f.GPU, f.CPU = sum.GPU, float64(sum.CPU)/1000
		items = append(items, f)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// jobHoldView: a running job's rank on a machine, as the reservation
// timeline draws it — what it takes there and until when at most.
type jobHoldView struct {
	Cluster     string    `json:"cluster,omitempty"`
	ID          string    `json:"id"`
	Title       string    `json:"title,omitempty"`
	Owner       string    `json:"owner"`
	Mine        bool      `json:"mine"`
	Rank        int32     `json:"rank"`
	Node        string    `json:"node"`
	GPU         int32     `json:"gpu"`
	CPU         float64   `json:"cpu"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Preemptible bool      `json:"preemptible,omitempty"`
}

// jobHoldViews: cluster's running (or starting) jobs' ranks, for user,
// on the machines show lets them see.
func (s *Server) jobHoldViews(ctx context.Context, cluster, label, user string, show func(node string) bool, now time.Time) []jobHoldView {
	var out []jobHoldView
	for _, j := range s.jobsByID(ctx) {
		if j.Spec.Cluster != cluster || (j.Status.Phase != devpodv1alpha1.JobStarting && j.Status.Phase != devpodv1alpha1.JobRunning) {
			continue
		}
		since := now
		switch {
		case j.Status.StartedAt != nil:
			since = j.Status.StartedAt.Time
		case j.Status.AttemptAt != nil:
			since = j.Status.AttemptAt.Time
		}
		for _, r := range j.Status.Ranks {
			if r.Node == "" || (j.Spec.Owner != user && !show(r.Node)) {
				continue
			}
			out = append(out, jobHoldView{Cluster: label, ID: j.Name, Title: j.Spec.Title, Owner: j.Spec.Owner, Mine: j.Spec.Owner == user, Rank: r.Rank,
				Node: r.Node, GPU: j.Spec.GPUsPerNode, CPU: float64(j.Spec.CPUsPerNode.MilliValue()) / 1000,
				Start: since, End: since.Add(j.Spec.TimeLimit.Duration), Preemptible: j.Spec.Preemptible})
		}
	}
	slices.SortFunc(out, func(a, b jobHoldView) int {
		return cmp.Or(strings.Compare(a.Node, b.Node), a.Start.Compare(b.Start), strings.Compare(a.ID, b.ID))
	})
	return out
}

// ranksMachines: the machines ranks are on.
func ranksMachines(ranks []devpodv1alpha1.JobRank) []string {
	out := make([]string, 0, len(ranks))
	for _, r := range ranks {
		out = append(out, r.Node)
	}
	return out
}

// keptBesideReservations: st's ranks still have room on each of their
// machines beside the reservations as they are now (fresh) and runs.
func keptBesideReservations(v queueView, st qStart, fresh []devpodv1alpha1.NodeReservation, cluster string, runs []qRun) bool {
	ask, limit := jobAsk(st.Job), st.Job.Spec.TimeLimit.Duration
	for _, name := range st.Machines {
		i := slices.IndexFunc(v.Machines, func(m qMachine) bool { return m.Name == name })
		if i < 0 {
			continue
		}
		m := v.Machines[i]
		m.Rs = nil
		for k := range fresh {
			if fresh[k].Spec.Cluster == cluster && fresh[k].Spec.Node == name {
				m.Rs = append(m.Rs, &fresh[k])
			}
		}
		if !ask.fits(freeFor(m, runs, v.Now, limit)) {
			return false
		}
	}
	return true
}
