// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package httpauth is what the webui and the gateway share for the
// DevPod HTTP proxy: the DevPod's random id and public ports (stored as
// annotations), who may open a private port, and the HMAC-signed tokens
// that carry a webui login over to a proxy hostname.
package httpauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const (
	// IDAnnotation holds the DevPod's random id: the only thing a proxy
	// URL reveals. Replacing it invalidates every link handed out.
	IDAnnotation = "devpod.io/http-id"
	// PublicPortsAnnotation lists the ports open without a login, "8080,8888".
	PublicPortsAnnotation = "devpod.io/http-public-ports"
	// UserPortsAnnotation lists the ports open to anyone signed in to the
	// console (not only the owner and collaborators).
	UserPortsAnnotation = "devpod.io/http-user-ports"

	// AuthPath on a proxy hostname takes the webui's ticket and sets the cookie.
	AuthPath = "/__devpod/auth"
	// CookieName is the gateway's per-hostname login cookie.
	CookieName = "__devpod_http"

	KindTicket = "t" // webui → gateway, single redirect, short-lived
	KindCookie = "c" // gateway's own cookie

	TicketTTL = time.Minute
	CookieTTL = 12 * time.Hour

	idAlphabet = "abcdefghijklmnopqrstuvwxyz234567" // no hyphen: the port follows the last one
	idLen      = 12
)

// NewID returns a random 60-bit id.
func NewID() string {
	b := make([]byte, idLen)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// ValidID reports whether s has the shape NewID produces.
func ValidID(s string) bool {
	if len(s) != idLen {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(idAlphabet, c) {
			return false
		}
	}
	return true
}

// Access levels of a port, from the most open.
const (
	AccessPublic  = "public"  // anyone with the link
	AccessUsers   = "users"   // anyone signed in to the console
	AccessPrivate = "private" // the owner, collaborators and admins
)

// PublicPorts parses the DevPod's public-port annotation.
func PublicPorts(dp *devpodv1alpha1.DevPod) []int { return portsOf(dp, PublicPortsAnnotation) }

// UserPorts parses the ports open to every signed-in user.
func UserPorts(dp *devpodv1alpha1.DevPod) []int { return portsOf(dp, UserPortsAnnotation) }

// Access is the port's level; public wins when a port is listed twice.
func Access(dp *devpodv1alpha1.DevPod, port int) string {
	switch {
	case slices.Contains(PublicPorts(dp), port):
		return AccessPublic
	case slices.Contains(UserPorts(dp), port):
		return AccessUsers
	}
	return AccessPrivate
}

// MayOpen: whether user ("" = not signed in) may reach the port.
func MayOpen(dp *devpodv1alpha1.DevPod, port int, user string, admin bool) bool {
	switch Access(dp, port) {
	case AccessPublic:
		return true
	case AccessUsers:
		return user != ""
	}
	return Allowed(dp, user, admin)
}

func portsOf(dp *devpodv1alpha1.DevPod, annotation string) []int {
	var out []int
	for _, f := range strings.Split(dp.Annotations[annotation], ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n >= 1 && n <= 65535 && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// FormatPorts is PublicPorts' inverse.
func FormatPorts(ports []int) string {
	ports = slices.Clone(ports)
	sort.Ints(ports)
	s := make([]string, 0, len(ports))
	for _, p := range slices.Compact(ports) {
		s = append(s, strconv.Itoa(p))
	}
	return strings.Join(s, ",")
}

// Allowed: the owner, a collaborator or an admin may open a private port.
func Allowed(dp *devpodv1alpha1.DevPod, user string, admin bool) bool {
	return user != "" && (admin || dp.Spec.Owner == user || slices.Contains(dp.Spec.Collaborators, user))
}

// Claims is a signed statement that User may reach the DevPod with ID.
type Claims struct {
	Kind  string `json:"k"`
	ID    string `json:"i"`
	User  string `json:"u"`
	Admin bool   `json:"a,omitempty"`
	Exp   int64  `json:"e"` // unix seconds
}

var (
	ErrBadToken = errors.New("httpauth: bad token")
	ErrExpired  = errors.New("httpauth: token expired")
)

var enc = base64.RawURLEncoding

func mac(key, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(body)
	return h.Sum(nil)
}

// Sign returns body.signature.
func Sign(key []byte, c Claims) string {
	body, _ := json.Marshal(c)
	return enc.EncodeToString(body) + "." + enc.EncodeToString(mac(key, body))
}

// Verify checks the signature, the kind and the expiry.
func Verify(key []byte, tok, kind string, now time.Time) (Claims, error) {
	var c Claims
	b, s, ok := strings.Cut(tok, ".")
	if !ok || len(key) == 0 {
		return c, ErrBadToken
	}
	body, err1 := enc.DecodeString(b)
	sig, err2 := enc.DecodeString(s)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, mac(key, body)) {
		return c, ErrBadToken
	}
	if err := json.Unmarshal(body, &c); err != nil || c.Kind != kind {
		return c, ErrBadToken
	}
	if now.Unix() >= c.Exp {
		return c, ErrExpired
	}
	return c, nil
}
