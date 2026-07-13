// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// requireAdmin authenticates and enforces the admin bit. Reading and
// quota management work on any auth method; password-specific actions
// (create user, reset password) gate on PasswordAuth themselves.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (Session, bool) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return Session{}, false
	}
	if !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "admin only", nil)
		return Session{}, false
	}
	return sess, true
}

type adminUsage struct {
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Storage string `json:"storage,omitempty"`
}

type adminUser struct {
	Name        string                     `json:"name"`
	DisplayName string                     `json:"displayName,omitempty"`
	Admin       bool                       `json:"admin"`
	HasPassword bool                       `json:"hasPassword"`
	DevPods     int                        `json:"devpods"`
	Running     int                        `json:"running"`
	Usage       adminUsage                 `json:"usage"`
	Quota       *devpodv1alpha1.UserQuota  `json:"quota,omitempty"`
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
	// Per-owner aggregates mirror the quota accounting: compute over
	// running DevPods, storage over all.
	type agg struct {
		total, running int
		compute        corev1.ResourceList
		storage        resource.Quantity
	}
	byOwner := map[string]*agg{}
	for _, dp := range dps.Items {
		a := byOwner[dp.Spec.Owner]
		if a == nil {
			a = &agg{compute: corev1.ResourceList{}}
			byOwner[dp.Spec.Owner] = a
		}
		a.total++
		if dp.Spec.Persistence != nil {
			a.storage.Add(dp.Spec.Persistence.Size)
		}
		if dp.Spec.Running && dp.Spec.Pod != nil {
			a.running++
			for name, qty := range PodLimits(&dp.Spec.Pod.Spec) {
				cur := a.compute[name]
				cur.Add(qty)
				a.compute[name] = cur
			}
		}
	}
	items := make([]adminUser, 0, len(users.Items))
	for _, u := range users.Items {
		au := adminUser{
			Name:        u.Name,
			DisplayName: u.Spec.DisplayName,
			Admin:       u.Spec.Admin,
			HasPassword: u.Spec.PasswordHash != "",
			Quota:       u.Spec.Quota,
		}
		if a := byOwner[u.Name]; a != nil {
			au.DevPods = a.total
			au.Running = a.running
			if c := a.compute.Cpu(); !c.IsZero() {
				au.Usage.CPU = c.String()
			}
			if m := a.compute.Memory(); !m.IsZero() {
				au.Usage.Memory = m.String()
			}
			if !a.storage.IsZero() {
				au.Usage.Storage = a.storage.String()
			}
		}
		items = append(items, au)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items, "defaultQuota": s.DefaultQuota})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if !s.PasswordAuth {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "creating users requires password login to be enabled", nil)
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
		Password    *string     `json:"password,omitempty"`
		DisplayName *string     `json:"displayName,omitempty"`
		Quota       *quotaPatch `json:"quota,omitempty"`
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
		if !s.PasswordAuth {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "password login is disabled", nil)
			return
		}
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
	if req.Quota != nil {
		q, err := req.Quota.build()
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
		u.Spec.Quota = q // nil = clear → global defaults apply
	}
	if err := s.Client.Update(r.Context(), &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// quotaPatch is the UI's editable quota form. All fields optional;
// an all-empty patch clears the quota (falls back to global defaults).
type quotaPatch struct {
	MaxDevPods *int32 `json:"maxDevPods,omitempty"`
	CPU        string `json:"cpu,omitempty"`
	Memory     string `json:"memory,omitempty"`
	Storage    string `json:"storage,omitempty"`
}

func (p *quotaPatch) build() (*devpodv1alpha1.UserQuota, error) {
	q := &devpodv1alpha1.UserQuota{}
	empty := true
	if p.MaxDevPods != nil {
		q.MaxDevPods = p.MaxDevPods
		empty = false
	}
	compute := corev1.ResourceList{}
	for name, val := range map[corev1.ResourceName]string{corev1.ResourceCPU: p.CPU, corev1.ResourceMemory: p.Memory} {
		if val == "" {
			continue
		}
		qty, err := resource.ParseQuantity(val)
		if err != nil {
			return nil, fmt.Errorf("invalid %s quota %q", name, val)
		}
		compute[name] = qty
		empty = false
	}
	if len(compute) > 0 {
		q.Compute = compute
	}
	if p.Storage != "" {
		qty, err := resource.ParseQuantity(p.Storage)
		if err != nil {
			return nil, fmt.Errorf("invalid storage quota %q", p.Storage)
		}
		q.Storage = &qty
		empty = false
	}
	if empty {
		return nil, nil // clear
	}
	return q, nil
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
