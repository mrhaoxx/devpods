// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command fakeidp is a minimal OIDC issuer for e2e: discovery, JWKS,
// auto-approving /auth, and a /token endpoint returning an RS256
// id_token with a fixed preferred_username. NEVER use outside tests.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"time"
)

func main() {
	var (
		listen   = flag.String("listen", ":9998", "listen address")
		issuer   = flag.String("issuer", "http://fakeidp.devpod-system.svc:9998", "issuer URL as seen by the webui")
		username = flag.String("username", "alice", "preferred_username to assert")
		clientID = flag.String("client-id", "webui-client", "audience")
	)
	flag.Parse()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}

	sign := func() string {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"e2e"}`))
		claims, _ := json.Marshal(map[string]any{
			"iss": *issuer, "aud": *clientID, "sub": "1",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"preferred_username": *username,
		})
		payload := base64.RawURLEncoding.EncodeToString(claims)
		h := sha256.Sum256([]byte(header + "." + payload))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		if err != nil {
			log.Fatal(err)
		}
		return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": *issuer, "authorization_endpoint": *issuer + "/auth",
			"token_endpoint": *issuer + "/token", "jwks_uri": *issuer + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		pub := &key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "e2e",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		v := redirect.Query()
		v.Set("code", "e2e-code")
		v.Set("state", q.Get("state"))
		redirect.RawQuery = v.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": sign(),
		})
	})

	fmt.Println("fakeidp listening on", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux)) //nolint:gosec // test-only binary
}
