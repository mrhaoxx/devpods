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
	"sort"
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

// registryAuth is the JSON carried in X-Registry-Auth, base64url encoded
// with padding (RFC 4648 §5; the daemon decodes with base64.URLEncoding).
// serveraddress is "a domain/IP without a protocol". With no credentials
// the header is still sent, as "{}" — the docker CLI does the same, and
// without the header the daemon tries to read the auth from the body.
type registryAuth struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

// loadRegistryAuth reads the credentials for ref's registry from
// <dir>/config.json the way the docker CLI does: the entry whose key is
// the registry exactly (Docker Hub: "https://index.docker.io/v1/"), else
// one whose key reduces to it once scheme and path are dropped; an
// "auth" value (base64 "user:password") wins over separate fields. No
// file, or no entry for the registry, is not an error: the push then
// goes out anonymously. Credential helpers (credsStore / credHelpers)
// cannot run here: a registry that would need one is an error.
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
	type entry struct {
		Auth          string `json:"auth"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		IdentityToken string `json:"identitytoken"`
	}
	var cfg struct {
		Auths       map[string]entry  `json:"auths"`
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return out, errors.New("the push credentials (config.json) are not valid JSON")
	}
	host := registryOf(ref)
	want := registryKey(host)
	exact := host
	if want == "docker.io" {
		exact = "https://index.docker.io/v1/"
	}
	a, found := cfg.Auths[exact]
	if !found {
		keys := make([]string, 0, len(cfg.Auths))
		for key := range cfg.Auths {
			keys = append(keys, key)
		}
		sort.Strings(keys) // a stable choice when several keys reduce to the host
		for _, key := range keys {
			if registryKey(key) == want {
				a, found = cfg.Auths[key], true
				break
			}
		}
	}
	if !found || (a.Auth == "" && a.Username == "" && a.IdentityToken == "") {
		for key := range cfg.CredHelpers {
			if registryKey(key) == want {
				return out, errors.New("the push credentials for " + host + " need a credential helper, which is not supported here")
			}
		}
		if cfg.CredsStore != "" {
			return out, errors.New("the push credentials use a credential store (credsStore), which is not supported here")
		}
		return out, nil
	}
	out = registryAuth{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken, ServerAddress: host}
	if a.Auth != "" {
		dec, err := base64.StdEncoding.DecodeString(a.Auth)
		user, pass, ok := strings.Cut(string(dec), ":")
		if err != nil || !ok {
			return out, errors.New("the push credentials for " + host + " are malformed (auth must be base64 of user:password)")
		}
		out.Username, out.Password = user, pass
	}
	return out, nil
}

// header encodes the credentials for X-Registry-Auth.
func (a registryAuth) header() string {
	b, _ := json.Marshal(a)
	return base64.URLEncoding.EncodeToString(b)
}
