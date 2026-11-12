// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/httpauth"
)

// HTTPProxy configures the console's side of the gateway's HTTP proxy
// (ports of a DevPod published as {id}-{port} hostnames): nil on Server
// turns the feature off.
type HTTPProxy struct {
	// HostTemplate is the proxy hostname with {id} and {port}, e.g.
	// "{id}-{port}-devpod.clusters.zjusct.io" (it must match the
	// gateway's --http-base-domain / --http-subdomain-suffix).
	HostTemplate string
	Scheme       string // "https" unless testing
	Key          []byte // shared with the gateway
	// Port is the port of the public entry when it is not the scheme's
	// own (https://…:4430): the links the console gives carry it. A
	// host is recognised whatever port comes with it.
	Port int

	host *regexp.Regexp
}

// NewHTTPProxy validates the template.
func NewHTTPProxy(hostTemplate, scheme string, key []byte) (*HTTPProxy, error) {
	if strings.Count(hostTemplate, "{id}") != 1 || strings.Count(hostTemplate, "{port}") != 1 {
		return nil, fmt.Errorf("http proxy host %q must contain {id} and {port} once each", hostTemplate)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("http proxy needs the key shared with the gateway")
	}
	if scheme == "" {
		scheme = "https"
	}
	pat := regexp.QuoteMeta(hostTemplate)
	pat = strings.Replace(pat, regexp.QuoteMeta("{id}"), `([a-z2-7]+)`, 1)
	pat = strings.Replace(pat, regexp.QuoteMeta("{port}"), `([0-9]{1,5})`, 1)
	idFirst := strings.Index(hostTemplate, "{id}") < strings.Index(hostTemplate, "{port}")
	if !idFirst {
		return nil, fmt.Errorf("http proxy host %q: {id} must come before {port}", hostTemplate)
	}
	return &HTTPProxy{HostTemplate: hostTemplate, Scheme: scheme, Key: key, host: regexp.MustCompile("^" + pat + "$")}, nil
}

// parseHost returns the id and port of a proxy hostname.
func (h *HTTPProxy) parseHost(host string) (string, int, bool) {
	m := h.host.FindStringSubmatch(bareHost(host))
	if m == nil || !httpauth.ValidID(m[1]) {
		return "", 0, false
	}
	port, err := strconv.Atoi(m[2])
	return m[1], port, err == nil && port >= 1 && port <= 65535
}

// bareHost is host in lower case without its port, when what follows the
// colon is a port: anything else is left on, and then matches nothing.
func bareHost(host string) string {
	host = strings.ToLower(host)
	if h, port, err := net.SplitHostPort(host); err == nil {
		if n, err := strconv.Atoi(port); err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == port {
			return h
		}
	}
	return host
}

// authority is host as the public entry serves it: with the entry's port
// when it has one of its own.
func (h *HTTPProxy) authority(host string) string {
	if h.Port == 0 {
		return host
	}
	return host + ":" + strconv.Itoa(h.Port)
}

// httpView is what the ports panel shows.
type httpView struct {
	ID string `json:"id"`
	// URLTemplate has the id filled in and {port} left for the caller.
	URLTemplate string `json:"urlTemplate"`
	PublicPorts []int  `json:"publicPorts"`
	// UserPorts are open to anyone signed in to the console.
	UserPorts []int `json:"userPorts"`
}

func (h *HTTPProxy) view(dp *devpodv1alpha1.DevPod) httpView {
	id := dp.Annotations[httpauth.IDAnnotation]
	ports := httpauth.PublicPorts(dp)
	if ports == nil {
		ports = []int{}
	}
	users := httpauth.UserPorts(dp)
	if users == nil {
		users = []int{}
	}
	return httpView{ID: id, URLTemplate: h.Scheme + "://" + h.authority(strings.Replace(h.HostTemplate, "{id}", id, 1)), PublicPorts: ports, UserPorts: users}
}

func (s *Server) httpProxy(w http.ResponseWriter) (*HTTPProxy, bool) {
	if s.HTTPProxy == nil {
		s.writeErr(w, http.StatusNotFound, "HTTP_PROXY_DISABLED", "the HTTP proxy is not enabled", nil)
		return nil, false
	}
	return s.HTTPProxy, true
}

// httpProxyIn is the port-link address scheme for the request's cluster:
// that of the gateway serving it, else the console's own. Called once
// the request's cluster is settled.
func (s *Server) httpProxyIn(r *http.Request, h *HTTPProxy) *HTTPProxy {
	if s.Clusters == nil {
		return h
	}
	if gh := s.proxyOfGateway(s.gatewayOf(r.Context(), s.k(r).Name)); gh != nil {
		return gh
	}
	return h
}

// annotate rewrites the DevPod's annotations with fn, retrying conflicts.
func (s *Server) annotate(r *http.Request, dp *devpodv1alpha1.DevPod, fn func(a map[string]string)) (*devpodv1alpha1.DevPod, error) {
	for attempt := 0; ; attempt++ {
		cp := dp.DeepCopy()
		if cp.Annotations == nil {
			cp.Annotations = map[string]string{}
		}
		fn(cp.Annotations)
		err := s.k(r).Client.Update(r.Context(), cp)
		if err == nil {
			return cp, nil
		}
		if !apierrors.IsConflict(err) || attempt >= 4 {
			return nil, err
		}
		fresh := &devpodv1alpha1.DevPod{}
		if err := s.k(r).Reader.Get(r.Context(), client.ObjectKeyFromObject(dp), fresh); err != nil {
			return nil, err
		}
		dp = fresh
	}
}

// handleGetHTTP serves GET /api/devpods/{name}/http; the DevPod gets its
// random id the first time someone looks.
func (s *Server) handleGetHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	h, ok := s.httpProxy(w)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	if dp.Annotations[httpauth.IDAnnotation] == "" {
		var err error
		if dp, err = s.annotate(r, dp, func(a map[string]string) {
			if a[httpauth.IDAnnotation] == "" {
				a[httpauth.IDAnnotation] = httpauth.NewID()
			}
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, s.httpProxyIn(r, h).view(dp))
}

// handlePutHTTP serves PUT /api/devpods/{name}/http: {"publicPorts":[…]} and
// {"userPorts":[…]} (ports open to every signed-in user)
// replaces the public ports; {"reset":true} draws a new id, which kills
// every link handed out so far.
func (s *Server) handlePutHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	h, ok := s.httpProxy(w)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var req struct {
		PublicPorts *[]int `json:"publicPorts"`
		UserPorts   *[]int `json:"userPorts"`
		Reset       bool   `json:"reset"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	for _, list := range []*[]int{req.PublicPorts, req.UserPorts} {
		if list == nil {
			continue
		}
		if len(*list) > 32 {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "at most 32 ports per access level", nil)
			return
		}
		for _, p := range *list {
			if p < 1 || p > 65535 {
				s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("invalid port %d", p), nil)
				return
			}
		}
	}
	dp, err := s.annotate(r, dp, func(a map[string]string) {
		if req.Reset || a[httpauth.IDAnnotation] == "" {
			a[httpauth.IDAnnotation] = httpauth.NewID()
		}
		set := func(key string, ports []int) {
			if v := httpauth.FormatPorts(ports); v != "" {
				a[key] = v
			} else {
				delete(a, key)
			}
		}
		if req.PublicPorts != nil {
			set(httpauth.PublicPortsAnnotation, *req.PublicPorts)
		}
		if req.UserPorts != nil {
			set(httpauth.UserPortsAnnotation, *req.UserPorts)
		}
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, s.httpProxyIn(r, h).view(dp))
}

// handleHTTPTicket serves POST /api/http/ticket {host, rd}: the logged-in
// user asks for access to a private port. The answer is the URL on that
// proxy hostname which trades a one-minute ticket for the gateway's
// cookie. Only hostnames of the configured shape are answered, so a
// ticket never leaves for another site.
func (s *Server) handleHTTPTicket(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	h, ok := s.httpProxy(w)
	if !ok {
		return
	}
	var req struct {
		Host string `json:"host"`
		RD   string `json:"rd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body", nil)
		return
	}
	// the address may be the console's own scheme or any gateway's
	var id string
	var port int
	ok = false
	for _, cand := range s.httpProxies(r.Context()) {
		if id, port, ok = cand.parseHost(req.Host); ok {
			h = cand
			break
		}
	}
	if !ok {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "not a DevPod port address", nil)
		return
	}
	var list devpodv1alpha1.DevPodList
	var err error
	if list.Items, err = s.allDevPods(r); err != nil { // the id says which, in whichever cluster
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	var dp *devpodv1alpha1.DevPod
	for i := range list.Items {
		if list.Items[i].Annotations[httpauth.IDAnnotation] == id {
			dp = &list.Items[i]
		}
	}
	// one answer for "no such address" and "not yours": ids stay unguessable
	if dp == nil || !httpauth.MayOpen(dp, port, sess.User, sess.Admin) {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "this address does not exist or you have no access to it", nil)
		return
	}
	rd := req.RD
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") {
		rd = "/"
	}
	t := httpauth.Sign(h.Key, httpauth.Claims{Kind: httpauth.KindTicket, ID: id, User: sess.User, Admin: sess.Admin, Exp: time.Now().Add(httpauth.TicketTTL).Unix()})
	u := url.URL{Scheme: h.Scheme, Host: h.authority(bareHost(req.Host)), Path: httpauth.AuthPath, RawQuery: url.Values{"t": {t}, "rd": {rd}}.Encode()}
	s.writeJSON(w, http.StatusOK, map[string]string{"url": u.String()})
}
