// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
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
	GPU     string `json:"gpu,omitempty"`
	Storage string `json:"storage,omitempty"`
}

type adminUser struct {
	Name        string                    `json:"name"`
	DisplayName string                    `json:"displayName,omitempty"`
	Admin       bool                      `json:"admin"`
	HasPassword bool                      `json:"hasPassword"`
	DevPods     int                       `json:"devpods"`
	Running     int                       `json:"running"`
	Usage       adminUsage                `json:"usage"`
	Quota       *devpodv1alpha1.UserQuota `json:"quota,omitempty"`
	// Quotas: per cluster, in a console of several.
	Quotas map[string]devpodv1alpha1.UserQuota `json:"quotas,omitempty"`
	// Clusters: in a console of several, the user in each cluster — what
	// is used there and the quota that holds there (quotas are counted per
	// cluster; the fields above add up every cluster).
	Clusters map[string]adminUserIn `json:"clusters,omitempty"`
	// LiveUsage is measured usage from the live layer (absent without it).
	LiveUsage *liveUsage `json:"liveUsage,omitempty"`
	// LastSession is the user's latest SSH session (absent without ClickHouse).
	LastSession *time.Time `json:"lastSession,omitempty"`
	// Source: oidc (GitLab login), password (built-in), ldap (neither:
	// keys and identity come from LDAP or kubectl).
	Source       string `json:"source"`
	CustomPolicy bool   `json:"customPolicy"`
	Pubkeys      int    `json:"pubkeys"`
}

func userSource(u *devpodv1alpha1.User) string {
	switch {
	case u.Spec.OIDCSubject != "":
		return "oidc"
	case u.Spec.PasswordHash != "":
		return "password"
	default:
		return "ldap"
	}
}

// adminUserIn is a user in one cluster.
type adminUserIn struct {
	DevPods int        `json:"devpods"`
	Running int        `json:"running"`
	Usage   adminUsage `json:"usage"`
	// Quota: the one that holds (the user's own over the cluster's default).
	Quota devpodv1alpha1.UserQuota `json:"quota"`
	// Default: the cluster's default quota.
	Default devpodv1alpha1.UserQuota `json:"default"`
}

// usageAgg adds up DevPods the way quotas count them: compute over
// running ones, storage over all.
type usageAgg struct {
	total, running int
	compute        corev1.ResourceList
	storage        resource.Quantity
}

func (a *usageAgg) add(dp *devpodv1alpha1.DevPod) {
	if a.compute == nil {
		a.compute = corev1.ResourceList{}
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

func (a *usageAgg) usage() adminUsage {
	var u adminUsage
	if c := a.compute.Cpu(); !c.IsZero() {
		u.CPU = c.String()
	}
	if m := a.compute.Memory(); !m.IsZero() {
		u.Memory = m.String()
	}
	if n := gpures.Count(a.compute); n > 0 {
		u.GPU = strconv.Itoa(n)
	}
	if !a.storage.IsZero() {
		u.Storage = a.storage.String()
	}
	return u
}

type liveUsage struct {
	CPU            float64 `json:"cpu"`
	Mem            float64 `json:"mem"`
	GPU            int     `json:"gpu"`
	DevPodsRunning int     `json:"devpodsRunning"`
}

// lastSessionLookback bounds the last-session query.
const lastSessionLookback = 90 * 24 * time.Hour

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var users devpodv1alpha1.UserList
	if err := s.Client.List(r.Context(), &users); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	dps, err := s.devPods(r, "") // of every cluster
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	// per owner over every cluster, and per owner in each cluster
	byOwner := map[string]*usageAgg{}
	byOwnerIn := map[string]map[string]*usageAgg{}
	for i := range dps {
		dp := &dps[i].DevPod
		a := byOwner[dp.Spec.Owner]
		if a == nil {
			a = &usageAgg{}
			byOwner[dp.Spec.Owner] = a
		}
		a.add(dp)
		if s.Clusters != nil {
			if byOwnerIn[dp.Spec.Owner] == nil {
				byOwnerIn[dp.Spec.Owner] = map[string]*usageAgg{}
			}
			in := byOwnerIn[dp.Spec.Owner][dps[i].Cluster]
			if in == nil {
				in = &usageAgg{}
				byOwnerIn[dp.Spec.Owner][dps[i].Cluster] = in
			}
			in.add(dp)
		}
	}
	// each cluster's default quota, once
	var clusterList []*clusters.ClusterClients
	defaults := map[string]devpodv1alpha1.UserQuota{}
	if s.Clusters != nil {
		clusterList = s.ks(r)
		for _, cc := range clusterList {
			defaults[cc.Name] = s.clusterQuota(r.Context(), cc)
		}
	}
	live := map[string]liveUsage{}
	if s.Live != nil && s.Live.Snapshot != nil {
		if snap, ok := s.Live.Snapshot(r.Context()); ok {
			for _, u := range snap.Users {
				live[u.User] = liveUsage{CPU: u.CPU, Mem: u.Mem, GPU: u.GPU, DevPodsRunning: u.Running}
			}
		}
	}
	var last map[string]time.Time
	if s.Audit != nil {
		if ts, ok := s.Audit.Store.(TelemetryStore); ok {
			last, _ = ts.LastSessionByUser(r.Context(), time.Now().Add(-lastSessionLookback)) // optional: omitted on error
		}
	}
	items := make([]adminUser, 0, len(users.Items))
	for _, u := range users.Items {
		au := adminUser{
			Name:         u.Name,
			DisplayName:  u.Spec.DisplayName,
			Admin:        s.adminFor(u.Name, &u),
			HasPassword:  u.Spec.PasswordHash != "",
			Quota:        u.Spec.Quota,
			Quotas:       u.Spec.Quotas,
			Source:       userSource(&u),
			CustomPolicy: u.Spec.AuditPolicy != nil,
			Pubkeys:      len(u.Spec.Pubkeys),
		}
		if lu, ok := live[u.Name]; ok {
			au.LiveUsage = &lu
		}
		if t, ok := last[u.Name]; ok {
			t = t.UTC()
			au.LastSession = &t
		}
		if a := byOwner[u.Name]; a != nil {
			au.DevPods, au.Running, au.Usage = a.total, a.running, a.usage()
		}
		if len(clusterList) > 0 {
			au.Clusters = map[string]adminUserIn{}
			for _, cc := range clusterList {
				in := adminUserIn{Quota: EffectiveQuota(&u, cc.Name, defaults[cc.Name]), Default: defaults[cc.Name]}
				if a := byOwnerIn[u.Name][cc.Name]; a != nil {
					in.DevPods, in.Running, in.Usage = a.total, a.running, a.usage()
				}
				au.Clusters[cc.Name] = in
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
			// Admin is never set at creation; PATCH {"admin":true} grants it.
		},
	}
	if err := s.Client.Create(r.Context(), u); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeErr(w, http.StatusConflict, "ALREADY_EXISTS", "user already exists", nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("create", err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, adminUser{Name: u.Name, DisplayName: u.Spec.DisplayName, HasPassword: true})
}

func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var req struct {
		Password    *string     `json:"password,omitempty"`
		DisplayName *string     `json:"displayName,omitempty"`
		Quota       *quotaPatch `json:"quota,omitempty"`
		// Quotas: per cluster, in a console of several; an empty entry
		// removes that cluster's.
		Quotas map[string]*quotaPatch `json:"quotas,omitempty"`
		Admin  *bool                  `json:"admin,omitempty"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	// validate and prepare first; then apply on a fresh read (the cache
	// can trail a user created a moment ago) with conflict retries
	var hash string
	if req.Password != nil {
		if !s.PasswordAuth {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "password login is disabled", nil)
			return
		}
		h, err := HashPassword(*req.Password, s.PasswordMinLength)
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
		hash = h
	}
	if req.Admin != nil && !*req.Admin && name == sess.User {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "you cannot remove your own admin flag", nil)
		return
	}
	var quota *devpodv1alpha1.UserQuota
	if req.Quota != nil {
		q, err := req.Quota.build()
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
		quota = q
	}
	quotas := map[string]*devpodv1alpha1.UserQuota{}
	if len(req.Quotas) > 0 {
		known := s.clusterNames(r.Context())
		for cluster, p := range req.Quotas {
			if s.Clusters == nil || !slices.Contains(known, cluster) {
				s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "no cluster named "+cluster, nil)
				return
			}
			if p == nil {
				p = &quotaPatch{}
			}
			q, err := p.build()
			if err != nil {
				s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", cluster+": "+err.Error(), nil)
				return
			}
			quotas[cluster] = q
		}
	}
	err := s.updateUser(r.Context(), name, func(u *devpodv1alpha1.User) error {
		for cluster, q := range quotas {
			if q == nil {
				delete(u.Spec.Quotas, cluster)
				continue
			}
			if u.Spec.Quotas == nil {
				u.Spec.Quotas = map[string]devpodv1alpha1.UserQuota{}
			}
			u.Spec.Quotas[cluster] = *q
		}
		if req.Password != nil {
			u.Spec.PasswordHash = hash
		}
		if req.DisplayName != nil {
			u.Spec.DisplayName = *req.DisplayName
		}
		if req.Admin != nil {
			u.Spec.Admin = *req.Admin
		}
		if req.Quota != nil {
			u.Spec.Quota = quota.DeepCopy() // nil = clear → global defaults apply
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "user not found", nil)
		return
	}
	if err != nil {
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
	GPU        string `json:"gpu,omitempty"`
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
	for name, val := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    p.CPU,
		corev1.ResourceMemory: p.Memory,
		gpures.Total:          p.GPU, // GPUs of every type together
	} {
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
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("delete", err), err.Error())
		return
	}
	if err := s.dropUserConfigs(r.Context(), u.Name); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "the user is deleted; their saved configs could not all be removed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
