// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// fakeIssuer is a minimal OIDC provider good enough for go-oidc:
// discovery, JWKS, and a token endpoint that returns an RS256 id_token
// for a fixed username regardless of the code presented.
type fakeIssuer struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	username string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, username: "alice"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		pub := &f.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idToken := f.signIDToken(t, "webui-client")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": idToken,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) signIDToken(t *testing.T, aud string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": f.srv.URL, "aud": aud, "sub": "42",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"preferred_username": f.username,
	})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	signing := header + "." + payload
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newOAuthForTest(t *testing.T, f *fakeIssuer, prefix string, admins []string) (*webui.OAuth, *webui.SessionManager) {
	t.Helper()
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	o, err := webui.NewOAuth(context.Background(), webui.OAuthConfig{
		IssuerURL: f.srv.URL, ClientID: "webui-client", ClientSecret: "secret",
		RedirectURL: "https://ui.example.com/auth/callback",
		UserPrefix:  prefix, Admins: admins,
	}, k8sClient, sm)
	if err != nil {
		t.Fatalf("NewOAuth: %v", err)
	}
	return o, sm
}

// loginThenCallback drives HandleLogin, carries its cookies + state
// into HandleCallback, and returns the callback response recorder.
func loginThenCallback(t *testing.T, o *webui.OAuth) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	o.HandleLogin(login, httptest.NewRequest("GET", "/auth/login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("login status = %d", login.Code)
	}
	loc, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("missing state or PKCE in %s", loc)
	}

	cb := httptest.NewRequest("GET", "/auth/callback?code=fake&state="+url.QueryEscape(state), nil)
	for _, c := range login.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	o.HandleCallback(rec, cb)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == webui.SessionCookie {
			return c
		}
	}
	return nil
}

func TestOAuth(t *testing.T) {
	setupSuite(t)
	f := newFakeIssuer(t)

	t.Run("callback mints session and provisions User", func(t *testing.T) {
		o, sm := newOAuthForTest(t, f, "gl-", nil)
		rec := loginThenCallback(t, o)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body)
		}
		c := sessionCookie(rec)
		if c == nil || !c.HttpOnly {
			t.Fatalf("session cookie missing or not HttpOnly: %+v", c)
		}
		sess, err := sm.Verify(c.Value, time.Now())
		if err != nil || sess.User != "gl-alice" || sess.Admin {
			t.Fatalf("sess=%+v err=%v", sess, err)
		}
		var u devpodv1alpha1.User
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "gl-alice"}, &u); err != nil {
			t.Fatalf("auto-provisioned User missing: %v", err)
		}
		// Second login must not fail on AlreadyExists.
		rec2 := loginThenCallback(t, o)
		if rec2.Code != http.StatusFound {
			t.Fatalf("second login status = %d", rec2.Code)
		}
	})

	t.Run("admin list keys off GitLab username", func(t *testing.T) {
		o, sm := newOAuthForTest(t, f, "gl-", []string{"alice"})
		rec := loginThenCallback(t, o)
		sess, _ := sm.Verify(sessionCookie(rec).Value, time.Now())
		if !sess.Admin {
			t.Fatal("expected admin session")
		}
	})

	t.Run("unmappable username refused", func(t *testing.T) {
		f.username = "Bad.Name"
		defer func() { f.username = "alice" }()
		o, _ := newOAuthForTest(t, f, "gl-", nil)
		rec := loginThenCallback(t, o)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "DNS-1123") {
			t.Fatalf("error page should name the reason: %s", rec.Body)
		}
	})

	t.Run("state mismatch rejected", func(t *testing.T) {
		o, _ := newOAuthForTest(t, f, "gl-", nil)
		login := httptest.NewRecorder()
		o.HandleLogin(login, httptest.NewRequest("GET", "/auth/login", nil))
		cb := httptest.NewRequest("GET", "/auth/callback?code=fake&state=WRONG", nil)
		for _, c := range login.Result().Cookies() {
			cb.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		o.HandleCallback(rec, cb)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}
