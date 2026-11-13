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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// A user's image registry credentials (snapshot spec v2 §5): used to pull
// private images into their DevPods and to push snapshots. The webui only
// records the registry and hands the credential to the user controller
// through spec.registryAuth; it never reads the Secret.
const maxRegistries = 20

var registryHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)

// normalizeRegistryHost returns registry[:port] in lowercase, or "" when
// the input is not a bare registry host (a scheme, a path, a user).
func normalizeRegistryHost(in string) string {
	h := strings.ToLower(strings.TrimSpace(in))
	if !registryHost.MatchString(h) {
		return ""
	}
	switch h {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io" // one name for Docker Hub
	}
	return h
}

// imageRegistryHost is the registry of an image reference, in the form
// normalizeRegistryHost produces.
func imageRegistryHost(image string) string {
	first, _, ok := strings.Cut(image, "/")
	if !ok || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io" // "name:tag" or "user/name:tag"
	}
	return normalizeRegistryHost(first)
}

type registryView struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	// Pending: the credential has not reached the Secret yet.
	Pending bool `json:"pending"`
}

func registryViews(u *devpodv1alpha1.User) []registryView {
	items := make([]registryView, 0, len(u.Spec.Registries))
	for _, reg := range u.Spec.Registries {
		items = append(items, registryView{Host: reg.Host, Username: reg.Username,
			Pending: slices.ContainsFunc(u.Spec.RegistryAuth, func(a devpodv1alpha1.UserRegistryAuth) bool { return a.Host == reg.Host })})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Host < items[j].Host })
	return items
}

func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var u devpodv1alpha1.User
	if err := s.ownUser(r.Context(), sess.User, false, &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": registryViews(&u)})
}

// handlePutRegistry serves PUT /api/me/registries/{host}: add or replace
// the credential for one registry.
func (s *Server) handlePutRegistry(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	bad := func(msg string) { s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", msg, nil) }
	host := normalizeRegistryHost(r.PathValue("host"))
	if host == "" {
		bad("the registry is a host with an optional port, like harbor.example.com or registry.example.com:5000 (no scheme, no path)")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		bad("malformed request body") // never echo the body: it carries a password
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" || len(req.Username) > 256 || len(req.Password) > 4096 {
		bad("username and password are required")
		return
	}
	full := false
	var out []registryView
	var registries []devpodv1alpha1.UserRegistry
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var u devpodv1alpha1.User
		if err := s.ownUser(r.Context(), sess.User, true, &u); err != nil {
			return err
		}
		i := slices.IndexFunc(u.Spec.Registries, func(reg devpodv1alpha1.UserRegistry) bool { return reg.Host == host })
		if full = i < 0 && len(u.Spec.Registries) >= maxRegistries; full {
			return nil
		}
		if i < 0 {
			u.Spec.Registries = append(u.Spec.Registries, devpodv1alpha1.UserRegistry{Host: host, Username: req.Username})
		} else {
			u.Spec.Registries[i].Username = req.Username
		}
		u.Spec.RegistryAuth = slices.DeleteFunc(u.Spec.RegistryAuth, func(a devpodv1alpha1.UserRegistryAuth) bool { return a.Host == host })
		if s.Clusters == nil { // with several clusters the password goes to them, not into the console's record
			u.Spec.RegistryAuth = append(u.Spec.RegistryAuth, devpodv1alpha1.UserRegistryAuth{Host: host, Username: req.Username, Password: req.Password})
		}
		out, registries = registryViews(&u), slices.Clone(u.Spec.Registries)
		return s.Client.Update(r.Context(), &u)
	})
	switch {
	case err != nil:
		// an apiserver message could echo the object, password included
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not save the credential", nil)
	case full:
		s.writeErr(w, http.StatusConflict, "TOO_MANY", fmt.Sprintf("at most %d registries; remove one first", maxRegistries), nil)
	default:
		answer := map[string]any{"items": out}
		if s.Clusters != nil {
			// cluster → ok | offline | failed: where the credential is now
			answer["clusters"] = s.projectRegistries(r.Context(), sess.User, registries,
				&devpodv1alpha1.UserRegistryAuth{Host: host, Username: req.Username, Password: req.Password})
		}
		s.writeJSON(w, http.StatusOK, answer)
	}
}

// handleDeleteRegistry serves DELETE /api/me/registries/{host}.
func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	host := normalizeRegistryHost(r.PathValue("host"))
	found := false
	var registries []devpodv1alpha1.UserRegistry
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var u devpodv1alpha1.User
		if err := s.ownUser(r.Context(), sess.User, false, &u); err != nil {
			return err
		}
		n := len(u.Spec.Registries)
		u.Spec.Registries = slices.DeleteFunc(u.Spec.Registries, func(reg devpodv1alpha1.UserRegistry) bool { return reg.Host == host })
		registries = slices.Clone(u.Spec.Registries)
		if found = len(u.Spec.Registries) != n; !found {
			return nil
		}
		u.Spec.RegistryAuth = slices.DeleteFunc(u.Spec.RegistryAuth, func(a devpodv1alpha1.UserRegistryAuth) bool { return a.Host == host })
		return s.Client.Update(r.Context(), &u)
	})
	switch {
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "could not remove the credential", nil)
	case !found && s.Clusters == nil:
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such registry", nil)
	case s.Clusters != nil:
		// also when the console no longer lists it: a cluster that was
		// away when it was removed is told now
		s.writeJSON(w, http.StatusOK, map[string]any{"clusters": s.projectRegistries(r.Context(), sess.User, registries, nil)})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// ownUser reads the caller's own User object into u. Someone whose
// identity is the LDAP directory's may have none: u is then left empty —
// nothing saved yet — unless create is set, for the first thing they
// save; the object made has no keys and no password (their SSH keys stay
// the directory's).
func (s *Server) ownUser(ctx context.Context, name string, create bool, u *devpodv1alpha1.User) error {
	err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, u)
	if !apierrors.IsNotFound(err) {
		return err
	}
	*u = devpodv1alpha1.User{}
	if !create {
		return nil
	}
	if err := s.ensureUser(ctx, name); err != nil {
		return err
	}
	return s.Reader.Get(ctx, types.NamespacedName{Name: name}, u)
}

// ensureUser makes the key-less User object of name if there is none.
func (s *Server) ensureUser(ctx context.Context, name string) error {
	err := s.Client.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
