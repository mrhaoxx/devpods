// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// accessView is the rules as the admin API reads and writes them: the
// whole ordered list at once, with the version it was read at.
type accessView struct {
	Rules           []devpodv1alpha1.AccessRule `json:"rules"`
	Default         devpodv1alpha1.AccessEffect `json:"default"`
	ResourceVersion string                      `json:"resourceVersion"`
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

func viewOf(p *devpodv1alpha1.AccessPolicy) accessView {
	if p == nil {
		return accessView{Rules: []devpodv1alpha1.AccessRule{}, Default: devpodv1alpha1.DefaultAccessEffect()}
	}
	rules := p.Spec.Rules
	if rules == nil {
		rules = []devpodv1alpha1.AccessRule{}
	}
	return accessView{Rules: rules, Default: p.Spec.Default, ResourceVersion: p.ResourceVersion}
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
	s.writeJSON(w, http.StatusOK, viewOf(p))
}

// handlePutAccess: PUT /api/admin/access — the whole list, refused (409)
// when the rules changed since resourceVersion was read.
func (s *Server) handlePutAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req accessView
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if apiErr := s.validateAccess(r.Context(), &req); apiErr != nil {
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
	spec := devpodv1alpha1.AccessPolicySpec{Rules: req.Rules, Default: req.Default}
	switch {
	case cur == nil && req.ResourceVersion != "", cur != nil && req.ResourceVersion != cur.ResourceVersion:
		conflict()
		return
	case cur == nil:
		cur = &devpodv1alpha1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.AccessPolicyName}, Spec: spec}
		err = s.Client.Create(r.Context(), cur)
	default:
		cur.Spec = spec
		err = s.Client.Update(r.Context(), cur)
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		conflict()
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("save the rules", err), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, viewOf(cur))
}

// validateAccess checks every rule (and cleans the host paths).
func (s *Server) validateAccess(ctx context.Context, v *accessView) *serverErr {
	var groups devpodv1alpha1.UserGroupList
	if err := s.Reader.List(ctx, &groups); err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	group := map[string]bool{}
	for i := range groups.Items {
		group[groups.Items[i].Name] = true
	}
	clusterKnown := map[string]bool{}
	var cl devpodv1alpha1.ClusterList
	if s.Reader.List(ctx, &cl) == nil {
		for i := range cl.Items {
			clusterKnown[cl.Items[i].Name] = true
		}
	}
	effect := func(where string, e *devpodv1alpha1.AccessEffect) *serverErr {
		for i, h := range e.HostPaths {
			if !strings.HasPrefix(h.Path, "/") {
				return badRequest("%s: host path %q is not absolute", where, h.Path)
			}
			e.HostPaths[i].Path = path.Clean(h.Path)
		}
		return nil
	}
	for i := range v.Rules {
		rule := &v.Rules[i]
		where := "rule " + strconv.Itoa(i+1)
		switch rule.Subject.Kind {
		case "everyone":
			rule.Subject.Name = ""
		case "user":
			if rule.Subject.Name == "" {
				return badRequest("%s: which user?", where)
			}
		case "group":
			if !group[rule.Subject.Name] {
				return badRequest("%s: no group %q", where, rule.Subject.Name)
			}
		default:
			return badRequest("%s: the subject is everyone, a user or a group", where)
		}
		if rule.Cluster != "" && !clusterKnown[rule.Cluster] {
			return badRequest("%s: no cluster %q", where, rule.Cluster)
		}
		for _, m := range rule.Machines {
			if !access.ValidMachine(m) {
				return badRequest("%s: %q is neither a machine name (globs allowed) nor key=value", where, m)
			}
		}
		if apiErr := effect(where, &rule.Effect); apiErr != nil {
			return apiErr
		}
	}
	return effect("the default rule", &v.Default)
}

// handleAccessMatch: GET /api/admin/access/match?machines=a,b[&cluster=] —
// the machines a selector names, per cluster.
func (s *Server) handleAccessMatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var sel []string
	for _, m := range strings.Split(r.URL.Query().Get("machines"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			sel = append(sel, m)
		}
	}
	type item struct {
		Cluster      string   `json:"cluster,omitempty"`
		Count        int      `json:"count"`
		Names        []string `json:"names"`
		Unreservable int      `json:"unreservable"`
	}
	items := []item{}
	for _, cc := range s.ks(r) {
		var nodes corev1.NodeList
		if err := cc.Client.List(r.Context(), &nodes); err != nil {
			continue // away
		}
		it := item{Cluster: s.clusterLabel(cc), Names: []string{}}
		for i := range nodes.Items {
			n := &nodes.Items[i]
			if !access.MachineMatches(sel, access.Node{Name: n.Name, Labels: n.Labels}) {
				continue
			}
			it.Count++
			if len(it.Names) < 20 {
				it.Names = append(it.Names, n.Name)
			}
			if n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" {
				it.Unreservable++
			}
		}
		slices.Sort(it.Names)
		items = append(items, it)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
			for _, dp := range dps {
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
		for i, rule := range p.Spec.Rules {
			if rule.Subject.Kind == "group" && rule.Subject.Name == name {
				s.writeErr(w, http.StatusConflict, "IN_USE", "rule "+strconv.Itoa(i+1)+" names this group", nil)
				return
			}
		}
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
	w.WriteHeader(http.StatusNoContent)
}
