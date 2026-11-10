// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
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
	m := h.host.FindStringSubmatch(strings.ToLower(host))
	if m == nil || !httpauth.ValidID(m[1]) {
		return "", 0, false
	}
	port, err := strconv.Atoi(m[2])
	return m[1], port, err == nil && port >= 1 && port <= 65535
}

// httpView is what the ports panel shows.
type httpView struct {
	ID string `json:"id"`
	// URLTemplate has the id filled in and {port} left for the caller.
	URLTemplate string `json:"urlTemplate"`
	PublicPorts []int  `json:"publicPorts"`
}

func (h *HTTPProxy) view(dp *devpodv1alpha1.DevPod) httpView {
	id := dp.Annotations[httpauth.IDAnnotation]
	ports := httpauth.PublicPorts(dp)
	if ports == nil {
		ports = []int{}
	}
	return httpView{ID: id, URLTemplate: h.Scheme + "://" + strings.Replace(h.HostTemplate, "{id}", id, 1), PublicPorts: ports}
}

func (s *Server) httpProxy(w http.ResponseWriter) (*HTTPProxy, bool) {
	if s.HTTPProxy == nil {
		s.writeErr(w, http.StatusNotFound, "HTTP_PROXY_DISABLED", "the HTTP proxy is not enabled", nil)
		return nil, false
	}
	return s.HTTPProxy, true
}

// annotate rewrites the DevPod's annotations with fn, retrying conflicts.
func (s *Server) annotate(r *http.Request, dp *devpodv1alpha1.DevPod, fn func(a map[string]string)) (*devpodv1alpha1.DevPod, error) {
	for attempt := 0; ; attempt++ {
		cp := dp.DeepCopy()
		if cp.Annotations == nil {
			cp.Annotations = map[string]string{}
		}
		fn(cp.Annotations)
		err := s.Client.Update(r.Context(), cp)
		if err == nil {
			return cp, nil
		}
		if !apierrors.IsConflict(err) || attempt >= 4 {
			return nil, err
		}
		fresh := &devpodv1alpha1.DevPod{}
		if err := s.Reader.Get(r.Context(), client.ObjectKeyFromObject(dp), fresh); err != nil {
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
	s.writeJSON(w, http.StatusOK, h.view(dp))
}

// handlePutHTTP serves PUT /api/devpods/{name}/http: {"publicPorts":[…]}
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
		Reset       bool   `json:"reset"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	if req.PublicPorts != nil {
		if len(*req.PublicPorts) > 32 {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "at most 32 public ports", nil)
			return
		}
		for _, p := range *req.PublicPorts {
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
		if req.PublicPorts != nil {
			if v := httpauth.FormatPorts(*req.PublicPorts); v != "" {
				a[httpauth.PublicPortsAnnotation] = v
			} else {
				delete(a, httpauth.PublicPortsAnnotation)
			}
		}
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, h.view(dp))
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
	id, _, ok := h.parseHost(req.Host)
	if !ok {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "not a DevPod port address", nil)
		return
	}
	var list devpodv1alpha1.DevPodList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(s.NS)); err != nil {
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
	if dp == nil || !httpauth.Allowed(dp, sess.User, sess.Admin) {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "this address does not exist or you have no access to it", nil)
		return
	}
	rd := req.RD
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") {
		rd = "/"
	}
	t := httpauth.Sign(h.Key, httpauth.Claims{Kind: httpauth.KindTicket, ID: id, User: sess.User, Admin: sess.Admin, Exp: time.Now().Add(httpauth.TicketTTL).Unix()})
	u := url.URL{Scheme: h.Scheme, Host: strings.ToLower(req.Host), Path: httpauth.AuthPath, RawQuery: url.Values{"t": {t}, "rd": {rd}}.Encode()}
	s.writeJSON(w, http.StatusOK, map[string]string{"url": u.String()})
}
