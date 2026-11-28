// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// The job queue (design 2026-11-26): users submit jobs — so many machines,
// so many cards and cores on each, for at most so long, at a priority —
// that wait in their cluster's queue for the reservable machines'
// unreserved capacity and run as job ranks (run-to-completion DevPods).
// Jobs (Job) and the priority caps (JobPriorityRule) are kept by the
// console in its own cluster; the leader schedules (queue_run.go).

// QueueRules are the queue's parameters; nil on Server turns it off.
type QueueRules struct {
	DefaultTimeLimit time.Duration // a job's, when it says none
	MaxTimeLimit     time.Duration // the longest, unless a Cluster says less
	GangTimeout      time.Duration // all ranks running within, else requeued
	PreemptGrace     time.Duration // a stopped rank's time to finish
	Tick             time.Duration // how often the scheduler goes round
	KeepEnded        time.Duration // an ended job's object is kept so long
	MaxJobsPerUser   int           // jobs one person may have waiting or running in a cluster, unless a job rule says (0: no cap; admins none)
	Now              func() time.Time
}

func (q *QueueRules) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// priorityCap: the highest priority user (in groups) may give a job in
// cluster — the highest of the JobPriorityRules whose condition holds
// for them there (none: 0); unlimited for an admin. A rule that does
// not parse gives nothing.
func (s *Server) priorityCap(ctx context.Context, user string, groups map[string]bool, cluster string) (int32, bool, error) {
	if s.ownerIsAdmin(ctx, user) {
		return 0, true, nil
	}
	var rules devpodv1alpha1.JobPriorityRuleList
	if err := s.Client.List(ctx, &rules); err != nil {
		return 0, false, err
	}
	var best int32
	for i := range rules.Items {
		r := &rules.Items[i]
		e, err := access.Parse(r.Spec.Condition)
		if err != nil {
			slog.Warn("job_priority_rule", "rule", r.Name, "err", err)
			continue
		}
		if e.Eval(access.Facts{User: &user, Groups: groups, Cluster: &cluster}) == access.True && r.Spec.MaxPriority > best {
			best = r.Spec.MaxPriority
		}
	}
	return best, false, nil
}

// jobCap: how many jobs user (in groups) may have waiting or running in
// cluster — the most of the JobPriorityRules that hold for them there and
// say (MaxJobs), else the console's default (0: no cap); none for an
// admin. A rule that does not parse gives nothing.
func (s *Server) jobCap(ctx context.Context, user string, groups map[string]bool, cluster string) (int, error) {
	if s.ownerIsAdmin(ctx, user) {
		return 0, nil
	}
	var rules devpodv1alpha1.JobPriorityRuleList
	if err := s.Client.List(ctx, &rules); err != nil {
		return 0, err
	}
	best, said := 0, false
	for i := range rules.Items {
		r := &rules.Items[i]
		if r.Spec.MaxJobs == nil {
			continue
		}
		e, err := access.Parse(r.Spec.Condition)
		if err != nil {
			continue // priorityCap warns of it
		}
		if e.Eval(access.Facts{User: &user, Groups: groups, Cluster: &cluster}) == access.True && int(*r.Spec.MaxJobs) > best {
			best, said = int(*r.Spec.MaxJobs), true
		}
	}
	if !said {
		return s.Queue.MaxJobsPerUser, nil
	}
	return best, nil
}

// maxTimeLimit: the longest time limit a job may ask in cluster — its
// Cluster's JobMaxTimeLimit, else the console's.
func (s *Server) maxTimeLimit(ctx context.Context, cluster string) time.Duration {
	if cluster != "" {
		var c devpodv1alpha1.Cluster
		if err := s.Client.Get(ctx, types.NamespacedName{Name: cluster}, &c); err == nil && c.Spec.JobMaxTimeLimit != nil && c.Spec.JobMaxTimeLimit.Duration > 0 {
			return c.Spec.JobMaxTimeLimit.Duration
		}
	}
	return s.Queue.MaxTimeLimit
}

// partialReservations: cluster's reservations may be of part of a machine
// — as its Cluster says (PartialReservations); a cluster no Cluster object
// is of (a console's own), as the console's rules say (off by default).
func (s *Server) partialReservations(ctx context.Context, cluster string) bool {
	if cluster != "" {
		var c devpodv1alpha1.Cluster
		if s.Client.Get(ctx, types.NamespacedName{Name: cluster}, &c) == nil {
			return c.Spec.PartialReservations
		}
	}
	return s.Reservations != nil && s.Reservations.Partial
}
