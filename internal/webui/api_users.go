// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
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
	hasPassword := false
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		userPtr = nil
	} else {
		hasPassword = u.Spec.PasswordHash != ""
	}
	owned, err := s.devPods(r, sess.User)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	usageOf := func(cluster string, all bool) usageInfo {
		usage := usageInfo{Compute: map[string]string{}}
		compute := corev1.ResourceList{}
		storage := resource.Quantity{}
		for _, dp := range owned {
			if !all && dp.Cluster != cluster {
				continue
			}
			usage.DevPods++
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
		// GPUs of every kind together, as a quota's total counts them
		if n := gpures.Count(compute); n > 0 {
			usage.Compute[string(gpures.Total)] = strconv.Itoa(n)
		}
		usage.Storage = storage.String()
		return usage
	}
	usage := usageOf("", true)
	// a quota is a cluster's: with several, each has its own count and cap
	var perCluster map[string]any
	if s.Clusters != nil {
		perCluster = map[string]any{}
		for _, cc := range s.ks(r) {
			of := map[string]any{
				"usage": usageOf(cc.Name, false),
				"quota": EffectiveQuota(userPtr, cc.Name, s.clusterQuota(r.Context(), cc)),
			}
			// where to connect for this cluster's DevPods, when a gateway says
			if ssh := s.sshOf(r.Context(), cc.Name); ssh != nil {
				of["ssh"] = ssh
			}
			perCluster[cc.Name] = of
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"user":       sess.User,
		"admin":      sess.Admin,
		"nameBudget": NameBudget(sess.User),
		"quota":      EffectiveQuota(userPtr, "", s.DefaultQuota),
		"usage":      usage,
		"clusters":   perCluster,
		// the kinds of GPU here, the default first; quotas limit their sum as devpod.io/gpu
		"gpuTypes": gpures.Types(),
		"features": map[string]any{
			"pubkeySelfService": s.PubkeySelfService,
			"kore":              s.KoreEnabled,
			"clusters":          s.Clusters != nil,
			"passwordAuth":      s.PasswordAuth,
			"audit":             s.Audit != nil,
			"telemetry":         s.Live != nil && s.Live.Hub != nil,
			"webshell":          s.Webshell != nil,
			"httpProxy":         s.HTTPProxy != nil,
			"snapshots":         s.Snapshots != nil,
			// a platform registry to save into without bringing credentials
			"snapshotDefaultRegistry": s.Snapshots != nil && s.Snapshots.ImageTemplate != "",
			// node reservations arrive with the reservation backend
			"reservations": s.Reservations != nil,
		},
		"hasPassword": hasPassword,
		"ssh":         map[string]any{"host": s.SSHHost, "port": s.SSHPort, "loginSuffix": s.SSHLoginSuffix},
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
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("update", err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"pubkeys": u.Spec.Pubkeys})
}
