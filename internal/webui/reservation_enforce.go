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
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

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
	byNode := map[string][]*devpodv1alpha1.NodeReservation{}
	for i := range all {
		if nr := &all[i]; nr.Spec.Cluster == cluster {
			byNode[nr.Spec.Node] = append(byNode[nr.Spec.Node], nr)
		}
	}
	on, err := s.devPodsByNode(ctx, cc)
	if err != nil {
		slog.Warn("reservation_clear", "cluster", cluster, "err", err)
		return
	}
	type mark struct {
		at    time.Time
		forID string
	}
	want := map[string]mark{} // by DevPod name
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
		for _, t := range moments {
			id := clearedFor(rs, t)
			for _, p := range toClear(c, rs, left, t) {
				want[p.DP.Name] = mark{at: later(t, now.Add(notice)), forID: id}
				left = slices.DeleteFunc(left, func(q placed) bool { return q.DP.Name == p.DP.Name })
			}
		}
	}
	cleared := map[string][]string{} // by reservation
	stillMarked := false
	for _, ps := range on {
		for _, p := range ps {
			dp := p.DP
			atText, has := dp.Annotations[devpodv1alpha1.ClearAtAnnotation]
			forID := dp.Annotations[devpodv1alpha1.ClearForAnnotation]
			m, listed := want[dp.Name]
			switch {
			case !listed:
				if has || forID != "" {
					s.unmarkClearing(ctx, cc, dp)
				}
				continue
			case has && forID == m.forID:
				if at, err := time.Parse(time.RFC3339, atText); err == nil {
					if now.Before(at) {
						stillMarked = true
					} else if s.hibernateFor(ctx, cc, dp, "reservation/"+m.forID) {
						cleared[m.forID] = append(cleared[m.forID], dp.Name)
					} else {
						stillMarked = true
					}
					continue
				}
			}
			// not marked, or marked for another reservation: a fresh notice
			s.markClearing(ctx, cc, dp, m.at, m.forID)
			stillMarked = true
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
				if dp.Spec.Owner != nr.Spec.User || covered(all, cluster, nr.Spec.Node, nr.Spec.User, now) {
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
	var dps devpodv1alpha1.DevPodList
	if err := cc.Client.List(ctx, &dps, client.InNamespace(s.NS)); err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := cc.Client.List(ctx, &pods, client.InNamespace(s.NS)); err != nil {
		return nil, err
	}
	podOf := map[string]*corev1.Pod{}
	for i := range pods.Items {
		podOf[pods.Items[i].Name] = &pods.Items[i]
	}
	out := map[string][]placed{}
	for i := range dps.Items {
		dp := &dps.Items[i]
		if !dp.Spec.Running || !dp.DeletionTimestamp.IsZero() {
			continue
		}
		p := placed{DP: dp, Since: dp.CreationTimestamp.Time}
		if dp.Spec.Pod != nil {
			p.Ask = askOfPod(&dp.Spec.Pod.Spec)
		}
		node := ""
		if pod := podOf[render.PodName(dp)]; pod != nil {
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
	return out, nil
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
