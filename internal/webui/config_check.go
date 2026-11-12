// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/mrhaoxx/devpod/internal/configvars"
)

// The check before a config is saved (config-centric create spec §4.3):
// the text with example values goes through what a person may write in
// a DevPod. What depends on the moment — quota, a node being free, a
// name being taken — is the launch's to say.

// CheckRequest is the POST /api/configs:check body.
type CheckRequest struct {
	Content   string            `json:"content"`
	Variables []VarDecl         `json:"variables,omitempty"`
	Vars      map[string]string `json:"vars,omitempty"`
}

// CheckProblem is one thing wrong with a config.
type CheckProblem struct {
	Line    int    `json:"line,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

var yamlLineRE = regexp.MustCompile(`line (\d+)`)

func (s *Server) handleCheckConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var req CheckRequest
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if apiErr := checkConfigText(nil, nil, &req.Content); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"problems": s.checkConfig(r, sess, req)})
}

func (s *Server) checkConfig(r *http.Request, sess Session, req CheckRequest) []CheckProblem {
	one := func(code, msg string) []CheckProblem { return []CheckProblem{{Code: code, Message: msg}} }
	decls, apiErr := cleanDecls(req.Content, req.Variables)
	if apiErr != nil {
		return one("BAD_VARIABLES", apiErr.Message)
	}
	name := sess.User + "-x"
	values := map[string]string{}
	for _, v := range configvars.Parse(req.Content) {
		if given, ok := req.Vars[v.Name]; ok {
			values[v.Name] = given
			continue
		}
		switch v.Name {
		case configvars.Owner:
			values[v.Name] = sess.User
		case configvars.Name, configvars.Leader:
			values[v.Name] = name
		case configvars.Index:
			values[v.Name] = "0"
		case configvars.Count:
			values[v.Name] = "1"
		case configvars.Node:
			values[v.Name] = s.exampleNode(r, sess)
		default:
			if v.Default != "" {
				continue // the default stands
			}
			values[v.Name] = s.exampleValue(r, sess, declOf(decls, v.Name))
		}
	}
	text, err := configvars.Render(req.Content, values)
	if err != nil {
		return one("BAD_VARIABLES", err.Error())
	}
	dp, apiErr := s.buildDevPod(sess, CreateRequest{YAML: text})
	if apiErr != nil {
		switch {
		case strings.HasPrefix(apiErr.Message, "invalid YAML"):
			p := CheckProblem{Code: "BAD_YAML", Message: apiErr.Message}
			if m := yamlLineRE.FindStringSubmatch(apiErr.Message); m != nil {
				p.Line, _ = strconv.Atoi(m[1])
			}
			return []CheckProblem{p}
		case apiErr.status == http.StatusForbidden:
			return one("FORBIDDEN_CONTENT", apiErr.Message)
		default:
			return one("BAD_NAME", apiErr.Message)
		}
	}
	if apiErr := s.checkWriting(sess, dp); apiErr != nil {
		return one("FORBIDDEN_CONTENT", apiErr.Message)
	}
	if len(dp.Name) > MaxDevPodNameLen || !dns1123Label.MatchString(dp.Name) {
		return one("BAD_NAME", fmt.Sprintf("name %q is invalid or too long (at most %d characters: lower-case letters, digits and hyphens)", dp.Name, MaxDevPodNameLen))
	}
	return []CheckProblem{}
}

// exampleValue is a value of d's type, for a check.
func (s *Server) exampleValue(r *http.Request, sess Session, d VarDecl) string {
	switch d.Type {
	case VarInt:
		return "1"
	case VarChoice:
		return d.Options[0]
	case VarNode:
		return s.exampleNode(r, sess)
	}
	return "x"
}

// exampleNode is the first node the caller may pin to now.
func (s *Server) exampleNode(r *http.Request, sess Session) string {
	var nodes corev1.NodeList
	if err := s.k(r).Reader.List(r.Context(), &nodes); err == nil {
		names := make([]string, 0, len(nodes.Items))
		for _, n := range nodes.Items {
			names = append(names, n.Name)
		}
		sort.Strings(names)
		for _, n := range names {
			if s.pinErr(r.Context(), sess.User, n) == nil {
				return n
			}
		}
	}
	return "node"
}
