// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/configvars"
)

// Rebuilding a DevPod from the config it came from (config-centric
// create spec §4.5): the config's text as it is now, with the values
// recorded when the DevPod was launched, saved the way a save from the
// YAML tab is.

// configOutdated: the config dp came from has changed since — nil when
// dp did not come from one, or the caller may not see it.
func (s *Server) configOutdated(ctx context.Context, sess Session, dp *devpodv1alpha1.DevPod) *bool {
	name := dp.Labels[lblConfig]
	if name == "" {
		return nil
	}
	c, apiErr := s.visibleConfig(ctx, sess, name)
	if apiErr != nil {
		return nil
	}
	v := contentHash(c.Spec.Content) != dp.Annotations[annConfigHash]
	return &v
}

func configConflict(code, msg string, detail any) *serverErr {
	return &serverErr{http.StatusConflict, code, msg, detail}
}

// renderFromConfig is dp's manifest from its config as it is now, and
// that text's hash.
func (s *Server) renderFromConfig(r *http.Request, sess Session, dp *devpodv1alpha1.DevPod) (string, string, *serverErr) {
	name := dp.Labels[lblConfig]
	if name == "" {
		return "", "", configConflict("CONFIG_GONE", "this DevPod was not made from a config", nil)
	}
	c, apiErr := s.visibleConfig(r.Context(), sess, name)
	if apiErr != nil {
		return "", "", configConflict("CONFIG_GONE", fmt.Sprintf("the config %q is gone, or no longer shared with you", name), nil)
	}
	recorded := map[string]string{}
	if raw := dp.Annotations[annConfigVars]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &recorded) // broken: as if nothing was recorded
	}
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	decls := readDecls(c)
	values := map[string]string{}
	var bad []string
	for _, v := range configvars.Parse(c.Spec.Content) {
		switch v.Name {
		case configvars.Owner:
			values[v.Name] = dp.Spec.Owner
		case configvars.Index:
			values[v.Name] = or(dp.Annotations[annGroupIndex], "0")
		case configvars.Count:
			values[v.Name] = or(dp.Annotations[annGroupSize], "1")
		case configvars.Leader:
			values[v.Name] = or(dp.Annotations[annGroupLeader], dp.Name)
		case configvars.Node:
			if node := pinnedNode(dp); node != "" {
				values[v.Name] = node
			} else {
				bad = append(bad, v.Name)
			}
		case configvars.Name:
			values[v.Name] = or(recorded[v.Name], dp.Name)
		default:
			given, ok := recorded[v.Name]
			if ok {
				values[v.Name] = given
			}
			effective := or(given, v.Default)
			if effective == "" && v.Required {
				bad = append(bad, v.Name)
				continue
			}
			if effective != "" && s.checkVarValue(r, sess, declOf(decls, v.Name), effective) != nil {
				bad = append(bad, v.Name)
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return "", "", configConflict("CONFIG_VARS",
			fmt.Sprintf("the config's variables changed: %s no longer fit the values this DevPod was launched with", strings.Join(bad, ", ")), bad)
	}
	text, err := configvars.Render(c.Spec.Content, values)
	if err != nil {
		return "", "", configConflict("CONFIG_VARS", err.Error(), nil)
	}
	var m struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(text), &m); err == nil && m.Metadata.Name != "" && m.Metadata.Name != dp.Name {
		return "", "", configConflict("CONFIG_RENAMES",
			fmt.Sprintf("the config now names this DevPod %q: a DevPod cannot be renamed", m.Metadata.Name), nil)
	}
	return text, contentHash(c.Spec.Content), nil
}

// handleRebuildFromConfig: POST /api/devpods/{name}/rebuild-from-config
// {dryRun?}.
func (s *Server) handleRebuildFromConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	// the route asks for devpods:update; reading the config is its own scope
	if !s.requireScope(w, sess, "configs:read") {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var req struct {
		DryRun bool `json:"dryRun"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		s.writeServerErr(w, badRequest("malformed request body: %s", err.Error()))
		return
	}
	// like every edit, on the object as the API server has it now
	fresh := &devpodv1alpha1.DevPod{}
	if err := s.k(r).Reader.Get(r.Context(), types.NamespacedName{Name: dp.Name, Namespace: dp.Namespace}, fresh); err == nil {
		dp = fresh
	}
	text, hash, apiErr := s.renderFromConfig(r, sess, dp)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if req.DryRun {
		var in devpodv1alpha1.DevPod
		rebuild := false
		if yaml.Unmarshal([]byte(text), &in) == nil {
			rebuild = dp.Spec.Running && (!equality.Semantic.DeepEqual(in.Spec.Pod, dp.Spec.Pod) || in.Spec.Shell != dp.Spec.Shell)
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"yaml": text, "rebuild": rebuild})
		return
	}
	resp, apiErr := s.replaceDevPodSpec(r, sess, dp, text, map[string]string{annConfigHash: hash})
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}
