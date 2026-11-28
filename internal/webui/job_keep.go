// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// A queue job lives in the cluster while it runs; once it has ended (its
// ranks cleaned up, their output kept) it is kept whole in the audit
// store — to be listed, shown and submitted again from there — and taken
// out of the cluster. With no audit store it stays until KeepEnded.

// keepJob keeps the ended job j in the audit store and, kept, deletes it
// from the cluster. Not kept (no store, the store away), it stays: the
// next round tries again.
func (s *Server) keepJob(ctx context.Context, j *devpodv1alpha1.Job) {
	if s.Audit == nil || s.Audit.Store == nil {
		return
	}
	row, err := jobRowOf(j)
	if err != nil {
		slog.Warn("queue_keep_job", "job", j.Name, "err", err)
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.Audit.Store.InsertJob(wctx, row, string(j.UID)); err != nil {
		slog.Warn("queue_keep_job", "job", j.Name, "err", err)
		return
	}
	if err := s.Client.Delete(ctx, j); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("queue_release_job", "job", j.Name, "err", err)
		return
	}
	slog.Info("queue_job_kept", "job", j.Name, "owner", j.Spec.Owner, "phase", j.Status.Phase)
}

// jobRowOf: j as the audit store keeps it.
func jobRowOf(j *devpodv1alpha1.Job) (store.JobRow, error) {
	spec, err := json.Marshal(j.Spec)
	if err != nil {
		return store.JobRow{}, err
	}
	status, err := json.Marshal(j.Status)
	if err != nil {
		return store.JobRow{}, err
	}
	finished := time.Now().UTC()
	if j.Status.FinishedAt != nil {
		finished = j.Status.FinishedAt.UTC()
	}
	return store.JobRow{ID: j.Name, UID: string(j.UID), Cluster: j.Spec.Cluster, Owner: j.Spec.Owner, Title: j.Spec.Title,
		Phase: string(j.Status.Phase), SubmittedAt: j.CreationTimestamp.UTC(), FinishedAt: finished, Spec: string(spec), Status: string(status)}, nil
}

// jobOfRow: the job a kept row is, as it was in the cluster.
func jobOfRow(r *store.JobRow) (*devpodv1alpha1.Job, error) {
	j := &devpodv1alpha1.Job{ObjectMeta: metav1.ObjectMeta{Name: r.ID, UID: types.UID(r.UID), CreationTimestamp: metav1.NewTime(r.SubmittedAt)}}
	if err := json.Unmarshal([]byte(r.Spec), &j.Spec); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(r.Status), &j.Status); err != nil {
		return nil, err
	}
	return j, nil
}

// jobByID: job id — in the cluster while it runs (and a moment after), else
// as the audit store keeps it; a NotFound error when neither has it.
func (s *Server) jobByID(ctx context.Context, id string) (*devpodv1alpha1.Job, error) {
	var j devpodv1alpha1.Job
	err := s.Reader.Get(ctx, types.NamespacedName{Name: id}, &j)
	if err == nil || !apierrors.IsNotFound(err) || s.Audit == nil || s.Audit.Store == nil {
		if err != nil {
			return nil, err
		}
		return &j, nil
	}
	row, serr := s.Audit.Store.GetJob(ctx, id)
	switch {
	case serr != nil:
		return nil, serr
	case row == nil:
		return nil, err
	}
	return jobOfRow(row)
}
