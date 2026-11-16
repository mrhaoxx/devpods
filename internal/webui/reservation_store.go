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
// its node. Read afresh (not from a cache): a reservation just made
// counts at once.

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

func (s *Server) createReservation(ctx context.Context, nr *devpodv1alpha1.NodeReservation) error {
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
// name (read afresh on each conflict) and records it in its history.
func (s *Server) changeReservation(ctx context.Context, name string, fn func(*devpodv1alpha1.NodeReservationSpec) bool) (*devpodv1alpha1.NodeReservation, error) {
	cluster := s.resCluster(ctx)
	var nr devpodv1alpha1.NodeReservation
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &nr); err != nil {
			return err
		}
		if nr.Spec.Cluster != cluster {
			return fmt.Errorf("reservation %s is of cluster %q", name, nr.Spec.Cluster)
		}
		if changed = fn(&nr.Spec); !changed {
			return nil
		}
		return s.Client.Update(ctx, &nr)
	})
	if err != nil {
		return nil, err
	}
	if changed {
		s.reservationHistory(ctx, &nr, "changed")
	}
	return &nr, nil
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
		Start: nr.Spec.Start.Time.UTC(), End: nr.Spec.End.Time.UTC(), Event: event, Detail: strings.Join(detail, " "),
	}
	if nr.Spec.Cancelled != nil {
		row.CancelledBy = nr.Spec.Cancelled.By
	}
	if err := s.Audit.Store.InsertReservationEvent(context.WithoutCancel(ctx), row); err != nil {
		slog.Warn("reservation_history", "reservation", nr.Name, "event", event, "err", err)
	}
}
