// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// The console carries the reservations out itself, on whichever replica
// leads. When a reservation starts, it gets its share: of the DevPods on
// the machine whose owners hold none of the reservations in force, as
// many as it takes (the newest first) are marked — the notice, at least
// ClearNotice long, the web shows — and hibernated when it runs out
// (clearIn). When a reservation ends (its time is up, or it is released),
// its holder's DevPods on the machine that only it let run are hibernated
// — those whose owner the access rules ask for a reservation there and
// who holds no other reservation covering now.

// reservationTick: how often the reservations are carried out.
const reservationTick = 15 * time.Second

// RunReservations carries the reservations out every reservationTick
// while leads says this replica leads (nil: it always does), until ctx
// ends. Without reservations it returns at once.
func (s *Server) RunReservations(ctx context.Context, leads func() bool) {
	if s.Reservations == nil {
		return
	}
	t := time.NewTicker(reservationTick)
	defer t.Stop()
	for {
		if leads == nil || leads() {
			s.EnforceReservations(ctx)
		} else {
			s.loseLead()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// loseLead: this replica no longer leads — when it leads again it sweeps
// every cluster with reservations once more (another leader's marks).
func (s *Server) loseLead() { s.clearSwept.Store(false) }

// clientsOf: a cluster's clients by the name reservations give it ("" is
// the console's own, in a console of one cluster); nil when away.
func (s *Server) clientsOf(cluster string) *clusters.ClusterClients {
	if s.Clusters == nil {
		if cluster == "" {
			return s.local()
		}
		return nil
	}
	cc, err := s.Clusters.Clients(cluster)
	if err != nil {
		return nil
	}
	return cc
}

// EnforceReservations is one round: reservations over for keptFor go;
// the end of every reservation that ended is carried out.
func (s *Server) EnforceReservations(ctx context.Context) {
	all, err := s.nodeReservations(ctx)
	if err != nil {
		slog.Warn("reservations", "err", err)
		return
	}
	now := s.Reservations.now()
	ended := map[string][]*devpodv1alpha1.NodeReservation{} // by cluster
	for i := range all {
		nr := &all[i]
		over := overAt(nr)
		if now.Before(over) {
			continue
		}
		if now.Sub(over) > keptFor {
			if err := s.deleteReservation(ctx, nr.Name); err != nil {
				slog.Warn("reservation_prune", "reservation", nr.Name, "err", err)
			}
			continue
		}
		if !nr.Status.Ended {
			ended[nr.Spec.Cluster] = append(ended[nr.Spec.Cluster], nr)
		}
	}
	for cluster, rs := range ended {
		s.endIn(ctx, cluster, rs, all, now)
	}
	for cluster := range s.clearingClusters(all, now) {
		s.clearIn(ctx, cluster, all, now)
	}
}

// clearingClusters: the clusters to clear in — those with a reservation
// in force or starting within the notice, those this replica has marked
// DevPods in, and (once, after it starts) every cluster with reservations,
// whose marks another replica may have left.
func (s *Server) clearingClusters(all []devpodv1alpha1.NodeReservation, now time.Time) map[string]bool {
	out := map[string]bool{}
	sweep := !s.clearSwept.Swap(true)
	soon := now.Add(s.Reservations.ClearNotice)
	for i := range all {
		nr := &all[i]
		if sweep || (nr.Spec.Start.Time.Before(overAt(nr)) && !soon.Before(nr.Spec.Start.Time) && now.Before(overAt(nr))) {
			out[nr.Spec.Cluster] = true
		}
	}
	s.clearMarked.Range(func(k, _ any) bool {
		out[k.(string)] = true
		return true
	})
	return out
}

// clearedFor: of rs, the reservation in force at t that DevPods are
// cleared for — the latest to start, then the greater name.
func clearedFor(rs []*devpodv1alpha1.NodeReservation, t time.Time) string {
	var best *devpodv1alpha1.NodeReservation
	for _, nr := range rs {
		if !inForce(nr, t) {
			continue
		}
		if best == nil || nr.Spec.Start.Time.After(best.Spec.Start.Time) || (nr.Spec.Start.Time.Equal(best.Spec.Start.Time) && nr.Name > best.Name) {
			best = nr
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// clearIn gives cluster's reservations in force now, and those starting
// within the notice, their shares (all: every reservation). A DevPod in
// the way is marked with when it goes and for which reservation — no
// sooner than ClearNotice after it is first marked — and hibernated when
// that time comes; one no longer in the way loses its mark.
func (s *Server) clearIn(ctx context.Context, cluster string, all []devpodv1alpha1.NodeReservation, now time.Time) {
	cc := s.clientsOf(cluster)
	if cc == nil {
		return // away: done when it is back
	}
	notice := s.Reservations.ClearNotice
	// only reservations made since they hold shares clear anyone (one made
	// before was a permit, and its holder never asked for that)
	byNode := map[string][]*devpodv1alpha1.NodeReservation{}
	for i := range all {
		if nr := &all[i]; nr.Spec.Cluster == cluster && nr.Annotations[devpodv1alpha1.ClearsAnnotation] == "true" {
			byNode[nr.Spec.Node] = append(byNode[nr.Spec.Node], nr)
		}
	}
	on, stopped, err := s.devPodsOn(ctx, cc)
	if err != nil {
		slog.Warn("reservation_clear", "cluster", cluster, "err", err)
		return
	}
	// queue jobs' ranks: whether their job may be stopped
	jobs := s.jobsByID(ctx)
	for _, ps := range on {
		for i := range ps {
			if ps[i].Job != nil {
				if j := jobs[ps[i].Job.ID]; j != nil && j.Spec.Cluster == cluster && isRankOf(ps[i].DP, j) {
					ps[i].Job.Preemptible = j.Spec.Preemptible
				} else {
					ps[i].Job = nil // its job is gone (or it is none of its): as any DevPod
				}
			}
		}
	}
	stillMarked := false
	// stopped (by its owner, after the notice): the mark goes with it
	for _, dp := range stopped {
		if !s.unmarkClearing(ctx, cc, dp) {
			stillMarked = true
		}
	}
	type mark struct {
		at    time.Time
		forID string
	}
	want := map[string]mark{}    // by DevPod name
	requeue := map[string]bool{} // preemptible jobs in the way, by id
	hold := map[string]bool{}    // on a machine not judged now: left as they are
	for node, rs := range byNode {
		var moments []time.Time
		for _, nr := range rs {
			start := nr.Spec.Start.Time
			switch {
			case !start.Before(overAt(nr)):
			case inForce(nr, now):
				moments = append(moments, now)
			case start.After(now) && !start.After(now.Add(notice)):
				moments = append(moments, start)
			}
		}
		if len(moments) == 0 || len(on[node]) == 0 {
			continue
		}
		var n corev1.Node
		if err := cc.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil || n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" {
			continue
		}
		slices.SortFunc(moments, func(a, b time.Time) int { return a.Compare(b) })
		moments = slices.Compact(moments)
		c, left := capacityOf(&n), slices.Clone(on[node])
		// not ready, or reporting less than its reservations hold (a device
		// plugin restarting): what it has is not known — nobody is marked or
		// hibernated there until it is back
		if !readyOrUnknown(&n) || slices.ContainsFunc(moments, func(t time.Time) bool { return !bookedAt(rs, c, t).fits(c) }) {
			for _, p := range on[node] {
				hold[p.DP.Name] = true
			}
			continue
		}
		for _, t := range moments {
			id := clearedFor(rs, t)
			for _, p := range toClear(c, rs, left, t) {
				if p.Job != nil {
					// a preemptible job's rank: its job stops at once and waits again
					requeue[p.Job.ID] = true
					left = slices.DeleteFunc(left, func(q placed) bool { return q.Job != nil && q.Job.ID == p.Job.ID })
					continue
				}
				// cleared for the reservation it was marked for, while that
				// one is in force then
				forID := id
				if was := p.DP.Annotations[devpodv1alpha1.ClearForAnnotation]; was != "" && slices.ContainsFunc(rs, func(nr *devpodv1alpha1.NodeReservation) bool { return nr.Name == was && inForce(nr, t) }) {
					forID = was
				}
				want[p.DP.Name] = mark{at: later(t, now.Add(notice)), forID: forID}
				left = slices.DeleteFunc(left, func(q placed) bool { return q.DP.Name == p.DP.Name })
			}
		}
	}
	for id := range requeue {
		if j := jobs[id]; j != nil {
			s.requeueJob(ctx, cc, j, "preempted")
		}
	}
	cleared := map[string][]string{} // by reservation
	for _, ps := range on {
		for _, p := range ps {
			dp := p.DP
			atText, has := dp.Annotations[devpodv1alpha1.ClearAtAnnotation]
			forID := dp.Annotations[devpodv1alpha1.ClearForAnnotation]
			if hold[dp.Name] {
				if has || forID != "" {
					stillMarked = true
				}
				continue
			}
			if p.Job != nil && requeue[p.Job.ID] {
				continue // gone with its job
			}
			m, listed := want[dp.Name]
			if !listed {
				if (has || forID != "") && !s.unmarkClearing(ctx, cc, dp) {
					stillMarked = true
				}
				continue
			}
			at, err := time.Parse(time.RFC3339, atText)
			switch {
			case !has || err != nil:
				// not told yet: the notice
				s.markClearing(ctx, cc, dp, m.at, m.forID)
				stillMarked = true
			case now.Before(at):
				// told: the time stands (another reservation starting meanwhile
				// does not put it off); only what it is for may change
				if forID != m.forID {
					s.markClearing(ctx, cc, dp, at, m.forID)
				}
				stillMarked = true
			case s.hibernateFor(ctx, cc, dp, "reservation/"+m.forID):
				cleared[m.forID] = append(cleared[m.forID], dp.Name)
			default:
				stillMarked = true
			}
		}
	}
	if stillMarked {
		s.clearMarked.Store(cluster, true)
	} else {
		s.clearMarked.Delete(cluster)
	}
	for id, names := range cleared {
		s.setReservationStatus(ctx, id, names, false, "cleared", func(st *devpodv1alpha1.NodeReservationStatus) {
			st.Cleared = append(st.Cleared, names...)
		})
	}
}

// readyOrUnknown: n says it is Ready, or says nothing either way.
func readyOrUnknown(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return true
}

// markClearing notes on dp when it is to be hibernated, and for which
// reservation; false when it could not.
func (s *Server) markClearing(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod, at time.Time, forID string) bool {
	return s.setClearMark(ctx, cc, dp, map[string]any{devpodv1alpha1.ClearAtAnnotation: at.UTC().Format(time.RFC3339), devpodv1alpha1.ClearForAnnotation: forID})
}

// unmarkClearing takes dp's mark off.
func (s *Server) unmarkClearing(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod) bool {
	return s.setClearMark(ctx, cc, dp, map[string]any{devpodv1alpha1.ClearAtAnnotation: nil, devpodv1alpha1.ClearForAnnotation: nil})
}

// setClearMark merges anns (nil: remove) into dp's annotations — the metadata
// only: nothing about how it runs changes.
func (s *Server) setClearMark(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod, anns map[string]any) bool {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": anns}})
	if err := cc.Client.Patch(ctx, dp, client.RawPatch(types.MergePatchType, patch)); err != nil {
		slog.Warn("reservation_clear_mark", "cluster", cc.Name, "devpod", dp.Name, "err", err)
		return false
	}
	slog.Info("reservation_clear_mark", "cluster", cc.Name, "devpod", dp.Name, "owner", dp.Spec.Owner, "mark", anns)
	return true
}

// covered: does user hold a reservation of cluster's node covering t?
func covered(all []devpodv1alpha1.NodeReservation, cluster, node, user string, t time.Time) bool {
	for i := range all {
		nr := &all[i]
		if nr.Spec.Cluster == cluster && nr.Spec.Node == node && nr.Spec.User == user && !t.Before(nr.Spec.Start.Time) && t.Before(overAt(nr)) {
			return true
		}
	}
	return false
}

// endIn carries out the end of cluster's reservations rs (all: every
// reservation, for the others that still cover their holder). An end not
// carried out in full — the machine, the rules or a hibernation could not
// be had — is not marked done: the next round tries it again.
func (s *Server) endIn(ctx context.Context, cluster string, rs []*devpodv1alpha1.NodeReservation, all []devpodv1alpha1.NodeReservation, now time.Time) {
	cc := s.clientsOf(cluster)
	if cc == nil {
		return // away: done when it is back
	}
	on, err := s.devPodsByNode(ctx, cc)
	if err != nil {
		slog.Warn("reservations", "cluster", cluster, "err", err)
		return
	}
	for _, nr := range rs {
		var names []string
		done := true
		if nr.Spec.Start.Time.Before(overAt(nr)) && len(on[nr.Spec.Node]) > 0 { // it was in force, and someone is there
			var node corev1.Node
			if err := cc.Client.Get(ctx, types.NamespacedName{Name: nr.Spec.Node}, &node); err != nil && !apierrors.IsNotFound(err) {
				slog.Warn("reservation_end", "reservation", nr.Name, "err", err)
				continue
			}
			for _, p := range on[nr.Spec.Node] {
				dp := p.DP
				// a queue job's rank runs outside reservations: their end is
				// nothing to it
				if p.Job != nil || dp.Spec.Owner != nr.Spec.User || covered(all, cluster, nr.Spec.Node, nr.Spec.User, now) {
					continue
				}
				d, admin, err := s.decide(ctx, dp.Spec.Owner, cluster, access.Node{Name: nr.Spec.Node, Labels: node.Labels})
				switch {
				case err != nil:
					slog.Warn("reservation_end", "reservation", nr.Name, "devpod", dp.Name, "err", err)
					done = false
				case admin || !d.NeedsReservation:
					// the rules let them in without one
				case s.hibernateFor(ctx, cc, dp, "reservation-end/"+nr.Name):
					names = append(names, dp.Name)
				default:
					done = false
				}
			}
		}
		s.setReservationStatus(ctx, nr.Name, names, done, "hibernated", func(st *devpodv1alpha1.NodeReservationStatus) {
			st.Ended = done
			st.HibernatedAtEnd = append(st.HibernatedAtEnd, names...)
		})
	}
}

// devPodsByNode: a cluster's running DevPods by the node they are on —
// where their Pod is, or, not placed yet, where they are pinned — with
// what they take and since when (their Pod's, else their own).
func (s *Server) devPodsByNode(ctx context.Context, cc *clusters.ClusterClients) (map[string][]placed, error) {
	on, _, err := s.devPodsOn(ctx, cc)
	return on, err
}

// devPodsOn: devPodsByNode, and the stopped DevPods still marked to be
// cleared.
func (s *Server) devPodsOn(ctx context.Context, cc *clusters.ClusterClients) (map[string][]placed, []*devpodv1alpha1.DevPod, error) {
	var dps devpodv1alpha1.DevPodList
	if err := cc.Client.List(ctx, &dps, client.InNamespace(s.NS)); err != nil {
		return nil, nil, err
	}
	var pods corev1.PodList
	if err := cc.Client.List(ctx, &pods, client.InNamespace(s.NS)); err != nil {
		return nil, nil, err
	}
	podOf := map[string]*corev1.Pod{}
	for i := range pods.Items {
		podOf[pods.Items[i].Name] = &pods.Items[i]
	}
	out := map[string][]placed{}
	var stopped []*devpodv1alpha1.DevPod
	for i := range dps.Items {
		dp := &dps.Items[i]
		if !dp.Spec.Running || !dp.DeletionTimestamp.IsZero() {
			if _, marked := dp.Annotations[devpodv1alpha1.ClearAtAnnotation]; marked && dp.DeletionTimestamp.IsZero() {
				stopped = append(stopped, dp)
			}
			continue
		}
		p := placed{DP: dp, Since: dp.CreationTimestamp.Time}
		if rankShaped(dp) {
			p.Job = &jobRef{ID: dp.Spec.Job.Name}
		}
		if dp.Spec.Pod != nil {
			p.Ask = askOfPod(&dp.Spec.Pod.Spec)
		}
		node := ""
		// a Pod that has finished (evicted, done) holds nothing
		if pod := podOf[render.PodName(dp)]; pod != nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			// what holds the machine is its Pod, as it was made
			node, p.Since, p.Ask = pod.Spec.NodeName, pod.CreationTimestamp.Time, askOfPod(&pod.Spec)
		}
		if node == "" {
			node = pinnedNode(dp)
		}
		if node != "" {
			out[node] = append(out[node], p)
		}
	}
	return out, stopped, nil
}

// hibernateFor stops dp on behalf of a reservation (by, which the web
// reads to say why); false when it could not.
func (s *Server) hibernateFor(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod, by string) bool {
	before := dp.DeepCopy()
	dp.Spec.Running = false
	if dp.Annotations == nil {
		dp.Annotations = map[string]string{}
	}
	dp.Annotations[devpodv1alpha1.HibernatedByAnnotation] = by
	delete(dp.Annotations, devpodv1alpha1.ClearAtAnnotation)
	delete(dp.Annotations, devpodv1alpha1.ClearForAnnotation)
	if err := cc.Client.Patch(ctx, dp, client.MergeFrom(before)); err != nil {
		slog.Warn("reservation_hibernate", "cluster", cc.Name, "devpod", dp.Name, "by", by, "err", err)
		return false
	}
	slog.Info("reservation_hibernate", "cluster", cc.Name, "devpod", dp.Name, "owner", dp.Spec.Owner, "by", by)
	return true
}

// setReservationStatus changes a reservation's status and records its end
// (and the DevPods hibernated, as event) in its history.
func (s *Server) setReservationStatus(ctx context.Context, name string, names []string, ended bool, event string, fn func(*devpodv1alpha1.NodeReservationStatus)) {
	if !ended && len(names) == 0 {
		return // nothing to record yet
	}
	var done *devpodv1alpha1.NodeReservation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var nr devpodv1alpha1.NodeReservation
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &nr); err != nil {
			return err
		}
		fn(&nr.Status)
		if err := s.Client.Status().Update(ctx, &nr); err != nil {
			return err
		}
		done = &nr
		return nil
	})
	if err != nil {
		slog.Warn("reservation_status", "reservation", name, "err", err)
		return
	}
	if ended {
		s.reservationHistory(ctx, done, "ended")
	}
	if len(names) > 0 {
		s.reservationHistory(ctx, done, event, names...)
	}
}
