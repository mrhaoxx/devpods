// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// accessView is the rules as the admin API reads them: the whole ordered
// list at once, each rule a condition and an effect in the new form
// (design 2026-11-19 §8), with the version it was read at and the rules
// that deny everyone (§5.5).
type accessView struct {
	Rules           []ruleView                  `json:"rules"`
	Default         devpodv1alpha1.AccessEffect `json:"default"`
	ResourceVersion string                      `json:"resourceVersion"`
	Problems        []access.Problem            `json:"problems"`
}

type ruleView struct {
	Match  string                      `json:"match"`
	Effect devpodv1alpha1.AccessEffect `json:"effect"`
}

// accessPut is what a save sends. A rule may still be in the old form (a
// page loaded before the console was upgraded): it is read as the
// condition it makes, so its meaning is kept. Problems, sent back as
// read, is ignored.
type accessPut struct {
	Rules           []devpodv1alpha1.AccessRule `json:"rules"`
	Default         devpodv1alpha1.AccessEffect `json:"default"`
	ResourceVersion string                      `json:"resourceVersion"`
	Problems        []access.Problem            `json:"problems,omitempty"`
}

// currentAccess reads the rules afresh (not from the cache).
func (s *Server) currentAccess(ctx context.Context) (*devpodv1alpha1.AccessPolicy, error) {
	var p devpodv1alpha1.AccessPolicy
	err := s.Reader.Get(ctx, types.NamespacedName{Name: devpodv1alpha1.AccessPolicyName}, &p)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return &p, err
}

// groupNames: every UserGroup there is, read afresh.
func (s *Server) groupNames(ctx context.Context) (map[string]bool, error) {
	var list devpodv1alpha1.UserGroupList
	if err := s.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range list.Items {
		out[list.Items[i].Name] = true
	}
	return out, nil
}

func viewOf(p *devpodv1alpha1.AccessPolicy, groups map[string]bool) accessView {
	v := accessView{Rules: []ruleView{}, Default: access.NormalizeEffect(devpodv1alpha1.DefaultAccessEffect()), Problems: []access.Problem{}}
	if p == nil {
		return v
	}
	for _, r := range p.Spec.Rules {
		v.Rules = append(v.Rules, ruleView{Match: access.MatchOf(r), Effect: access.NormalizeEffect(r.Effect)})
	}
	v.Default, v.ResourceVersion = access.NormalizeEffect(p.Spec.Default), p.ResourceVersion
	if pr := access.Compile(&p.Spec, groups).Problems(); pr != nil {
		v.Problems = pr
	}
	return v
}

// handleGetAccess: GET /api/admin/access.
func (s *Server) handleGetAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	p, err := s.currentAccess(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	groups, err := s.groupNames(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, viewOf(p, groups))
}

// handlePutAccess: PUT /api/admin/access — the whole list, refused (409)
// when the rules changed since resourceVersion was read.
func (s *Server) handlePutAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req accessPut
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	spec, apiErr := s.validateAccess(r.Context(), &req)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	conflict := func() {
		s.writeErr(w, http.StatusConflict, "CONFLICT", "the rules were changed meanwhile: reload them", nil)
	}
	cur, err := s.currentAccess(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	switch {
	case cur == nil && req.ResourceVersion != "", cur != nil && req.ResourceVersion != cur.ResourceVersion:
		conflict()
		return
	case cur == nil:
		cur = &devpodv1alpha1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.AccessPolicyName}, Spec: spec}
		// strict: a CRD that predates conditions refuses them rather than
		// dropping them (a rule without its match holds for everything)
		err = s.Client.Create(r.Context(), cur, client.FieldValidation("Strict"))
	default:
		cur.Spec = spec
		err = s.Client.Update(r.Context(), cur, client.FieldValidation("Strict"))
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		conflict()
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("save the rules", err), nil)
		return
	}
	groups, err := s.groupNames(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, viewOf(cur, groups))
}

// maxMatch is the longest condition a rule or a grant may have (the
// CRDs' maxLength).
const maxMatch = 2000

// matchChecker checks conditions against the clusters and groups there
// are (design 2026-11-19 §5.4): rules and grants alike.
type matchChecker struct{ clusters, groups map[string]bool }

func (s *Server) matchChecker(ctx context.Context) (*matchChecker, *serverErr) {
	groups, err := s.groupNames(ctx)
	if err != nil {
		return nil, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	clusters := map[string]bool{}
	var cl devpodv1alpha1.ClusterList
	if s.Reader.List(ctx, &cl) == nil {
		for i := range cl.Items {
			clusters[cl.Items[i].Name] = true
		}
	}
	return &matchChecker{clusters: clusters, groups: groups}, nil
}

// check: m as a condition — its length, its syntax, items that can match
// nothing, clusters and groups (not globs) that are not there; where
// opens the message ("第 3 条", "授权 x 的条件").
func (c *matchChecker) check(where, m string) *serverErr {
	if utf8.RuneCountInString(m) > maxMatch {
		return badRequest("%s：条件超过 %d 个字符", where, maxMatch)
	}
	e, err := access.Parse(m)
	if err != nil {
		return badRequest("%s%s", where, err.Error())
	}
	for _, f := range []access.Field{access.FieldCluster, access.FieldMachine, access.FieldUser, access.FieldGroup} {
		for _, it := range e.Items(f) {
			if p := access.ItemProblem(f, it); p != "" {
				return badRequest("%s：%s", where, p)
			}
			switch {
			case f == access.FieldCluster && !access.HasGlob(it) && !c.clusters[it]:
				return badRequest("%s：没有集群 %s", where, it)
			case f == access.FieldGroup && !access.HasGlob(it) && !c.groups[it]:
				return badRequest("%s：没有用户组 %s", where, it)
			}
		}
	}
	return nil
}

// capName: a capability as NormalizeCap names it, or "*".
var capName = regexp.MustCompile(`^([A-Z0-9_]+|\*)$`)

// validateAccess checks every rule (design 2026-11-19 §5.4) and returns
// the rules as they are kept: each its condition (old-form rules read as
// theirs) and its effect in the new form, host paths cleaned.
func (s *Server) validateAccess(ctx context.Context, v *accessPut) (devpodv1alpha1.AccessPolicySpec, *serverErr) {
	var spec devpodv1alpha1.AccessPolicySpec
	mc, apiErr := s.matchChecker(ctx)
	if apiErr != nil {
		return spec, apiErr
	}
	effect := func(where string, e devpodv1alpha1.AccessEffect) (devpodv1alpha1.AccessEffect, *serverErr) {
		for i, h := range e.HostPaths {
			if !strings.HasPrefix(h.Path, "/") {
				return e, badRequest("%s: host path %q is not absolute", where, h.Path)
			}
			e.HostPaths[i].Path = path.Clean(h.Path)
		}
		if e.Privileges != nil {
			for _, c := range e.Privileges.Capabilities {
				if !capName.MatchString(access.NormalizeCap(c)) {
					return e, badRequest("%s：capability %q 不是合法的名字（如 NET_ADMIN，或 * 表示任意）", where, c)
				}
			}
		}
		return access.NormalizeEffect(e), nil
	}
	for i := range v.Rules {
		where := fmt.Sprintf("第 %d 条", i+1)
		m := access.MatchOf(v.Rules[i])
		if apiErr := mc.check(where, m); apiErr != nil {
			return spec, apiErr
		}
		eff, apiErr := effect(where, v.Rules[i].Effect)
		if apiErr != nil {
			return spec, apiErr
		}
		spec.Rules = append(spec.Rules, devpodv1alpha1.AccessRule{Match: m, Effect: eff})
	}
	def, apiErr := effect("默认规则", v.Default)
	spec.Default = def
	return spec, apiErr
}

// handleAccessPreview: POST /api/admin/access/preview {match} — what a
// condition names before it is saved (design 2026-11-19 §8): the
// clusters; per cluster reachable now, the machines (any user); the users
// there are (any cluster, any machine). A hit is anything not false;
// "maybe" counts those that depend on what is not known. Not the rules'
// outcome: order and effects are left out.
func (s *Server) handleAccessPreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req struct {
		Match string `json:"match"`
	}
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	e, err := access.Parse(req.Match)
	if err != nil {
		var pe *access.ParseError
		errors.As(err, &pe)
		s.writeJSON(w, http.StatusOK, map[string]any{"error": map[string]any{"offset": pe.Offset, "message": pe.Msg}})
		return
	}
	type clusterHit struct {
		Name  string `json:"name"`
		Maybe bool   `json:"maybe"`
	}
	type machineHits struct {
		Cluster      string   `json:"cluster,omitempty"`
		Count        int      `json:"count"`
		Maybe        int      `json:"maybe"`
		Names        []string `json:"names"`
		Unreservable int      `json:"unreservable"`
	}
	type userHits struct {
		Count int      `json:"count"`
		Maybe int      `json:"maybe"`
		Total int      `json:"total"`
		Names []string `json:"names"`
	}
	const shown = 20
	clusters, machines := []clusterHit{}, []machineHits{}
	for _, cc := range s.ks(r) {
		name := cc.Name
		if v := e.Eval(access.Facts{Cluster: &name}); v != access.False {
			clusters = append(clusters, clusterHit{Name: name, Maybe: v == access.Unknown})
		}
		var nodes corev1.NodeList
		if err := cc.Client.List(r.Context(), &nodes); err != nil {
			continue // away
		}
		m := machineHits{Cluster: s.clusterLabel(cc), Names: []string{}}
		for i := range nodes.Items {
			n := &nodes.Items[i]
			v := e.Eval(access.Facts{Cluster: &name, Node: &access.Node{Name: n.Name, Labels: n.Labels}})
			if v == access.False {
				continue
			}
			m.Count++
			if v == access.Unknown {
				m.Maybe++
			}
			m.Names = append(m.Names, n.Name)
			if n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" {
				m.Unreservable++
			}
		}
		slices.Sort(m.Names)
		if len(m.Names) > shown {
			m.Names = m.Names[:shown]
		}
		machines = append(machines, m)
	}
	users := userHits{Names: []string{}}
	var ul devpodv1alpha1.UserList
	var gl devpodv1alpha1.UserGroupList
	if err := s.Reader.List(r.Context(), &ul); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if err := s.Reader.List(r.Context(), &gl); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	users.Total = len(ul.Items)
	for i := range ul.Items {
		u := ul.Items[i].Name
		in := map[string]bool{}
		for j := range gl.Items {
			if slices.Contains(gl.Items[j].Spec.Members, u) {
				in[gl.Items[j].Name] = true
			}
		}
		v := e.Eval(access.Facts{User: &u, Groups: in})
		if v == access.False {
			continue
		}
		users.Count++
		if v == access.Unknown {
			users.Maybe++
		}
		users.Names = append(users.Names, u)
	}
	slices.Sort(users.Names)
	if len(users.Names) > shown {
		users.Names = users.Names[:shown]
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"clusters": clusters, "machines": machines, "users": users,
		"hasMachine": len(e.Items(access.FieldMachine)) > 0,
	})
}

// violation: a running DevPod the rules (as saved now) would not let be
// where it is.
type violation struct {
	Cluster string `json:"cluster,omitempty"`
	DevPod  string `json:"devpod"`
	Owner   string `json:"owner"`
	Node    string `json:"node"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
	Rule    int    `json:"rule"`
}

// handleAccessCheck (admin) judges every running DevPod of every cluster
// reachable now for its owner on the machine it is on (where its Pod is;
// not placed yet, where it is pinned), as a start there would be judged:
// rule, reservation, privileges and host paths. Admins' are skipped. It
// only reads: what to do about them is the admin's.
func (s *Server) handleAccessCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	items := []violation{}
	for _, cc := range s.ks(r) {
		ctx := s.in(r.Context(), cc)
		on, err := s.devPodsByNode(ctx, cc)
		if err != nil {
			continue // away
		}
		judges := map[string]*judge{}
		for node, dps := range on {
			var n corev1.Node
			if err := cc.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
				continue // gone: nothing runs there
			}
			for _, p := range dps {
				dp := p.DP
				j := judges[dp.Spec.Owner]
				if j == nil {
					if j, err = s.judgeFor(ctx, dp.Spec.Owner); err != nil {
						s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
						return
					}
					judges[dp.Spec.Owner] = j
				}
				var needs access.Needs
				if dp.Spec.Pod != nil {
					needs = access.NeedsOf(&dp.Spec.Pod.Spec)
				}
				if e := j.refusal(&n, &needs, true); e != nil {
					items = append(items, violation{Cluster: s.clusterLabel(cc), DevPod: dp.Name, Owner: dp.Spec.Owner, Node: node, Code: e.Code, Reason: e.Message, Rule: j.on(&n).Rule})
				}
			}
		}
	}
	slices.SortFunc(items, func(a, b violation) int {
		if c := strings.Compare(a.Cluster, b.Cluster); c != 0 {
			return c
		}
		return strings.Compare(a.DevPod, b.DevPod)
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

/* ---- user groups ---- */

type groupView struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Members     []string `json:"members"`
}

func groupViewOf(g *devpodv1alpha1.UserGroup) groupView {
	m := g.Spec.Members
	if m == nil {
		m = []string{}
	}
	return groupView{Name: g.Name, DisplayName: g.Spec.DisplayName, Members: m}
}

func cleanMembers(in []string) []string {
	out := []string{}
	for _, m := range in {
		if m = strings.TrimSpace(m); m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// handleListGroups: GET /api/admin/groups.
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var list devpodv1alpha1.UserGroupList
	if err := s.Reader.List(r.Context(), &list); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := make([]groupView, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, groupViewOf(&list.Items[i]))
	}
	slices.SortFunc(items, func(a, b groupView) int { return strings.Compare(a.Name, b.Name) })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCreateGroup: POST /api/admin/groups.
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req groupView
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if len(req.Name) > 63 || !dns1123Label.MatchString(req.Name) {
		s.writeServerErr(w, badRequest("a group name is 1 to 63 lower-case letters, digits and hyphens"))
		return
	}
	g := &devpodv1alpha1.UserGroup{ObjectMeta: metav1.ObjectMeta{Name: req.Name}, Spec: devpodv1alpha1.UserGroupSpec{DisplayName: req.DisplayName, Members: cleanMembers(req.Members)}}
	if err := s.Client.Create(r.Context(), g); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeErr(w, http.StatusConflict, "ALREADY_EXISTS", "a group named "+req.Name+" exists", nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("create the group", err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, groupViewOf(g))
}

// handleUpdateGroup: PUT /api/admin/groups/{name} — its display name and members.
func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req groupView
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	var g devpodv1alpha1.UserGroup
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name")}, &g); err != nil {
			return err
		}
		g.Spec.DisplayName, g.Spec.Members = req.DisplayName, cleanMembers(req.Members)
		return s.Client.Update(r.Context(), &g)
	})
	if apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such group", nil)
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("update the group", err), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, groupViewOf(&g))
}

// handleDeleteGroup: DELETE /api/admin/groups/{name} — not while a rule names it.
func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	p, err := s.currentAccess(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if p != nil {
		// named anywhere in a condition, "not" included (a glob does not hold
		// it: a rule naming a group gone denies everyone, design §5.5)
		var in []string
		for i, rule := range p.Spec.Rules {
			e, err := access.Parse(access.MatchOf(rule))
			if err != nil {
				continue // denies everyone already
			}
			if slices.Contains(e.Items(access.FieldGroup), name) {
				in = append(in, strconv.Itoa(i+1))
			}
		}
		if len(in) > 0 {
			s.writeErr(w, http.StatusConflict, "IN_USE", "rule "+strings.Join(in, ", ")+" names this group", nil)
			return
		}
	}
	var grants devpodv1alpha1.AccessGrantList
	if err := s.Reader.List(r.Context(), &grants); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	var by []string
	for i := range grants.Items {
		if e, err := access.Parse(grants.Items[i].Spec.Match); err == nil && slices.Contains(e.Items(access.FieldGroup), name) {
			by = append(by, grants.Items[i].Name)
		}
	}
	if len(by) > 0 {
		s.writeErr(w, http.StatusConflict, "IN_USE", "grant "+strings.Join(by, ", ")+" names this group", nil)
		return
	}
	err = s.Client.Delete(r.Context(), &devpodv1alpha1.UserGroup{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such group", nil)
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if err := s.dropGroupShares(r.Context(), name); err != nil {
		slog.Warn("group_shares", "group", name, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleShareGroups: GET /api/groups — the user groups anyone signed in
// may share a config with: their names, not their members.
func (s *Server) handleShareGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFrom(w, r); !ok {
		return
	}
	var list devpodv1alpha1.UserGroupList
	if err := s.Reader.List(r.Context(), &list); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	type item struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName,omitempty"`
	}
	items := []item{}
	for i := range list.Items {
		items = append(items, item{Name: list.Items[i].Name, DisplayName: list.Items[i].Spec.DisplayName})
	}
	slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.Name, b.Name) })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
