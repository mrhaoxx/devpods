// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// The console carries the reservations out itself, on whichever replica
// leads: while a node is held, every DevPod of another user found on it
// (its Pod there, or pinned there) is hibernated — at the start, and any
// that comes later; the API already refuses to start or move one there
// (pinErr). The notices (the node is about to be held, the hold is about
// to end) are worked out when a DevPod is shown, from the reservations.

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
// in each cluster, other users' DevPods on a held node are hibernated.
func (s *Server) EnforceReservations(ctx context.Context) {
	all, err := s.nodeReservations(ctx)
	if err != nil {
		slog.Warn("reservations", "err", err)
		return
	}
	now := s.Reservations.now()
	byCluster := map[string]bool{}
	for i := range all {
		nr := &all[i]
		over := nr.Spec.Cancelled != nil && nr.Spec.Cancelled.At.Time.Before(nr.Spec.End.Time) || !now.Before(nr.Spec.End.Time)
		end := nr.Spec.End.Time
		if nr.Spec.Cancelled != nil && nr.Spec.Cancelled.At.Time.Before(end) {
			end = nr.Spec.Cancelled.At.Time
		}
		if over && now.Sub(end) > keptFor {
			if err := s.deleteReservation(ctx, nr.Name); err != nil {
				slog.Warn("reservation_prune", "reservation", nr.Name, "err", err)
			}
			continue
		}
		byCluster[nr.Spec.Cluster] = true
	}
	for cluster := range byCluster {
		s.enforceIn(ctx, cluster, all, now)
	}
}

// enforceIn carries out cluster's reservations: on a node held now,
// other users' DevPods are hibernated; when a hold ends (its time is up,
// or it is cancelled once started) the holder's own DevPods on the node
// are — unless the same user's next reservation follows on at once.
func (s *Server) enforceIn(ctx context.Context, cluster string, all []devpodv1alpha1.NodeReservation, now time.Time) {
	cc := s.clientsOf(cluster)
	if cc == nil {
		return // away: done when it is back
	}
	rs := reservationsOf(all, cluster)
	eff := effectiveReservations(rs)
	at := metav1.NewTime(now)
	held := map[string]*devpodv1alpha1.Reservation{}
	var ended []*devpodv1alpha1.Reservation
	for i := range eff {
		r := &eff[i]
		switch {
		case r.HeldAt(at):
			held[r.Spec.Node] = r
		case r.Status.Phase == devpodv1alpha1.ReservationEnded || r.Status.Phase == devpodv1alpha1.ReservationCancelled:
		case r.Spec.Cancelled != nil || !now.Before(r.Spec.End.Time):
			ended = append(ended, r)
		}
	}
	if len(held) == 0 && len(ended) == 0 {
		return
	}
	on, err := s.devPodsByNode(ctx, cc)
	if err != nil {
		slog.Warn("reservations", "cluster", cluster, "err", err)
		return
	}
	for node, h := range held {
		var names []string
		for _, dp := range on[node] {
			if dp.Spec.Owner != h.Spec.User && s.hibernateFor(ctx, cc, dp, "reservation/"+h.Name) {
				names = append(names, dp.Name)
			}
		}
		if len(names) > 0 || h.Status.Phase != devpodv1alpha1.ReservationActive {
			s.setReservationStatus(ctx, h.Name, "hibernated", names, func(st *devpodv1alpha1.ReservationStatus) {
				st.Phase = devpodv1alpha1.ReservationActive
				for _, n := range names {
					if !slices.Contains(st.ClearedDevPods, n) {
						st.ClearedDevPods = append(st.ClearedDevPods, n)
					}
				}
			})
		}
	}
	for _, r := range ended {
		over := r.Spec.End.Time
		if r.Spec.Cancelled != nil && r.Spec.Cancelled.At.Time.Before(over) {
			over = r.Spec.Cancelled.At.Time
		}
		var names []string
		// it held the node only if it started before it was over; its
		// holder's time goes on when their next reservation follows
		if r.Spec.Start.Time.Before(over) && !(r.Spec.Cancelled == nil && followedOn(r, rs)) && held[r.Spec.Node] == nil {
			for _, dp := range on[r.Spec.Node] {
				if dp.Spec.Owner == r.Spec.User && s.hibernateFor(ctx, cc, dp, "reservation-end/"+r.Name) {
					names = append(names, dp.Name)
				}
			}
		}
		phase := devpodv1alpha1.ReservationEnded
		if r.Spec.Cancelled != nil {
			phase = devpodv1alpha1.ReservationCancelled
		}
		s.setReservationStatus(ctx, r.Name, "ended", names, func(st *devpodv1alpha1.ReservationStatus) {
			st.Phase = phase
			st.HibernatedAtEnd = append(st.HibernatedAtEnd, names...)
		})
	}
}

// followedOn reports whether the same user holds the node from r's end on.
func followedOn(r *devpodv1alpha1.Reservation, all []devpodv1alpha1.Reservation) bool {
	for i := range all {
		o := &all[i]
		if o.Name != r.Name && o.Spec.Node == r.Spec.Node && o.Spec.User == r.Spec.User && o.Spec.Cancelled == nil && o.Spec.Start.Time.Equal(r.Spec.End.Time) {
			return true
		}
	}
	return false
}

// devPodsByNode: a cluster's running DevPods by the node they are on —
// where their Pod is, or, not placed yet, where they are pinned.
func (s *Server) devPodsByNode(ctx context.Context, cc *clusters.ClusterClients) (map[string][]*devpodv1alpha1.DevPod, error) {
	var dps devpodv1alpha1.DevPodList
	if err := cc.Client.List(ctx, &dps, client.InNamespace(s.NS)); err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := cc.Client.List(ctx, &pods, client.InNamespace(s.NS)); err != nil {
		return nil, err
	}
	podNode := map[string]string{}
	for i := range pods.Items {
		podNode[pods.Items[i].Name] = pods.Items[i].Spec.NodeName
	}
	out := map[string][]*devpodv1alpha1.DevPod{}
	for i := range dps.Items {
		dp := &dps.Items[i]
		if !dp.Spec.Running || !dp.DeletionTimestamp.IsZero() {
			continue
		}
		node := podNode[render.PodName(dp)]
		if node == "" {
			node = pinnedNode(dp)
		}
		if node != "" {
			out[node] = append(out[node], dp)
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
	if err := cc.Client.Patch(ctx, dp, client.MergeFrom(before)); err != nil {
		slog.Warn("reservation_hibernate", "cluster", cc.Name, "devpod", dp.Name, "by", by, "err", err)
		return false
	}
	slog.Info("reservation_hibernate", "cluster", cc.Name, "devpod", dp.Name, "owner", dp.Spec.Owner, "by", by)
	return true
}

// setReservationStatus changes a reservation's status; when it hibernated
// DevPods (names) that goes into its history as event.
func (s *Server) setReservationStatus(ctx context.Context, name, event string, names []string, fn func(*devpodv1alpha1.ReservationStatus)) {
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
	if len(names) > 0 {
		s.reservationHistory(ctx, done, event, names...)
	}
}
