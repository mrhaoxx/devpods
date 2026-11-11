// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// splitRef splits "host:5000/repo/name:tag" into the repository and the
// tag ("latest" when there is none). Digest references are not valid
// snapshot targets.
func splitRef(ref string) (repo, tag string, err error) {
	if ref == "" || strings.Contains(ref, "@") {
		return "", "", errors.New("the target image must be a name with an optional tag")
	}
	slash := strings.LastIndexByte(ref, '/')
	if colon := strings.LastIndexByte(ref, ':'); colon > slash {
		return ref[:colon], ref[colon+1:], nil
	}
	return ref, "latest", nil
}

// registryOf returns the registry host of an image reference the way
// Docker reads it: the first path component if it looks like a host,
// else Docker Hub.
func registryOf(ref string) string {
	first, _, ok := strings.Cut(ref, "/")
	if !ok || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		return "docker.io"
	}
	return first
}

// registryKey normalises a config.json "auths" key or a registry host:
// no scheme, no path; Docker Hub's aliases become one name.
func registryKey(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s, _, _ = strings.Cut(s, "/")
	switch s {
	case "index.docker.io", "registry-1.docker.io", "docker.io":
		return "docker.io"
	}
	return s
}

// registryAuth is the JSON carried (base64url) in X-Registry-Auth.
type registryAuth struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

// loadRegistryAuth reads the credentials for ref's registry from
// <dir>/config.json. No file, or no entry for the registry, is not an
// error: the push then goes out anonymously.
func loadRegistryAuth(dir, ref string) (registryAuth, error) {
	var out registryAuth
	if dir == "" {
		return out, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var cfg struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return out, errors.New("the push credentials (config.json) are not valid JSON")
	}
	want := registryKey(registryOf(ref))
	for key, a := range cfg.Auths {
		if registryKey(key) != want {
			continue
		}
		out = registryAuth{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken, ServerAddress: registryOf(ref)}
		if out.Username == "" && a.Auth != "" {
			if dec, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
				out.Username, out.Password, _ = strings.Cut(string(dec), ":")
			}
		}
		return out, nil
	}
	return out, nil
}

// header encodes the credentials for X-Registry-Auth.
func (a registryAuth) header() string {
	b, _ := json.Marshal(a)
	return base64.URLEncoding.EncodeToString(b)
}
