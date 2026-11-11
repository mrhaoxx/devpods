// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// failFloor is the minimum time a failed password login takes, so the
// hash-miss and user-miss paths are indistinguishable by timing.
const failFloor = 200 * time.Millisecond

// HashPassword bcrypt-hashes pw after enforcing the minimum length.
func HashPassword(pw string, minLen int) (string, error) {
	if len(pw) < minLen {
		return "", fmt.Errorf("password must be at least %d characters", minLen)
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// VerifyPassword reports whether pw matches the bcrypt hash.
func VerifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// adminFor resolves the admin bit for a login: the --admins allowlist
// OR the User's kubectl-managed spec.admin.
func (s *Server) adminFor(username string, u *devpodv1alpha1.User) bool {
	if s.Admins[username] {
		return true
	}
	return u != nil && u.Spec.Admin
}

// setSession mints the session cookie shared by every login path.
func (s *Server) setSession(w http.ResponseWriter, username string, admin bool) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: s.Sessions.Mint(username, admin, time.Now()), Path: "/",
		MaxAge: int(s.Sessions.TTL().Seconds()), HttpOnly: true, Secure: s.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// handleLogout clears the session cookie regardless of login path.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]bool{
		// the name-and-password form: built-in passwords, the LDAP
		// directory's, or both
		"password": s.PasswordAuth || s.LDAPAuth != nil,
		"ldap":     s.LDAPAuth != nil,
		"oauth":    s.OAuth != nil,
	})
}

func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if !s.PasswordAuth && s.LDAPAuth == nil {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "password login is disabled", nil)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}

	start := time.Now()
	// Generic failure with a timing floor — never reveal whether the
	// username exists or the password was wrong.
	fail := func() {
		if d := failFloor - time.Since(start); d > 0 {
			time.Sleep(d)
		}
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid username or password", nil)
	}

	// A user may have no User object at all (their identity is the
	// directory's); userPtr stays nil then.
	var u devpodv1alpha1.User
	userPtr := &u
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: req.Username}, &u); err != nil {
		userPtr = nil
	}
	ok := s.PasswordAuth && userPtr != nil && u.Spec.PasswordHash != "" && VerifyPassword(u.Spec.PasswordHash, req.Password)
	// not a built-in password: the directory's, when there is one
	if !ok && s.LDAPAuth != nil {
		var err error
		if ok, err = s.LDAPAuth.Authenticate(r.Context(), req.Username, req.Password); err != nil {
			slog.Error("ldap login", "user", req.Username, "err", err)
			s.writeErr(w, http.StatusServiceUnavailable, "LDAP_UNAVAILABLE", "the login directory cannot be reached; try again in a moment", nil)
			return
		}
	}
	if !ok {
		fail()
		return
	}
	// A first sign-in from the directory leaves a User object (no keys, no
	// password): the person is listed among the users and has somewhere to
	// keep settings. Not having one is no reason to refuse the sign-in.
	if userPtr == nil {
		if err := s.ensureUser(r.Context(), req.Username); err != nil {
			slog.Warn("user object after ldap login", "user", req.Username, "err", err)
		}
	}
	s.setSession(w, req.Username, s.adminFor(req.Username, userPtr))
	s.writeJSON(w, http.StatusOK, map[string]any{"user": req.Username})
}

// handleChangePassword lets a password user change their own password.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	if !s.PasswordAuth {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "password login is disabled", nil)
		return
	}
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}

	var u devpodv1alpha1.User
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if u.Spec.PasswordHash == "" {
		s.writeErr(w, http.StatusConflict, "NO_PASSWORD", "this account has no password (OAuth-only)", nil)
		return
	}
	if !VerifyPassword(u.Spec.PasswordHash, req.OldPassword) {
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "current password is incorrect", nil)
		return
	}
	hash, err := HashPassword(req.NewPassword, s.PasswordMinLength)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	u.Spec.PasswordHash = hash
	if err := s.Client.Update(r.Context(), &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PasswordChecker says whether a password is a user's somewhere else
// (an LDAP directory). An error is that place being in trouble, not a
// wrong password.
type PasswordChecker interface {
	Authenticate(ctx context.Context, username, password string) (bool, error)
}
