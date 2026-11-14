// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Reservations are the console's: NodeReservation objects in its own
// cluster (where users and configs are too), each naming the cluster of
// its node. The reservation rules keep working on Reservations (the
// type they were written for); these convert.

// resCluster: the cluster the request is about, as reservations name it.
func (s *Server) resCluster(ctx context.Context) string { return s.kc(ctx).Name }

// nodeReservations: every reservation the console keeps (of all its
// clusters), not being deleted.
func (s *Server) nodeReservations(ctx context.Context) ([]devpodv1alpha1.NodeReservation, error) {
	var list devpodv1alpha1.NodeReservationList
	if err := s.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp == nil {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// reservationsOf: one cluster's reservations, as Reservations.
func reservationsOf(all []devpodv1alpha1.NodeReservation, cluster string) []devpodv1alpha1.Reservation {
	var out []devpodv1alpha1.Reservation
	for i := range all {
		if all[i].Spec.Cluster == cluster {
			out = append(out, all[i].Reservation())
		}
	}
	return out
}

// createReservation keeps res as the request's cluster's (its history
// starts once it has won against those made at the same time: reserve).
func (s *Server) createReservation(ctx context.Context, res *devpodv1alpha1.Reservation) error {
	nr := &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: res.Name}, Spec: devpodv1alpha1.NodeReservationSpec{Cluster: s.resCluster(ctx), ReservationSpec: res.Spec}}
	return s.Client.Create(ctx, nr)
}

func (s *Server) deleteReservation(ctx context.Context, name string) error {
	err := s.Client.Delete(ctx, &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// changeReservation applies fn to the request's cluster's reservation
// name (read afresh on each conflict); fn returns false to leave it.
func (s *Server) changeReservation(ctx context.Context, name string, fn func(*devpodv1alpha1.ReservationSpec) bool) error {
	cluster := s.resCluster(ctx)
	var done *devpodv1alpha1.NodeReservation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var nr devpodv1alpha1.NodeReservation
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &nr); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if nr.Spec.Cluster != cluster {
			return fmt.Errorf("reservation %s is of cluster %q", name, nr.Spec.Cluster)
		}
		if !fn(&nr.Spec.ReservationSpec) {
			return nil
		}
		if err := s.Client.Update(ctx, &nr); err != nil {
			return err
		}
		done = &nr
		return nil
	})
	if err == nil && done != nil {
		s.reservationHistory(ctx, done, "changed")
	}
	return err
}

// keptFor: how long an ended or cancelled reservation is kept as an
// object (the 预约管理 page lists them); its history stays in the audit
// store, when there is one.
const keptFor = 90 * 24 * time.Hour

// reservationHistory writes what happened to nr into the audit store,
// when there is one (best effort: the reservation itself is the record
// that counts).
func (s *Server) reservationHistory(ctx context.Context, nr *devpodv1alpha1.NodeReservation, event string, detail ...string) {
	if s.Audit == nil || s.Audit.Store == nil {
		return
	}
	row := store.ReservationEventRow{
		TS: time.Now().UTC(), Cluster: nr.Spec.Cluster, ID: nr.Name, Node: nr.Spec.Node, User: nr.Spec.User,
		Start: nr.Spec.Start.Time.UTC(), End: nr.Spec.End.Time.UTC(), Extends: nr.Spec.Extends, Event: event, Detail: strings.Join(detail, " "),
	}
	if nr.Spec.Cancelled != nil {
		row.CancelledBy = nr.Spec.Cancelled.By
	}
	if err := s.Audit.Store.InsertReservationEvent(context.WithoutCancel(ctx), row); err != nil {
		slog.Warn("reservation_history", "reservation", nr.Name, "event", event, "err", err)
	}
}
