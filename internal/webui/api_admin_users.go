// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"net/http"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// requireAdmin authenticates and enforces the admin bit. Admin user
// management is meaningless without password auth, so it 403s when
// password auth is off.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (Session, bool) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return Session{}, false
	}
	if !s.PasswordAuth {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "password login is disabled", nil)
		return Session{}, false
	}
	if !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "admin only", nil)
		return Session{}, false
	}
	return sess, true
}

type adminUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Admin       bool   `json:"admin"`
	HasPassword bool   `json:"hasPassword"`
	DevPods     int    `json:"devpods"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var users devpodv1alpha1.UserList
	if err := s.Client.List(r.Context(), &users); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	var dps devpodv1alpha1.DevPodList
	if err := s.Client.List(r.Context(), &dps, client.InNamespace(s.NS)); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	counts := map[string]int{}
	for _, dp := range dps.Items {
		counts[dp.Spec.Owner]++
	}
	items := make([]adminUser, 0, len(users.Items))
	for _, u := range users.Items {
		items = append(items, adminUser{
			Name:        u.Name,
			DisplayName: u.Spec.DisplayName,
			Admin:       u.Spec.Admin,
			HasPassword: u.Spec.PasswordHash != "",
			DevPods:     counts[u.Name],
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req struct {
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	// Username = DevPod owner name; validate shape + name budget (no prefix).
	if _, err := MapUsername("", req.Username); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	hash, err := HashPassword(req.Password, s.PasswordMinLength)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: req.Username},
		Spec: devpodv1alpha1.UserSpec{
			DisplayName:  req.DisplayName,
			PasswordHash: hash,
			// Admin is deliberately NOT set — it is kubectl-managed only.
		},
	}
	if err := s.Client.Create(r.Context(), u); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeErr(w, http.StatusConflict, "ALREADY_EXISTS", "user already exists", nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "create rejected", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, adminUser{Name: u.Name, DisplayName: u.Spec.DisplayName, HasPassword: true})
}

func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	var req struct {
		Password    *string `json:"password,omitempty"`
		DisplayName *string `json:"displayName,omitempty"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	var u devpodv1alpha1.User
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "user not found", nil)
			return
		}
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if req.Password != nil {
		hash, err := HashPassword(*req.Password, s.PasswordMinLength)
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
		u.Spec.PasswordHash = hash
	}
	if req.DisplayName != nil {
		u.Spec.DisplayName = *req.DisplayName
	}
	if err := s.Client.Update(r.Context(), &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("name")}}
	if err := s.Client.Delete(r.Context(), u); err != nil && !apierrors.IsNotFound(err) {
		// The gateway's finalizer blocks deletion while the user owns
		// DevPods — surface that verbatim.
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "delete rejected", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
