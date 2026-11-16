// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// The console carries the reservations out itself, on whichever replica
// leads: when a reservation ends (its time is up, or it is released), its
// holder's DevPods on the machine that only it let run are hibernated —
// those whose owner the access rules ask for a reservation there and who
// holds no other reservation covering now. Nothing is done when one
// starts: reservations are permits, not holds.

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
		s.endIn(ctx, cluster, rs, now)
	}
}

// endIn carries out the end of cluster's reservations rs.
func (s *Server) endIn(ctx context.Context, cluster string, rs []*devpodv1alpha1.NodeReservation, now time.Time) {
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
		if nr.Spec.Start.Time.Before(overAt(nr)) { // it was in force at all
			var node corev1.Node
			_ = cc.Client.Get(ctx, types.NamespacedName{Name: nr.Spec.Node}, &node)
			for _, dp := range on[nr.Spec.Node] {
				if dp.Spec.Owner != nr.Spec.User || s.activeReservation(ctx, cluster, nr.Spec.Node, nr.Spec.User, now) != nil {
					continue
				}
				d, admin, err := s.decide(ctx, dp.Spec.Owner, cluster, access.Node{Name: nr.Spec.Node, Labels: node.Labels})
				if err != nil || admin || !d.NeedsReservation {
					continue // the rules let them in without one
				}
				if s.hibernateFor(ctx, cc, dp, "reservation-end/"+nr.Name) {
					names = append(names, dp.Name)
				}
			}
		}
		s.setReservationStatus(ctx, nr.Name, names, func(st *devpodv1alpha1.NodeReservationStatus) {
			st.Ended = true
			st.HibernatedAtEnd = append(st.HibernatedAtEnd, names...)
		})
	}
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

// setReservationStatus changes a reservation's status and records its end
// (and the DevPods hibernated) in its history.
func (s *Server) setReservationStatus(ctx context.Context, name string, names []string, fn func(*devpodv1alpha1.NodeReservationStatus)) {
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
	s.reservationHistory(ctx, done, "ended")
	if len(names) > 0 {
		s.reservationHistory(ctx, done, "hibernated", names...)
	}
}
