// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// Management grants (design 2026-11-20 §7): one AccessGrant per grant,
// each created, changed and deleted on its own by an admin.

type grantView struct {
	Name            string                           `json:"name"`
	Match           string                           `json:"match"`
	Permissions     []devpodv1alpha1.GrantPermission `json:"permissions"`
	Description     string                           `json:"description,omitempty"`
	ResourceVersion string                           `json:"resourceVersion,omitempty"`
	// Problem: why it gives nothing; "" = it gives what it says
	Problem string `json:"problem"`
}

func grantViewOf(g *devpodv1alpha1.AccessGrant, problem string) grantView {
	ps := g.Spec.Permissions
	if ps == nil {
		ps = []devpodv1alpha1.GrantPermission{}
	}
	return grantView{Name: g.Name, Match: g.Spec.Match, Permissions: ps, Description: g.Spec.Description, ResourceVersion: g.ResourceVersion, Problem: problem}
}

// grantProblems: each grant's problem, by name, against the groups now.
func (s *Server) grantProblems(r *http.Request, gs []devpodv1alpha1.AccessGrant) (map[string]string, error) {
	groups, err := s.groupNames(r.Context())
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range access.CompileGrants(gs, groups).Problems() {
		out[p.Name] = p.Message
	}
	return out, nil
}

// handleListGrants: GET /api/admin/grants.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var list devpodv1alpha1.AccessGrantList
	if err := s.Reader.List(r.Context(), &list); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	problems, err := s.grantProblems(r, list.Items)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := make([]grantView, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, grantViewOf(&list.Items[i], problems[list.Items[i].Name]))
	}
	slices.SortFunc(items, func(a, b grantView) int { return strings.Compare(a.Name, b.Name) })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type grantBody struct {
	Name            string                           `json:"name"`
	Match           string                           `json:"match"`
	Permissions     []devpodv1alpha1.GrantPermission `json:"permissions"`
	Description     string                           `json:"description"`
	ResourceVersion string                           `json:"resourceVersion"`
	Problem         string                           `json:"problem"` // sent back as read; ignored
}

// validGrant checks a grant before it is written (design §7).
func (s *Server) validGrant(r *http.Request, name string, b *grantBody) (devpodv1alpha1.AccessGrantSpec, *serverErr) {
	var spec devpodv1alpha1.AccessGrantSpec
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return spec, badRequest("授权的名字 %q 不合法：只能用小写字母、数字、- 和 .，以字母或数字开头和结尾", name)
	}
	m := strings.TrimSpace(b.Match)
	if m == "" {
		return spec, badRequest("授权 %s 的条件是空的：要授给所有人，写 everyone", name)
	}
	mc, apiErr := s.matchChecker(r.Context())
	if apiErr != nil {
		return spec, apiErr
	}
	if apiErr := mc.check("授权 "+name+" 的条件", m); apiErr != nil {
		return spec, apiErr
	}
	if len(b.Permissions) == 0 {
		return spec, badRequest("授权 %s 没有授予任何权限", name)
	}
	known := []devpodv1alpha1.GrantPermission{devpodv1alpha1.PermSetReservable, devpodv1alpha1.PermViewAudit, devpodv1alpha1.PermDisconnectSessions}
	seen := map[devpodv1alpha1.GrantPermission]bool{}
	for _, p := range b.Permissions {
		switch {
		case !slices.Contains(known, p):
			return spec, badRequest("授权 %s：没有 %q 这项权限", name, p)
		case seen[p]:
			return spec, badRequest("授权 %s：%s 写了两次", name, p)
		}
		seen[p] = true
	}
	if seen[devpodv1alpha1.PermDisconnectSessions] && !seen[devpodv1alpha1.PermViewAudit] {
		return spec, badRequest("授权 %s：断开会话要和看审计一起授予", name)
	}
	return devpodv1alpha1.AccessGrantSpec{Match: m, Permissions: b.Permissions, Description: strings.TrimSpace(b.Description)}, nil
}

// handleCreateGrant: POST /api/admin/grants.
func (s *Server) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var b grantBody
	if apiErr := decodeStrict(r, &b); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	spec, apiErr := s.validGrant(r, b.Name, &b)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	g := &devpodv1alpha1.AccessGrant{ObjectMeta: metav1.ObjectMeta{Name: b.Name}, Spec: spec}
	err := s.Client.Create(r.Context(), g, client.FieldValidation("Strict"))
	if apierrors.IsAlreadyExists(err) {
		s.writeErr(w, http.StatusConflict, "CONFLICT", "a grant of that name exists", nil)
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("save the grant", err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, grantViewOf(g, ""))
}

// handleUpdateGrant: PUT /api/admin/grants/{name} — refused (409) when it
// changed since resourceVersion was read.
func (s *Server) handleUpdateGrant(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	var b grantBody
	if apiErr := decodeStrict(r, &b); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	spec, apiErr := s.validGrant(r, name, &b)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	var g devpodv1alpha1.AccessGrant
	if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: name}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such grant", nil)
			return
		}
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if b.ResourceVersion != g.ResourceVersion {
		s.writeErr(w, http.StatusConflict, "CONFLICT", "the grant was changed meanwhile: reload it", nil)
		return
	}
	g.Spec = spec
	err := s.Client.Update(r.Context(), &g, client.FieldValidation("Strict"))
	if apierrors.IsConflict(err) {
		s.writeErr(w, http.StatusConflict, "CONFLICT", "the grant was changed meanwhile: reload it", nil)
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("save the grant", err), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, grantViewOf(&g, ""))
}

// handleDeleteGrant: DELETE /api/admin/grants/{name}.
func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	err := s.Client.Delete(r.Context(), &devpodv1alpha1.AccessGrant{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("name")}})
	if apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such grant", nil)
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
