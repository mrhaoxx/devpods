// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Saved configs (config center spec §2, §5.1): DevPod YAML with
// variables, kept per user, shared with named users or with everyone.
// Reads go to the API server, not the cache: a config saved a moment
// ago is there on the next request.

const (
	// configUpdatedAnnotation: when the config was last written.
	configUpdatedAnnotation = "devpod.io/updated-at"

	maxConfigName    = 63
	maxConfigTitle   = 80
	maxConfigDesc    = 500
	maxConfigContent = 256 << 10
	maxConfigShares  = 64
)

// A user name as it may appear in a share list. Whether such a user
// exists is not checked — like a DevPod's collaborators: users of an
// LDAP source have no object here to look up.
var shareNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9._]{0,61}[a-z0-9])?$`)

// ConfigView is a config as the API shows it. Lists leave Content out.
type ConfigView struct {
	Name        string          `json:"name"`
	Owner       string          `json:"owner"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Content     *string         `json:"content,omitempty"`
	SharedWith  []string        `json:"sharedWith"`
	Public      bool            `json:"public"`
	Official    bool            `json:"official"`
	Variables   []ConfigVarView `json:"variables"`
	Mine        bool            `json:"mine"`
	UpdatedAt   string          `json:"updatedAt"`
}

func configView(c *devpodv1alpha1.DevPodConfig, sess Session, withContent bool) ConfigView {
	v := ConfigView{
		Name:        c.Name,
		Owner:       c.Spec.Owner,
		Title:       c.Spec.Title,
		Description: c.Spec.Description,
		SharedWith:  c.Spec.SharedWith,
		Public:      c.Spec.Public,
		Official:    isOfficial(c),
		Variables:   varViews(c),
		Mine:        c.Spec.Owner == sess.User,
		UpdatedAt:   c.Annotations[configUpdatedAnnotation],
	}
	if v.SharedWith == nil {
		v.SharedWith = []string{}
	}
	if v.UpdatedAt == "" {
		v.UpdatedAt = c.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	// who else a config is shared with is the owner's business
	if !v.Mine && !sess.Admin {
		v.SharedWith = []string{}
	}
	if withContent {
		v.Content = &c.Spec.Content
	}
	return v
}

// configVisible: the owner, the users it is shared with, everyone when
// it is public, and admins.
func configVisible(c *devpodv1alpha1.DevPodConfig, sess Session) bool {
	return sess.Admin || c.Spec.Owner == sess.User || c.Spec.Public || slices.Contains(c.Spec.SharedWith, sess.User)
}

// visibleConfig fetches a config the session may see. One it may not
// see answers like one that does not exist.
func (s *Server) visibleConfig(ctx context.Context, sess Session, name string) (*devpodv1alpha1.DevPodConfig, *serverErr) {
	notFound := &serverErr{http.StatusNotFound, "NOT_FOUND", "no such config", nil}
	if len(name) > maxConfigName || !dns1123Label.MatchString(name) {
		return nil, notFound
	}
	var c devpodv1alpha1.DevPodConfig
	if err := s.Reader.Get(ctx, types.NamespacedName{Name: name, Namespace: s.NS}, &c); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, notFound
		}
		return nil, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	if !configVisible(&c, sess) {
		return nil, notFound
	}
	return &c, nil
}

func (s *Server) writeServerErr(w http.ResponseWriter, e *serverErr) {
	s.writeErr(w, e.status, e.Code, e.Message, e.Detail)
}

func badRequest(format string, args ...any) *serverErr {
	return &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf(format, args...), nil}
}

// cleanShares validates a share list and puts it in order: no repeats,
// not the owner.
func cleanShares(owner string, in []string) ([]string, *serverErr) {
	out := []string{}
	for _, u := range in {
		if !shareNameRE.MatchString(u) {
			return nil, badRequest("%q is not a user name", u)
		}
		if u != owner && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	if len(out) > maxConfigShares {
		return nil, badRequest("a config is shared with at most %d users", maxConfigShares)
	}
	sort.Strings(out)
	return out, nil
}

func checkConfigText(title, desc, content *string) *serverErr {
	if title != nil && (*title == "" || utf8.RuneCountInString(*title) > maxConfigTitle) {
		return badRequest("title is required, at most %d characters", maxConfigTitle)
	}
	if desc != nil && utf8.RuneCountInString(*desc) > maxConfigDesc {
		return badRequest("description is at most %d characters", maxConfigDesc)
	}
	if content != nil && (*content == "" || len(*content) > maxConfigContent) {
		return badRequest("content is required, at most %d KiB", maxConfigContent>>10)
	}
	return nil
}

func stampConfig(c *devpodv1alpha1.DevPodConfig) {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[configUpdatedAnnotation] = time.Now().UTC().Format(time.RFC3339)
}

func decodeStrict(r *http.Request, v any) *serverErr {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("malformed request body: %s", err.Error())
	}
	return nil
}

// handleListConfigs: GET /api/configs?scope=mine|shared|public — the
// configs the caller may see; without a scope, all three together.
func (s *Server) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope != "" && scope != "mine" && scope != "shared" && scope != "public" && scope != "official" {
		s.writeServerErr(w, badRequest("scope is mine, shared, public or official"))
		return
	}
	var list devpodv1alpha1.DevPodConfigList
	if err := s.Reader.List(r.Context(), &list, client.InNamespace(s.NS)); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := []ConfigView{}
	for i := range list.Items {
		c := &list.Items[i]
		mine := c.Spec.Owner == sess.User
		shared := !mine && slices.Contains(c.Spec.SharedWith, sess.User)
		var in bool
		switch scope {
		case "mine":
			in = mine
		case "shared":
			in = shared
		case "public":
			in = c.Spec.Public
		case "official":
			in = isOfficial(c) && (mine || shared || c.Spec.Public)
		default:
			in = mine || shared || c.Spec.Public
		}
		if in {
			items = append(items, configView(c, sess, false))
		}
	}
	// official first, then the most recently changed
	sort.Slice(items, func(i, j int) bool {
		if items[i].Official != items[j].Official {
			return items[i].Official
		}
		if items[i].UpdatedAt != items[j].UpdatedAt {
			return items[i].UpdatedAt > items[j].UpdatedAt
		}
		return items[i].Name < items[j].Name
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleGetConfig: GET /api/configs/{name}.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	c, apiErr := s.visibleConfig(r.Context(), sess, r.PathValue("name"))
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	s.writeJSON(w, http.StatusOK, configView(c, sess, true))
}

// ConfigCreateRequest is the POST /api/configs body.
type ConfigCreateRequest struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Content     string   `json:"content"`
	SharedWith  []string `json:"sharedWith,omitempty"`
	Public      bool     `json:"public,omitempty"`
	// Variables: what the config says of its variables (type, description).
	Variables []VarDecl `json:"variables,omitempty"`
	// Official: admins only, on a public config.
	Official bool `json:"official,omitempty"`
}

// handleCreateConfig: POST /api/configs — a config of the caller's,
// named <user>-<id>.
func (s *Server) handleCreateConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var req ConfigCreateRequest
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	name := sess.User + "-" + req.ID
	if req.ID == "" || len(name) > maxConfigName || !dns1123Label.MatchString(req.ID) || !dns1123Label.MatchString(name) {
		s.writeServerErr(w, badRequest("id must be lower-case letters, digits and hyphens; %q may be at most %d characters", name, maxConfigName))
		return
	}
	if apiErr := checkConfigText(&req.Title, &req.Description, &req.Content); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	shares, apiErr := cleanShares(sess.User, req.SharedWith)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if req.Official && !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "only an admin marks a config official", nil)
		return
	}
	if req.Official && !req.Public {
		s.writeServerErr(w, badRequest("only a public config can be official"))
		return
	}
	decls, apiErr := cleanDecls(req.Content, req.Variables)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	c := &devpodv1alpha1.DevPodConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.NS},
		Spec: devpodv1alpha1.DevPodConfigSpec{
			Owner:       sess.User,
			Title:       req.Title,
			Description: req.Description,
			Content:     req.Content,
			SharedWith:  shares,
			Public:      req.Public,
		},
	}
	stampConfig(c)
	writeDecls(c, decls)
	if req.Official {
		c.Annotations[annOfficial] = "true"
	}
	if err := s.Client.Create(r.Context(), c); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeErr(w, http.StatusConflict, "CONFIG_EXISTS", fmt.Sprintf("you already have a config %q", req.ID), nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("create", err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, configView(c, sess, true))
}

// ConfigPatchRequest is the PATCH /api/configs/{name} body: the fields
// to change.
type ConfigPatchRequest struct {
	Title       *string    `json:"title,omitempty"`
	Description *string    `json:"description,omitempty"`
	Content     *string    `json:"content,omitempty"`
	SharedWith  *[]string  `json:"sharedWith,omitempty"`
	Public      *bool      `json:"public,omitempty"`
	Variables   *[]VarDecl `json:"variables,omitempty"`
	Official    *bool      `json:"official,omitempty"`
}

// handlePatchConfig: PATCH /api/configs/{name}. The owner changes
// anything but whether it is official; an admin who is not the owner
// may only take it off the public list, or mark it official or not.
func (s *Server) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	c, apiErr := s.visibleConfig(r.Context(), sess, r.PathValue("name"))
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	var req ConfigPatchRequest
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if req.Official != nil && !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "only an admin marks a config official", nil)
		return
	}
	if c.Spec.Owner != sess.User {
		nothingElse := req.Title == nil && req.Description == nil && req.Content == nil && req.SharedWith == nil && req.Variables == nil
		unpublish := req.Public != nil && !*req.Public && req.Official == nil
		mark := req.Official != nil && req.Public == nil
		if !sess.Admin || !nothingElse || !(unpublish || mark) {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "only the owner may change a config", nil)
			return
		}
	}
	if apiErr := checkConfigText(req.Title, req.Description, req.Content); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if req.Title != nil {
		c.Spec.Title = *req.Title
	}
	if req.Description != nil {
		c.Spec.Description = *req.Description
	}
	if req.Content != nil {
		c.Spec.Content = *req.Content
	}
	if req.SharedWith != nil {
		shares, apiErr := cleanShares(c.Spec.Owner, *req.SharedWith)
		if apiErr != nil {
			s.writeServerErr(w, apiErr)
			return
		}
		c.Spec.SharedWith = shares
	}
	if req.Public != nil {
		c.Spec.Public = *req.Public
	}
	switch {
	case req.Variables != nil:
		decls, apiErr := cleanDecls(c.Spec.Content, *req.Variables)
		if apiErr != nil {
			s.writeServerErr(w, apiErr)
			return
		}
		writeDecls(c, decls)
	case req.Content != nil:
		// what the new text no longer has is no longer described
		decls, apiErr := cleanDecls(c.Spec.Content, readDecls(c))
		if apiErr != nil {
			decls = nil
		}
		writeDecls(c, decls)
	}
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	if req.Official != nil {
		if *req.Official && !c.Spec.Public {
			s.writeServerErr(w, badRequest("only a public config can be official"))
			return
		}
		if *req.Official {
			c.Annotations[annOfficial] = "true"
		} else {
			delete(c.Annotations, annOfficial)
		}
	}
	if !c.Spec.Public {
		delete(c.Annotations, annOfficial)
	}
	stampConfig(c)
	if err := s.Client.Update(r.Context(), c); err != nil {
		if apierrors.IsConflict(err) {
			s.writeErr(w, http.StatusConflict, "CONFLICT", "the config changed meanwhile; reload and try again", nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("update", err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, configView(c, sess, true))
}

// handleDeleteConfig: DELETE /api/configs/{name} — the owner or an admin.
func (s *Server) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	c, apiErr := s.visibleConfig(r.Context(), sess, r.PathValue("name"))
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if c.Spec.Owner != sess.User && !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "only the owner may delete a config", nil)
		return
	}
	if err := s.Client.Delete(r.Context(), c); err != nil && !apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dropUserConfigs removes what a deleted user leaves in the config
// center: the configs they own, and their name in others' share lists —
// a later user of the same name inherits nothing.
func (s *Server) dropUserConfigs(ctx context.Context, user string) error {
	var list devpodv1alpha1.DevPodConfigList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.NS)); err != nil {
		return err
	}
	var first error
	keep := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) && first == nil {
			first = err
		}
	}
	for i := range list.Items {
		c := &list.Items[i]
		switch {
		case c.Spec.Owner == user:
			keep(s.Client.Delete(ctx, c))
		case slices.Contains(c.Spec.SharedWith, user):
			c.Spec.SharedWith = slices.DeleteFunc(c.Spec.SharedWith, func(u string) bool { return u == user })
			keep(s.Client.Update(ctx, c))
		}
	}
	return first
}
