// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SessionCookie is the browser cookie carrying the signed session.
const SessionCookie = "devpod_session"

// Session is the authenticated browser identity. User is the MAPPED
// DevPod username (prefix applied), not the raw GitLab login.
type Session struct {
	User   string `json:"u"`
	Admin  bool   `json:"a"`
	Expiry int64  `json:"e"` // unix seconds
}

// SessionManager mints and verifies HMAC-SHA256-signed session tokens.
// Stateless by design: any replica holding the same key verifies any
// token, so there is no server-side session store and no sticky
// sessions.
type SessionManager struct {
	key []byte
	ttl time.Duration
}

func NewSessionManager(key []byte, ttl time.Duration) *SessionManager {
	return &SessionManager{key: key, ttl: ttl}
}

// TTL returns the configured session lifetime (cookie Max-Age).
func (m *SessionManager) TTL() time.Duration { return m.ttl }

func (m *SessionManager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Mint returns "<b64url(json)>.<b64url(hmac)>".
func (m *SessionManager) Mint(user string, admin bool, now time.Time) string {
	raw, err := json.Marshal(Session{User: user, Admin: admin, Expiry: now.Add(m.ttl).Unix()})
	if err != nil {
		panic(err) // marshal of a plain struct cannot fail
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + m.sign(payload)
}

func (m *SessionManager) Verify(token string, now time.Time) (Session, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Session{}, fmt.Errorf("malformed session token")
	}
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return Session{}, fmt.Errorf("malformed session signature")
	}
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(payload))
	if !hmac.Equal(mac.Sum(nil), want) {
		return Session{}, fmt.Errorf("invalid session signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Session{}, fmt.Errorf("malformed session payload")
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return Session{}, fmt.Errorf("malformed session payload: %w", err)
	}
	if now.Unix() >= s.Expiry {
		return Session{}, fmt.Errorf("session expired")
	}
	return s, nil
}
