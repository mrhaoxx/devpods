// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

type usageInfo struct {
	DevPods int               `json:"devpods"`
	Running int               `json:"running"`
	Compute map[string]string `json:"compute"`
	Storage string            `json:"storage"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var u devpodv1alpha1.User
	userPtr := &u
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		userPtr = nil
	}
	owned, err := s.ownedDevPods(r, sess.User)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}

	usage := usageInfo{DevPods: len(owned), Compute: map[string]string{}}
	compute := corev1.ResourceList{}
	storage := resource.Quantity{}
	for _, dp := range owned {
		if dp.Spec.Persistence != nil {
			storage.Add(dp.Spec.Persistence.Size)
		}
		if !dp.Spec.Running || dp.Spec.Pod == nil {
			continue
		}
		usage.Running++
		for name, qty := range PodLimits(&dp.Spec.Pod.Spec) {
			cur := compute[name]
			cur.Add(qty)
			compute[name] = cur
		}
	}
	for name, qty := range compute {
		usage.Compute[string(name)] = qty.String()
	}
	usage.Storage = storage.String()

	s.writeJSON(w, http.StatusOK, map[string]any{
		"user":       sess.User,
		"admin":      sess.Admin,
		"nameBudget": NameBudget(sess.User),
		"quota":      EffectiveQuota(userPtr, s.DefaultQuota),
		"usage":      usage,
		"features": map[string]any{
			"pubkeySelfService": s.PubkeySelfService,
			"kore":              s.KoreEnabled,
		},
		"ssh": map[string]any{"host": s.SSHHost, "port": s.SSHPort},
	})
}

func (s *Server) handleGetPubkeys(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var u devpodv1alpha1.User
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil && !apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"pubkeys": u.Spec.Pubkeys})
}

func (s *Server) handlePutPubkeys(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	if !s.PubkeySelfService {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN",
			"pubkey self-service is disabled on this deployment (keys are managed externally)", nil)
		return
	}
	var req struct {
		Pubkeys []string `json:"pubkeys"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	for i, k := range req.Pubkeys {
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k)); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST",
				fmt.Sprintf("pubkey #%d is not a valid OpenSSH authorized key", i+1), err.Error())
			return
		}
	}
	var u devpodv1alpha1.User
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	u.Spec.Pubkeys = req.Pubkeys
	if err := s.Client.Update(r.Context(), &u); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "update rejected", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"pubkeys": u.Spec.Pubkeys})
}
