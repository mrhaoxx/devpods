// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Programmatic access to the REST API, two ways:
//
//   - a personal access token, "Authorization: Bearer dpt_<id>_<secret>":
//     it acts as its owner, limited to the scopes it lists and, when it
//     names DevPods, to those. A route without a scope in apiRoutes is
//     closed to tokens, so new endpoints are closed until listed.
//   - the SSH gateway's CLI, "Authorization: Bearer gwk_<shared key>"
//     with "X-DevPod-User": the gateway has authenticated that user by
//     SSH key and acts for them with their full rights.
const (
	tokenPrefix   = "dpt_"
	gatewayPrefix = "gwk_"
	maxTokens     = 50

	tokenAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
)

// Scope is one thing a token may be allowed to do.
type Scope struct {
	ID    string `json:"id"`
	Group string `json:"group"`
	Label string `json:"label"`
	Admin bool   `json:"admin,omitempty"` // only useful to (and offered to) admins
}

// Scopes is the catalogue, in display order.
var Scopes = []Scope{
	{ID: "devpods:read", Group: "DevPod", Label: "查看 DevPod（列表、详情、事件）"},
	{ID: "devpods:create", Group: "DevPod", Label: "新建 DevPod"},
	{ID: "devpods:power", Group: "DevPod", Label: "启动、休眠、重新部署"},
	{ID: "devpods:update", Group: "DevPod", Label: "修改配置（镜像、资源、Shell、协作者）"},
	{ID: "devpods:delete", Group: "DevPod", Label: "删除 DevPod"},
	{ID: "metrics:read", Group: "监控", Label: "查看指标、进程和容器信息"},
	{ID: "shell", Group: "访问", Label: "打开网页终端"},
	{ID: "http:read", Group: "访问", Label: "查看端口链接"},
	{ID: "http:write", Group: "访问", Label: "公开 / 取消公开端口、重置链接"},
	{ID: "http:connect", Group: "访问", Label: "换取私有端口的访问票据"},
	{ID: "audit:read", Group: "审计", Label: "查看会话、命令、文件记录和回放"},
	{ID: "keys:read", Group: "账号", Label: "查看 SSH 公钥"},
	{ID: "keys:write", Group: "账号", Label: "修改 SSH 公钥"},
	{ID: "admin:read", Group: "管理员", Label: "以管理员身份查看（全部 DevPod、节点、用户、审计）", Admin: true},
	{ID: "admin:write", Group: "管理员", Label: "以管理员身份修改（他人的 DevPod、用户、策略、断开会话）", Admin: true},
}

func validScope(id string) bool {
	return slices.ContainsFunc(Scopes, func(s Scope) bool { return s.ID == id })
}

// apiRoute documents one endpoint: the scope a token needs ("" = open to
// any valid token, "-" = never with a token) and a one-line summary.
// It is the single source for token enforcement and the OpenAPI index.
type apiRoute struct{ Pattern, Scope, Summary string }

const (
	scopeAny  = ""
	scopeNone = "-"
	// scopePatch: PATCH /api/devpods/{name} needs devpods:power when it
	// only flips running, devpods:update otherwise (checked in the handler).
	scopePatch = "devpods:power|devpods:update"
)

var apiRoutes = []apiRoute{
	{"GET /api/me", scopeAny, "Who am I: user, admin, quota, usage, features"},
	{"GET /api/templates", scopeAny, "DevPod templates"},
	{"GET /api/openapi.json", scopeAny, "This API as OpenAPI 3"},

	{"GET /api/devpods", "devpods:read", "List my DevPods (?scope=all: everyone's, admins)"},
	{"GET /api/devpods/{name}", "devpods:read", "One DevPod with its Pod"},
	{"GET /api/devpods/{name}/events", "devpods:read", "Kubernetes events of a DevPod"},
	{"GET /api/devpods/{name}/stream", "devpods:read", "Server-sent events: status and events of a DevPod"},
	{"GET /api/devpods/{name}/topology", "devpods:read", "CPU placement of a DevPod"},
	{"POST /api/devpods", "devpods:create", "Create a DevPod: {name, image, cpu, memory, gpu?, …} or {name, yaml}"},
	{"PATCH /api/devpods/{name}", scopePatch, "Edit a DevPod: {running} (devpods:power) or {image, cpu, memory, gpu, shell, collaborators, node} (devpods:update)"},
	{"POST /api/devpods/{name}/redeploy", "devpods:power", "Recreate the Pod"},
	{"DELETE /api/devpods/{name}", "devpods:delete", "Delete a DevPod"},

	{"GET /api/live/devpods", "metrics:read", "Server-sent events: live CPU / memory / GPU / network of my DevPods"},
	{"GET /api/live/devpods/recent", "metrics:read", "CPU and memory of the last 15 minutes"},
	{"GET /api/devpods/{name}/metrics", "metrics:read", "Metric history (?range=15m|1h|3h|6h|12h|24h|7d)"},
	{"GET /api/devpods/{name}/info", "metrics:read", "Container facts and listening ports"},
	{"GET /api/devpods/{name}/processes", "metrics:read", "Server-sent events: processes"},

	{"GET /api/devpods/{name}/shell", "shell", "WebSocket terminal through the SSH gateway"},
	{"GET /api/devpods/{name}/http", "http:read", "The DevPod's port address and public ports"},
	{"PUT /api/devpods/{name}/http", "http:write", "Set public ports {publicPorts} or draw a new address {reset}"},
	{"POST /api/http/ticket", "http:connect", "Trade a login for access to a private port: {host, rd} → {url}"},

	{"GET /api/audit/sessions", "audit:read", "SSH sessions"},
	{"GET /api/audit/sessions/{id}", "audit:read", "One session with its timeline"},
	{"GET /api/audit/sessions/{id}/channels/{ch}/cast", "audit:read", "A channel's recording (asciicast v2)"},
	{"GET /api/audit/sessions/{id}/channels/{ch}/live", "audit:read", "An open channel from its start, then live (NDJSON)"},
	{"GET /api/audit/sessions/{id}/channels/{ch}/raw", "admin:read", "Raw stream of a channel"},
	{"GET /api/audit/commands", "audit:read", "Search commands"},
	{"GET /api/audit/files", "audit:read", "Search file transfers"},
	{"GET /api/audit/files/{event}/content", "audit:read", "A captured file"},
	{"GET /api/audit/presence", "audit:read", "Who is connected now"},
	{"GET /api/audit/alerts", "admin:read", "Audit alerts"},
	{"GET /api/audit/access-log", "admin:read", "Who looked at which audit data"},
	{"POST /api/audit/sessions/{id}/disconnect", "admin:write", "Cut a session's SSH connection"},
	{"GET /api/search", "devpods:read", "Search users, DevPods, sessions and commands"},

	{"GET /api/me/pubkeys", "keys:read", "My SSH public keys"},
	{"PUT /api/me/pubkeys", "keys:write", "Replace my SSH public keys"},
	{"PUT /api/me/password", scopeNone, "Change my password (browser only)"},
	{"GET /api/me/tokens", scopeNone, "My access tokens (browser only)"},
	{"POST /api/me/tokens", scopeNone, "Create an access token (browser only)"},
	{"DELETE /api/me/tokens/{id}", scopeNone, "Revoke an access token (browser only)"},

	{"GET /api/admin/devpods", "admin:read", "Every DevPod"},
	{"GET /api/admin/live/overview", "admin:read", "Server-sent events: cluster overview"},
	{"GET /api/admin/nodes", "admin:read", "Nodes"},
	{"GET /api/admin/nodes/recent", "admin:read", "Node CPU and memory of the last 15 minutes"},
	{"GET /api/admin/nodes/{name}", "admin:read", "One node and its DevPods"},
	{"GET /api/kore/topology", "admin:read", "CPU topology of the cluster"},
	{"GET /api/admin/users", "admin:read", "Users with quota and usage"},
	{"POST /api/admin/users", "admin:write", "Create a user"},
	{"POST /api/admin/users:batch", "admin:write", "Batch operation on users"},
	{"POST /api/admin/users:import", "admin:write", "Import users"},
	{"PATCH /api/admin/users/{name}", "admin:write", "Edit a user"},
	{"DELETE /api/admin/users/{name}", "admin:write", "Delete a user"},
	{"GET /api/admin/users/{name}/audit-policy", "admin:read", "A user's audit policy"},
	{"PUT /api/admin/users/{name}/audit-policy", "admin:write", "Set a user's audit policy"},
	{"GET /api/admin/audit/policy-default", "admin:read", "The default audit policy"},
}

var routeScope = func() map[string]string {
	m := make(map[string]string, len(apiRoutes))
	for _, r := range apiRoutes {
		m[r.Pattern] = r.Scope
	}
	return m
}()

// TokenGrant is what a token session carries besides its user.
type TokenGrant struct {
	ID      string
	Scopes  []string
	DevPods []string // non-empty: only these DevPods
}

func (g *TokenGrant) has(scope string) bool { return slices.Contains(g.Scopes, scope) }

// AllowsDevPod: a token that names DevPods reaches only those.
func (s Session) AllowsDevPod(name string) bool {
	return s.Token == nil || len(s.Token.DevPods) == 0 || slices.Contains(s.Token.DevPods, name)
}

// RequireScope is for handlers whose scope depends on the request body.
func (s *Server) requireScope(w http.ResponseWriter, sess Session, scope string) bool {
	if sess.Token == nil || sess.Token.has(scope) {
		return true
	}
	s.writeErr(w, http.StatusForbidden, "SCOPE", "this token lacks the scope "+scope, nil)
	return false
}

func randString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = tokenAlphabet[int(b[i])%len(tokenAlphabet)]
	}
	return string(b)
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// bearerSession authenticates an Authorization: Bearer request.
func (s *Server) bearerSession(w http.ResponseWriter, r *http.Request, bearer string) (Session, bool) {
	deny := func(msg string) (Session, bool) {
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", msg, nil)
		return Session{}, false
	}
	if key, ok := strings.CutPrefix(bearer, gatewayPrefix); ok {
		if len(s.CLIKey) == 0 || subtle.ConstantTimeCompare([]byte(key), s.CLIKey) != 1 {
			return deny("invalid gateway key")
		}
		name := r.Header.Get("X-DevPod-User")
		var u devpodv1alpha1.User
		if name == "" || s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &u) != nil {
			return deny("unknown user")
		}
		return Session{User: name, Admin: s.adminFor(name, &u)}, true
	}
	rest, ok := strings.CutPrefix(bearer, tokenPrefix)
	id, secret, ok2 := strings.Cut(rest, "_")
	if !ok || !ok2 || id == "" || secret == "" {
		return deny("malformed token")
	}
	var users devpodv1alpha1.UserList
	if err := s.Client.List(r.Context(), &users); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return Session{}, false
	}
	var owner *devpodv1alpha1.User
	var tok *devpodv1alpha1.APIToken
	for i := range users.Items {
		for j := range users.Items[i].Spec.APITokens {
			if users.Items[i].Spec.APITokens[j].ID == id {
				owner, tok = &users.Items[i], &users.Items[i].Spec.APITokens[j]
			}
		}
	}
	if tok == nil || subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(tok.Hash)) != 1 {
		return deny("invalid token")
	}
	if tok.ExpiresAt != nil && !time.Now().Before(tok.ExpiresAt.Time) {
		return deny("this token has expired")
	}
	g := &TokenGrant{ID: tok.ID, Scopes: tok.Scopes, DevPods: tok.DevPods}
	need, listed := routeScope[r.Pattern]
	switch {
	case !listed || need == scopeNone:
		s.writeErr(w, http.StatusForbidden, "SCOPE", "this endpoint is not available to access tokens", nil)
		return Session{}, false
	case need == scopePatch:
		if !g.has("devpods:power") && !g.has("devpods:update") {
			s.writeErr(w, http.StatusForbidden, "SCOPE", "this token lacks the scope devpods:power or devpods:update", nil)
			return Session{}, false
		}
	case need != scopeAny && !g.has(need):
		s.writeErr(w, http.StatusForbidden, "SCOPE", "this token lacks the scope "+need, nil)
		return Session{}, false
	}
	// a token limited to some DevPods only reaches routes that name one
	// (and the plain list, which is filtered)
	if len(g.DevPods) > 0 {
		name := r.PathValue("name")
		onDevPod := strings.HasPrefix(r.Pattern, "GET /api/devpods/{name}") || strings.Contains(r.Pattern, " /api/devpods/{name}")
		open := r.Pattern == "GET /api/me" || r.Pattern == "GET /api/devpods" || r.Pattern == "GET /api/templates" || r.Pattern == "GET /api/openapi.json"
		if !open && !(onDevPod && slices.Contains(g.DevPods, name)) {
			s.writeErr(w, http.StatusForbidden, "SCOPE", "this token is limited to specific DevPods", nil)
			return Session{}, false
		}
	}
	// admin rights need the admin scopes: reading with admin:read,
	// changing anything with admin:write
	admin := s.adminFor(owner.Name, owner) && len(g.DevPods) == 0
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		admin = admin && (g.has("admin:read") || g.has("admin:write"))
	} else {
		admin = admin && g.has("admin:write")
	}
	return Session{User: owner.Name, Admin: admin, Token: g}, true
}

/* ---- managing tokens (browser session only) ---- */

type tokenView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	DevPods   []string   `json:"devpods"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Expired   bool       `json:"expired"`
}

func viewToken(t devpodv1alpha1.APIToken, now time.Time) tokenView {
	v := tokenView{ID: t.ID, Name: t.Name, Scopes: append([]string{}, t.Scopes...), DevPods: append([]string{}, t.DevPods...), CreatedAt: t.CreatedAt.Time}
	if t.ExpiresAt != nil {
		e := t.ExpiresAt.Time
		v.ExpiresAt, v.Expired = &e, !now.Before(e)
	}
	return v
}

// browserSession refuses token and gateway sessions: tokens are managed
// only by a person signed in to the console.
func (s *Server) browserSession(w http.ResponseWriter, r *http.Request) (Session, bool) {
	if r.Header.Get("Authorization") != "" {
		s.writeErr(w, http.StatusForbidden, "SCOPE", "access tokens are managed in the console", nil)
		return Session{}, false
	}
	return s.sessionFrom(w, r)
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	var u devpodv1alpha1.User
	if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	now := time.Now()
	items := make([]tokenView, 0, len(u.Spec.APITokens))
	for _, t := range u.Spec.APITokens {
		items = append(items, viewToken(t, now))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	scopes := make([]Scope, 0, len(Scopes))
	for _, sc := range Scopes {
		if !sc.Admin || sess.Admin {
			scopes = append(scopes, sc)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items, "scopes": scopes})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Name          string   `json:"name"`
		Scopes        []string `json:"scopes"`
		DevPods       []string `json:"devpods"`
		ExpiresInDays int      `json:"expiresInDays"` // 0 = never
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	bad := func(msg string) { s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", msg, nil) }
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 64 {
		bad("a token needs a name of at most 64 characters")
		return
	}
	if len(req.Scopes) == 0 {
		bad("pick at least one scope")
		return
	}
	for _, sc := range req.Scopes {
		if !validScope(sc) {
			bad(fmt.Sprintf("unknown scope %q", sc))
			return
		}
		if strings.HasPrefix(sc, "admin:") && !sess.Admin {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "admin scopes are for admins", nil)
			return
		}
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 || len(req.DevPods) > 100 {
		bad("invalid expiry or DevPod list")
		return
	}
	if len(req.DevPods) > 0 && slices.ContainsFunc(req.Scopes, func(sc string) bool { return strings.HasPrefix(sc, "admin:") }) {
		bad("a token limited to specific DevPods cannot carry admin scopes")
		return
	}
	id, secret := randString(10), randString(40)
	now := time.Now()
	tok := devpodv1alpha1.APIToken{ID: id, Name: req.Name, Hash: hashSecret(secret), Scopes: slices.Compact(slices.Sorted(slices.Values(req.Scopes))),
		DevPods: slices.Compact(slices.Sorted(slices.Values(req.DevPods))), CreatedAt: metav1.NewTime(now.UTC().Truncate(time.Second))}
	if req.ExpiresInDays > 0 {
		e := metav1.NewTime(now.Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour).UTC().Truncate(time.Second))
		tok.ExpiresAt = &e
	}
	full := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var u devpodv1alpha1.User
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
			return err
		}
		if full = len(u.Spec.APITokens) >= maxTokens; full {
			return nil
		}
		u.Spec.APITokens = append(u.Spec.APITokens, tok)
		return s.Client.Update(r.Context(), &u)
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if full {
		s.writeErr(w, http.StatusConflict, "TOO_MANY", fmt.Sprintf("at most %d tokens; revoke one first", maxTokens), nil)
		return
	}
	// the only time the token itself is returned
	s.writeJSON(w, http.StatusCreated, map[string]any{"token": tokenPrefix + id + "_" + secret, "info": viewToken(tok, now)})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	id, found := r.PathValue("id"), false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var u devpodv1alpha1.User
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
			return err
		}
		n := len(u.Spec.APITokens)
		u.Spec.APITokens = slices.DeleteFunc(u.Spec.APITokens, func(t devpodv1alpha1.APIToken) bool { return t.ID == id })
		if found = len(u.Spec.APITokens) != n; !found {
			return nil
		}
		return s.Client.Update(r.Context(), &u)
	})
	switch {
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	case !found:
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such token", nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleOpenAPI serves GET /api/openapi.json: every endpoint with its
// summary and the scope a token needs (x-scope), generated from apiRoutes.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	paths := map[string]map[string]any{}
	for _, rt := range apiRoutes {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		op := map[string]any{"summary": rt.Summary, "responses": map[string]any{"200": map[string]any{"description": "OK"}, "default": map[string]any{"description": "Error: {code, message, detail}"}}}
		switch rt.Scope {
		case scopeNone:
			op["x-scope"] = "browser session only"
		case scopeAny:
			op["x-scope"] = "any token"
		default:
			op["x-scope"] = rt.Scope
		}
		var params []map[string]any
		for _, seg := range strings.Split(path, "/") {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				params = append(params, map[string]any{"name": strings.Trim(seg, "{}"), "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			}
		}
		if params != nil {
			op["parameters"] = params
		}
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][strings.ToLower(method)] = op
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{"title": "DevPod API", "version": "v1",
			"description": "Authenticate with a personal access token created in the console (设置 → 访问令牌): `Authorization: Bearer dpt_…`. Each operation's x-scope names the scope the token needs. Errors are JSON {code, message, detail}."},
		"components": map[string]any{"securitySchemes": map[string]any{"token": map[string]string{"type": "http", "scheme": "bearer"}}},
		"security":   []map[string][]string{{"token": {}}},
		"paths":      paths,
	})
}
