// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"sort"
	"strings"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

const (
	searchPerGroup = 5
	searchMaxQ     = 100
	// searchLookback bounds the audit groups; the sessions group filters
	// this many recent sessions by substring.
	searchLookback    = 7 * 24 * time.Hour
	searchSessionScan = 200
)

type searchUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
}

type searchDevPod struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster,omitempty"`
	Owner   string `json:"owner"`
	Phase   string `json:"phase,omitempty"`
}

type searchSession struct {
	ID       string    `json:"id"`
	User     string    `json:"user"`
	DevPod   string    `json:"devpod"`
	OpenedAt time.Time `json:"openedAt"`
}

type searchCommand struct {
	SessionID string    `json:"sessionId"`
	TS        time.Time `json:"ts"`
	User      string    `json:"user"`
	DevPod    string    `json:"devpod"`
	Cmd       string    `json:"cmd"`
}

// SearchResult is the ⌘K palette's response (UI spec §5.9).
type SearchResult struct {
	Users      []searchUser    `json:"users"`
	DevPods    []searchDevPod  `json:"devpods"`
	Sessions   []searchSession `json:"sessions"`
	Commands   []searchCommand `json:"commands"`
	AuditError string          `json:"auditError,omitempty"`
}

func containsFold(s, sub string) bool { return strings.Contains(strings.ToLower(s), sub) }

// handleSearch serves GET /api/search?q=: up to five users, DevPods,
// sessions and commands matching q. Non-admins see no users and only
// their own DevPods, sessions and commands.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.freshSession(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > searchMaxQ {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "q must be 1 to 100 characters", nil)
		return
	}
	lq := strings.ToLower(q)
	out := SearchResult{Users: []searchUser{}, DevPods: []searchDevPod{}, Sessions: []searchSession{}, Commands: []searchCommand{}}

	if sess.Admin {
		var users devpodv1alpha1.UserList
		if err := s.Client.List(r.Context(), &users); err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		sort.Slice(users.Items, func(i, j int) bool { return users.Items[i].Name < users.Items[j].Name })
		for _, u := range users.Items {
			if len(out.Users) == searchPerGroup {
				break
			}
			if containsFold(u.Name, lq) || containsFold(u.Spec.DisplayName, lq) {
				out.Users = append(out.Users, searchUser{Name: u.Name, DisplayName: u.Spec.DisplayName})
			}
		}
	}

	dps, err := s.devPods(r, "")
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	sort.SliceStable(dps, func(i, j int) bool { return dps[i].Name < dps[j].Name })
	names := map[string]string{} // uid → name, for the commands group
	for _, dp := range dps {
		names[string(dp.UID)] = dp.Name
		if !sess.Admin && dp.Spec.Owner != sess.User {
			continue
		}
		if len(out.DevPods) < searchPerGroup && (containsFold(dp.Name, lq) || (sess.Admin && containsFold(dp.Spec.Owner, lq))) {
			out.DevPods = append(out.DevPods, searchDevPod{Name: dp.Name, Cluster: dp.Cluster, Owner: dp.Spec.Owner, Phase: string(dp.Status.Phase)})
		}
	}

	if s.Audit != nil {
		user := ""
		if !sess.Admin {
			user = sess.User
		}
		to := time.Now().UTC()
		from := to.Add(-searchLookback)
		recent, _, err := s.Audit.Store.ListSessions(r.Context(), store.SessionFilter{User: user, From: from, To: to, Limit: searchSessionScan})
		if err == nil {
			for _, ss := range recent {
				if len(out.Sessions) == searchPerGroup {
					break
				}
				if strings.HasPrefix(ss.SessionID, q) || containsFold(ss.DevPod, lq) || containsFold(ss.User, lq) {
					out.Sessions = append(out.Sessions, searchSession{ID: ss.SessionID, User: ss.User, DevPod: ss.DevPod, OpenedAt: ss.OpenedAt})
				}
			}
			var cmds []store.CommandRow
			cmds, _, err = s.Audit.Store.SearchCommands(r.Context(), store.CommandFilter{User: user, Q: q, From: from, To: to, Limit: searchPerGroup})
			for _, c := range cmds {
				if len(out.Commands) == searchPerGroup {
					break
				}
				dp := names[c.DevPodUID]
				if dp == "" {
					dp = c.DevPodUID // deleted DevPod
				}
				out.Commands = append(out.Commands, searchCommand{SessionID: c.SessionID, TS: c.TS, User: c.User, DevPod: dp, Cmd: c.Cmdline})
			}
		}
		if err != nil {
			out.Sessions, out.Commands = []searchSession{}, []searchCommand{}
			out.AuditError = "audit search unavailable: " + err.Error()
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}
