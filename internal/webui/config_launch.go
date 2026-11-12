// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/configvars"
)

// Launching DevPods from a saved config (config center spec §4): the
// variables are filled per instance, and each result goes through the
// same path as a DevPod created from YAML — nothing is added to those
// rules and nothing is skipped. A launch is checked as a whole first
// (nothing is written while any instance has a problem), then written
// one DevPod at a time.

// MaxLaunch is the most DevPods one launch creates.
const MaxLaunch = 32

// LaunchRequest is the POST /api/configs/{name}/launch body.
type LaunchRequest struct {
	// Vars are the values of the config's variables. NAME is the short
	// name (the caller's prefix is added); OWNER, INDEX and NODE are
	// filled by the server.
	Vars map[string]string `json:"vars,omitempty"`
	// Nodes: for a config with ${NODE} — one DevPod per node, in order.
	Nodes []string `json:"nodes,omitempty"`
	// Count: for a config without ${NODE} — how many (default 1).
	Count int `json:"count,omitempty"`
	// DryRun: check and show what would be created.
	DryRun bool `json:"dryRun,omitempty"`
}

// LaunchItem is the outcome for one DevPod of a launch.
type LaunchItem struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Node    string `json:"node,omitempty"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// YAML is the manifest after the variables: on a dry run, and for
	// an instance that was refused.
	YAML string `json:"yaml,omitempty"`
}

// LaunchPlan is a checked launch: what would be created.
type LaunchPlan struct {
	Items   []LaunchItem
	devpods []*devpodv1alpha1.DevPod // by item; nil where the item failed
}

// OK: every instance passed the check.
func (p *LaunchPlan) OK() bool {
	for _, it := range p.Items {
		if !it.OK {
			return false
		}
	}
	return true
}

func (it *LaunchItem) fail(code, msg string) {
	it.OK, it.Code, it.Message = false, code, msg
}

// planLaunch checks a launch without writing anything. A request that
// is wrong as a whole is a serverErr; a problem with one instance is on
// its item.
func (s *Server) planLaunch(r *http.Request, sess Session, c *devpodv1alpha1.DevPodConfig, req LaunchRequest) (*LaunchPlan, *serverErr) {
	content := c.Spec.Content
	usesName, usesNode := configvars.Uses(content, configvars.Name), configvars.Uses(content, configvars.Node)
	usesIndex, usesOwner := configvars.Uses(content, configvars.Index), configvars.Uses(content, configvars.Owner)

	for _, k := range []string{configvars.Owner, configvars.Index} {
		if _, given := req.Vars[k]; given {
			return nil, badRequest("%s is filled by the server", k)
		}
	}
	if _, given := req.Vars[configvars.Node]; given {
		return nil, badRequest("give the nodes in \"nodes\", not as the variable NODE")
	}
	n := req.Count
	if usesNode {
		if req.Count != 0 {
			return nil, badRequest("this config is pinned to a node: \"nodes\" says how many, \"count\" is not used")
		}
		n = len(req.Nodes)
		if n == 0 {
			return nil, badRequest("this config needs a node: give \"nodes\"")
		}
		for _, node := range req.Nodes {
			if node == "" {
				return nil, badRequest("a node name is empty")
			}
		}
	} else {
		if len(req.Nodes) > 0 {
			return nil, badRequest("this config has no ${NODE}: \"nodes\" is not used")
		}
		if n == 0 {
			n = 1
		}
	}
	if n < 1 || n > MaxLaunch {
		return nil, badRequest("a launch creates 1 to %d DevPods", MaxLaunch)
	}
	short := req.Vars[configvars.Name]
	if usesName && short == "" {
		return nil, &serverErr{http.StatusBadRequest, "BAD_VARIABLES", "missing variables: NAME", []string{configvars.Name}}
	}

	values := func(i int) map[string]string {
		v := make(map[string]string, len(req.Vars)+4)
		for k, val := range req.Vars {
			v[k] = val
		}
		if usesOwner {
			v[configvars.Owner] = sess.User
		}
		if usesIndex {
			v[configvars.Index] = strconv.Itoa(i)
		}
		if usesNode {
			v[configvars.Node] = req.Nodes[i-1]
		}
		if usesName {
			name := sess.User + "-" + short
			// several from one name: the index tells them apart, unless
			// the config places ${INDEX} itself
			if n > 1 && !usesIndex {
				name += "-" + strconv.Itoa(i)
			}
			v[configvars.Name] = name
		}
		return v
	}

	// the variables are the same for every instance: a wrong set is the
	// request's problem, said once
	if _, err := configvars.Render(content, values(1)); err != nil {
		var missing *configvars.MissingError
		var unknown *configvars.UnknownError
		var detail any
		switch {
		case errors.As(err, &missing):
			detail = missing.Names
		case errors.As(err, &unknown):
			detail = unknown.Names
		}
		return nil, &serverErr{http.StatusBadRequest, "BAD_VARIABLES", err.Error(), detail}
	}

	plan := &LaunchPlan{Items: make([]LaunchItem, n), devpods: make([]*devpodv1alpha1.DevPod, n)}
	var pending []*devpodv1alpha1.DevPod
	for i := 1; i <= n; i++ {
		it := &plan.Items[i-1]
		it.Index, it.OK = i, true
		v := values(i)
		it.Name = v[configvars.Name]
		if usesNode {
			it.Node = req.Nodes[i-1]
		}
		text, err := configvars.Render(content, v)
		if err != nil {
			it.fail("BAD_VARIABLES", err.Error())
			continue
		}
		it.YAML = text
		dp, apiErr := s.prepareDevPod(r, sess, CreateRequest{YAML: text}, pending)
		if apiErr != nil {
			it.fail(apiErr.Code, apiErr.Message)
			continue
		}
		it.Name = dp.Name
		if node := pinnedNode(dp); node != "" {
			it.Node = node
		}
		if len(dp.Name) > MaxDevPodNameLen || !dns1123Label.MatchString(dp.Name) {
			it.fail("BAD_NAME", fmt.Sprintf("name %q is invalid or too long (at most %d characters: lower-case letters, digits and hyphens)", dp.Name, MaxDevPodNameLen))
			continue
		}
		plan.devpods[i-1] = dp
		pending = append(pending, dp)
	}

	// names: no two alike in the launch, none already taken
	byName := map[string]int{}
	for i, dp := range plan.devpods {
		if dp != nil {
			byName[plan.Items[i].Name]++
		}
	}
	for i, dp := range plan.devpods {
		if dp == nil {
			continue
		}
		it := &plan.Items[i]
		if byName[it.Name] > 1 {
			it.fail("DUPLICATE_NAME", fmt.Sprintf("%d DevPods of this launch would be named %q: use ${NAME} or ${INDEX} in the config's name", byName[it.Name], it.Name))
			plan.devpods[i] = nil
			continue
		}
		var have devpodv1alpha1.DevPod
		err := s.k(r).Reader.Get(r.Context(), types.NamespacedName{Name: it.Name, Namespace: s.NS}, &have)
		switch {
		case err == nil:
			it.fail("NAME_TAKEN", fmt.Sprintf("a DevPod named %q already exists", it.Name))
			plan.devpods[i] = nil
		case !apierrors.IsNotFound(err):
			return nil, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
		}
	}
	return plan, nil
}

// runLaunch writes a checked plan, one DevPod after another. One that
// fails now — the name was taken meanwhile, say — does not stop the
// rest and nothing is undone. The status is 200 when all were created,
// 207 otherwise.
func (s *Server) runLaunch(r *http.Request, plan *LaunchPlan) ([]LaunchItem, int) {
	items := make([]LaunchItem, len(plan.Items))
	status := http.StatusOK
	for i, it := range plan.Items {
		it.YAML = ""
		if err := s.k(r).Client.Create(r.Context(), plan.devpods[i]); err != nil {
			if apierrors.IsAlreadyExists(err) {
				it.fail("NAME_TAKEN", fmt.Sprintf("a DevPod named %q already exists", it.Name))
			} else {
				it.fail("BAD_REQUEST", rejected("create", err))
			}
			status = http.StatusMultiStatus
		}
		items[i] = it
	}
	return items, status
}

// handleLaunchConfig: POST /api/configs/{name}/launch.
func (s *Server) handleLaunchConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	// the route asks for configs:read; creating DevPods is its own scope
	if !s.requireScope(w, sess, "devpods:create") {
		return
	}
	c, apiErr := s.visibleConfig(r.Context(), sess, r.PathValue("name"))
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	var req LaunchRequest
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	plan, apiErr := s.planLaunch(r, sess, c, req)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if !plan.OK() {
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"code":    "LAUNCH_REJECTED",
			"message": "nothing was created: some DevPods of this launch cannot be created",
			"items":   plan.Items,
		})
		return
	}
	if req.DryRun {
		s.writeJSON(w, http.StatusOK, map[string]any{"items": plan.Items})
		return
	}
	items, status := s.runLaunch(r, plan)
	s.writeJSON(w, status, map[string]any{"items": items})
}
