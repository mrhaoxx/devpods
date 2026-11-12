// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// maxBatchUsers bounds one POST /api/admin/users:batch.
const maxBatchUsers = 500

// generatedPasswordLen is the length of passwords the server makes up
// (batch reset, CSV import without a password).
const generatedPasswordLen = 16

type batchRequest struct {
	Names []string `json:"names"`
	Op    string   `json:"op"`
	Args  struct {
		Quota  *quotaPatch     `json:"quota,omitempty"`
		Policy json.RawMessage `json:"policy,omitempty"` // null clears
	} `json:"args"`
}

// BatchResult is one user's outcome.
type BatchResult struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Password   string `json:"password,omitempty"`   // resetPassword: shown once
	Hibernated int    `json:"hibernated,omitempty"` // hibernateAll
}

var errUserNotFound = errors.New("user not found")

// updateUser re-reads the User and applies mut until the write does not
// conflict.
func (s *Server) updateUser(ctx context.Context, name string, mut func(*devpodv1alpha1.User) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var u devpodv1alpha1.User
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &u); err != nil {
			return err
		}
		if err := mut(&u); err != nil {
			return err
		}
		return s.Client.Update(ctx, &u)
	})
}

// generatePassword returns n random characters from [A-Za-z0-9].
func generatePassword(n int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[k.Int64()]
	}
	return string(b), nil
}

// handleUsersBatch serves POST /api/admin/users:batch (UI spec §5.5).
// Users are processed one after another; one user's failure does not
// stop the rest.
func (s *Server) handleUsersBatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req batchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	if len(req.Names) == 0 || len(req.Names) > maxBatchUsers {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "names must list 1 to 500 users", nil)
		return
	}
	var apply func(ctx context.Context, name string) BatchResult
	switch req.Op {
	case "setQuota":
		if req.Args.Quota == nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "setQuota needs args.quota", nil)
			return
		}
		q, err := req.Args.Quota.build()
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
		apply = func(ctx context.Context, name string) BatchResult {
			return result(name, s.updateUser(ctx, name, func(u *devpodv1alpha1.User) error {
				u.Spec.Quota = q.DeepCopy()
				return nil
			}))
		}
	case "setPolicy":
		if req.Args.Policy == nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "setPolicy needs args.policy (null clears)", nil)
			return
		}
		var pol *devpodv1alpha1.AuditPolicy
		pd := json.NewDecoder(bytes.NewReader(req.Args.Policy))
		pd.DisallowUnknownFields()
		if err := pd.Decode(&pol); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed policy: "+err.Error(), nil)
			return
		}
		sess, _ := s.sessionFrom(w, r)
		apply = func(ctx context.Context, name string) BatchResult {
			res := result(name, s.setAuditPolicy(ctx, name, pol.DeepCopy()))
			if res.OK && s.Audit != nil { // the access log lives in the audit store
				_ = s.insertAccess(r, sess.User, "edit_policy", "", "user/"+name)
			}
			return res
		}
	case "resetPassword":
		if !s.PasswordAuth {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "password login is disabled", nil)
			return
		}
		apply = func(ctx context.Context, name string) BatchResult {
			pw, err := generatePassword(generatedPasswordLen)
			if err != nil {
				return result(name, err)
			}
			hash, err := HashPassword(pw, s.PasswordMinLength)
			if err != nil {
				return result(name, err)
			}
			res := result(name, s.updateUser(ctx, name, func(u *devpodv1alpha1.User) error {
				u.Spec.PasswordHash = hash
				return nil
			}))
			if res.OK {
				res.Password = pw
			}
			return res
		}
	case "hibernateAll":
		apply = s.hibernateAll
	default:
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "op must be setQuota, setPolicy, resetPassword or hibernateAll", nil)
		return
	}
	seen := map[string]bool{}
	results := make([]BatchResult, 0, len(req.Names))
	for _, name := range req.Names {
		if seen[name] {
			continue
		}
		seen[name] = true
		results = append(results, apply(r.Context(), name))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// hibernateAll stops every running DevPod the user owns. Users without a
// User object (LDAP) can own DevPods too, so none is required.
func (s *Server) hibernateAll(ctx context.Context, owner string) BatchResult {
	res := BatchResult{Name: owner, OK: true}
	for _, cc := range s.ksc(ctx) { // in every cluster
		one := s.hibernateIn(s.in(ctx, cc), owner)
		res.Hibernated += one.Hibernated
		if !one.OK {
			res.OK, res.Error = false, clusterPrefix(cc.Name)+one.Error
		}
	}
	return res
}

// hibernateIn is hibernateAll in the cluster of ctx.
func (s *Server) hibernateIn(ctx context.Context, owner string) BatchResult {
	var list devpodv1alpha1.DevPodList
	if err := s.kc(ctx).Client.List(ctx, &list, client.InNamespace(s.NS)); err != nil {
		return result(owner, err)
	}
	res := BatchResult{Name: owner, OK: true}
	for i := range list.Items {
		dp := &list.Items[i]
		if dp.Spec.Owner != owner || !dp.Spec.Running {
			continue
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur devpodv1alpha1.DevPod
			if err := s.kc(ctx).Reader.Get(ctx, client.ObjectKeyFromObject(dp), &cur); err != nil {
				return err
			}
			if !cur.Spec.Running {
				return nil
			}
			cur.Spec.Running = false
			return s.kc(ctx).Client.Update(ctx, &cur)
		})
		if err != nil {
			res.OK, res.Error = false, dp.Name+": "+err.Error()
			continue
		}
		res.Hibernated++
	}
	return res
}

func result(name string, err error) BatchResult {
	switch {
	case err == nil:
		return BatchResult{Name: name, OK: true}
	case apierrors.IsNotFound(err):
		return BatchResult{Name: name, Error: errUserNotFound.Error()}
	default:
		return BatchResult{Name: name, Error: err.Error()}
	}
}
