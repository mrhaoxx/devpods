// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/ldapauth"
)

// ldapLogin builds the directory sign-in from the gateway's LDAP
// settings (GatewayConfig.spec.ldap): one place says which directory,
// which service account and which users. The CA and the service
// account's password are the same Secrets the gateway mounts, in dir.
// The settings are read once, at start.
func ldapLogin(r client.Reader, gatewayConfig, dir string) (*ldapauth.Authenticator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var gc devpodv1alpha1.GatewayConfig
	if err := r.Get(ctx, types.NamespacedName{Name: gatewayConfig}, &gc); err != nil {
		return nil, fmt.Errorf("read GatewayConfig %q: %w", gatewayConfig, err)
	}
	l := gc.Spec.LDAP
	if l == nil {
		return nil, errors.New("GatewayConfig " + gatewayConfig + " has no spec.ldap")
	}
	return ldapauth.New(ldapauth.Config{
		URL:              l.URL,
		CAPath:           filepath.Join(dir, "ca.crt"),
		BindDN:           l.BindDN,
		BindPasswordPath: filepath.Join(dir, "password"),
		BaseDN:           l.BaseDN,
		UserFilter:       l.UserFilter,
		Timeout:          time.Duration(l.RequestTimeoutSeconds) * time.Second,
	})
}
