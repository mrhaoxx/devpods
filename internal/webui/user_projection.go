// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"errors"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// In a console of several clusters a user is the console's: keys,
// password, tokens, quotas and the admin flag never leave it. A cluster's
// controller needs one thing of a user — the registries they saved
// credentials for, to keep the pull secret — so that, and only that, is
// written into each cluster as a User of the same name.
//
// A credential's password is in the console's hands only while the
// request that saves it is being served, so it goes to the clusters then:
// to those connected at that moment. The answer says which were not; the
// user saves the credential again when they are back.

// projectRegistries writes the user's registries into every cluster, with
// auth (when not nil) as the credential on its way to the pull secret.
// The result says, per cluster: "ok", "offline" or "failed".
func (s *Server) projectRegistries(ctx context.Context, user string, registries []devpodv1alpha1.UserRegistry, auth *devpodv1alpha1.UserRegistryAuth) map[string]string {
	out := map[string]string{}
	for _, name := range s.clusterNames(ctx) {
		out[name] = "offline" // until shown otherwise: registered, never connected
	}
	s.Clusters.Each(func(cc *clusters.ClusterClients) {
		if _, stale := s.Clusters.Stale(cc.Name); stale {
			out[cc.Name] = "offline"
			return
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var u devpodv1alpha1.User
			err := cc.Reader.Get(ctx, types.NamespacedName{Name: user}, &u)
			create := apierrors.IsNotFound(err)
			if err != nil && !create {
				return err
			}
			if create {
				if len(registries) == 0 {
					return nil // nothing to say of someone the cluster does not know
				}
				u = devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: user}}
			}
			u.Spec.Registries = slices.Clone(registries)
			u.Spec.RegistryAuth = slices.DeleteFunc(u.Spec.RegistryAuth, func(a devpodv1alpha1.UserRegistryAuth) bool {
				listed := slices.ContainsFunc(registries, func(r devpodv1alpha1.UserRegistry) bool { return r.Host == a.Host })
				return !listed || (auth != nil && a.Host == auth.Host)
			})
			if auth != nil {
				u.Spec.RegistryAuth = append(u.Spec.RegistryAuth, *auth)
			}
			if create {
				return cc.Client.Create(ctx, &u)
			}
			return cc.Client.Update(ctx, &u)
		})
		switch {
		case err == nil:
			out[cc.Name] = "ok"
		case errors.Is(err, clusters.ErrClusterOffline):
			out[cc.Name] = "offline"
		default:
			out[cc.Name] = "failed" // the cause could echo the object, password included: not passed on
		}
	})
	return out
}
