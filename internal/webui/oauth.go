// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const (
	stateCookie = "devpod_oauth_state"
	pkceCookie  = "devpod_oauth_pkce"
)

// OAuthConfig wires the GitLab OIDC client. Admins lists GitLab
// usernames (pre-prefix) that receive the admin bit.
type OAuthConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	UserPrefix   string
	Admins       []string
}

// OAuth implements the GitLab OIDC authorization-code + PKCE flow,
// maps preferred_username through the configured prefix, and
// idempotently provisions the User CR on first login.
type OAuth struct {
	oauth2Cfg oauth2.Config
	verifier  *oidc.IDTokenVerifier
	prefix    string
	admins    map[string]bool
	sessions  *SessionManager
	client    client.Client
	// secure mirrors the RedirectURL scheme: https deployments set
	// the Secure cookie attribute; plain-http (e2e port-forward, dev)
	// must not, or curl/browsers will refuse to send the cookies.
	secure bool
}

func NewOAuth(ctx context.Context, cfg OAuthConfig, c client.Client, sm *SessionManager) (*OAuth, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery %s: %w", cfg.IssuerURL, err)
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil {
		return nil, fmt.Errorf("redirect url: %w", err)
	}
	admins := make(map[string]bool, len(cfg.Admins))
	for _, a := range cfg.Admins {
		admins[a] = true
	}
	return &OAuth{
		secure: redirect.Scheme == "https",
		oauth2Cfg: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "profile"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		prefix:   cfg.UserPrefix,
		admins:   admins,
		sessions: sm,
		client:   c,
	}, nil
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// flowCookie scopes the short-lived state/PKCE cookies to /auth.
func (o *OAuth) flowCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/auth",
		MaxAge: maxAge, HttpOnly: true, Secure: o.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (o *OAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state := randToken()
	pkce := oauth2.GenerateVerifier()
	http.SetCookie(w, o.flowCookie(stateCookie, state, 600))
	http.SetCookie(w, o.flowCookie(pkceCookie, pkce, 600))
	http.Redirect(w, r, o.oauth2Cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(pkce)), http.StatusFound)
}

func (o *OAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	stateC, err1 := r.Cookie(stateCookie)
	pkceC, err2 := r.Cookie(pkceCookie)
	if err1 != nil || err2 != nil || r.URL.Query().Get("state") != stateC.Value {
		http.Error(w, "OAuth state mismatch — restart login", http.StatusBadRequest)
		return
	}
	// Consume the flow cookies.
	http.SetCookie(w, o.flowCookie(stateCookie, "", -1))
	http.SetCookie(w, o.flowCookie(pkceCookie, "", -1))

	token, err := o.oauth2Cfg.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(pkceC.Value))
	if err != nil {
		slog.Error("oauth exchange failed", "err", err)
		http.Error(w, "token exchange with GitLab failed", http.StatusBadGateway)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "GitLab response missing id_token", http.StatusBadGateway)
		return
	}
	idToken, err := o.verifier.Verify(r.Context(), rawID)
	if err != nil {
		slog.Error("id_token verify failed", "err", err)
		http.Error(w, "invalid id_token", http.StatusBadGateway)
		return
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "malformed claims", http.StatusBadGateway)
		return
	}

	mapped, err := MapUsername(o.prefix, claims.PreferredUsername)
	if err != nil {
		// Explicit refusal page naming the reason (spec §3).
		http.Error(w, "login refused: "+err.Error(), http.StatusForbidden)
		return
	}
	if err := o.ensureUser(r.Context(), mapped); err != nil {
		slog.Error("auto-provision failed", "user", mapped, "err", err)
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
		return
	}

	tok := o.sessions.Mint(mapped, o.admins[claims.PreferredUsername], time.Now())
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: tok, Path: "/",
		MaxAge: int(o.sessions.TTL().Seconds()), HttpOnly: true, Secure: o.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (o *OAuth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: o.secure, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ensureUser idempotently creates the keyless User CR on first login
// (nil quota = global defaults; pubkeys added later via the UI).
func (o *OAuth) ensureUser(ctx context.Context, name string) error {
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := o.client.Create(ctx, u); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
