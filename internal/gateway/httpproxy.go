// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/httpauth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HTTPProxy routes {id}-{port}{suffix}.{baseDomain} to a port of the
// DevPod whose random id (httpauth.IDAnnotation) is {id}:
//
//	k3x7qmpw2abc-8888-devpod.clusters.zjusct.io → that DevPod, port 8888
//
// The hostname names neither the DevPod nor its owner. A port is private
// (owner, collaborators, admins) unless the owner opened it to every
// signed-in user or to anyone with the link. Anything short of public
// needs the gateway's cookie for that hostname, which the webui hands
// out as a one-minute ticket (see AuthPath). Every request re-checks
// that the cookie's user may still reach the port.
type HTTPProxy struct {
	c          client.Reader
	dpNS       string
	baseDomain string
	suffix     string
	// Key signs tickets and cookies (shared with the webui); without it
	// only public ports are served.
	Key []byte
	// LoginURL is the webui endpoint a browser is sent to for a ticket.
	LoginURL string
	Now      func() time.Time

	mu    sync.Mutex
	ids   map[string]string // id → DevPod name
	idsAt time.Time
}

// idsTTL bounds how stale the id index may be (a reset id stops working
// within it).
const (
	idsTTL         = 5 * time.Second
	idsMissRefresh = time.Second
)

func NewHTTPProxy(c client.Reader, devpodNamespace, baseDomain, suffix string) *HTTPProxy {
	return &HTTPProxy{c: c, dpNS: devpodNamespace, baseDomain: baseDomain, suffix: suffix, Now: time.Now}
}

// lookup finds the DevPod with the id.
func (p *HTTPProxy) lookup(r *http.Request, id string) (*devpodv1alpha1.DevPod, bool) {
	if !httpauth.ValidID(id) {
		return nil, false
	}
	p.mu.Lock()
	name, ok := p.ids[id]
	// an id drawn a moment ago is not in the index yet: a miss rebuilds
	// it (at most once per idsMissRefresh, so unknown ids stay cheap)
	if age := p.Now().Sub(p.idsAt); p.ids == nil || age > idsTTL || (!ok && age > idsMissRefresh) {
		var list devpodv1alpha1.DevPodList
		if err := p.c.List(r.Context(), &list, client.InNamespace(p.dpNS)); err == nil {
			ids := make(map[string]string, len(list.Items))
			for i := range list.Items {
				if v := list.Items[i].Annotations[httpauth.IDAnnotation]; v != "" {
					ids[v] = list.Items[i].Name
				}
			}
			p.ids, p.idsAt = ids, p.Now()
		}
		name, ok = p.ids[id]
	}
	p.mu.Unlock()
	if !ok {
		return nil, false
	}
	var dp devpodv1alpha1.DevPod
	if err := p.c.Get(r.Context(), client.ObjectKey{Name: name, Namespace: p.dpNS}, &dp); err != nil || dp.Annotations[httpauth.IDAnnotation] != id {
		return nil, false
	}
	return &dp, true
}

// parseHost splits a proxy hostname into the id and the port.
func (p *HTTPProxy) parseHost(host string) (string, int, error) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	sub, ok := strings.CutSuffix(host, "."+p.baseDomain)
	if ok && p.suffix != "" {
		sub, ok = strings.CutSuffix(sub, p.suffix)
	}
	if !ok {
		return "", 0, fmt.Errorf("invalid host")
	}
	return parseSubdomain(sub)
}

func (p *HTTPProxy) secure() bool { return strings.HasPrefix(p.LoginURL, "https://") }

// user returns who the request's cookie says it is, if that user may
// (still) reach this port of dp.
func (p *HTTPProxy) user(r *http.Request, id string, port int, dp *devpodv1alpha1.DevPod) (string, bool) {
	ck, err := r.Cookie(httpauth.CookieName)
	if err != nil {
		return "", false
	}
	c, err := httpauth.Verify(p.Key, ck.Value, httpauth.KindCookie, p.Now())
	if err != nil || c.ID != id || c.User == "" || !httpauth.MayOpen(dp, port, c.User, c.Admin) {
		return "", false
	}
	return c.User, true
}

// safeReturn keeps a redirect target on this host.
func safeReturn(rd string) string {
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.HasPrefix(rd, "/\\") {
		return "/"
	}
	return rd
}

// handleAuth turns the webui's ticket into this hostname's cookie.
func (p *HTTPProxy) handleAuth(w http.ResponseWriter, r *http.Request, id string, port int, dp *devpodv1alpha1.DevPod) {
	c, err := httpauth.Verify(p.Key, r.URL.Query().Get("t"), httpauth.KindTicket, p.Now())
	if err != nil || c.ID != id || c.User == "" || !httpauth.MayOpen(dp, port, c.User, c.Admin) {
		http.Error(w, "this link has expired; open the port again from the DevPod console", http.StatusForbidden)
		return
	}
	c.Kind, c.Exp = httpauth.KindCookie, p.Now().Add(httpauth.CookieTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: httpauth.CookieName, Value: httpauth.Sign(p.Key, c), Path: "/",
		MaxAge: int(httpauth.CookieTTL / time.Second), HttpOnly: true, Secure: p.secure(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeReturn(r.URL.Query().Get("rd")), http.StatusFound)
}

// stripCookie removes the gateway's cookie before the request reaches
// the user's program.
func stripCookie(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != httpauth.CookieName {
			r.AddCookie(c)
		}
	}
}

func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, port, err := p.parseHost(r.Host)
	if err != nil {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	dp, ok := p.lookup(r, id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.Path == httpauth.AuthPath {
		p.handleAuth(w, r, id, port, dp)
		return
	}
	user, authed := "", false
	if httpauth.Access(dp, port) != httpauth.AccessPublic {
		if user, authed = p.user(r, id, port, dp); !authed {
			browser := (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(r.Header.Get("Accept"), "text/html")
			if !browser || p.LoginURL == "" || len(p.Key) == 0 {
				http.Error(w, "this port needs a sign-in: open it from the DevPod console", http.StatusUnauthorized)
				return
			}
			q := url.Values{"host": {r.Host}, "rd": {r.URL.RequestURI()}}
			http.Redirect(w, r, p.LoginURL+"?"+q.Encode(), http.StatusFound)
			return
		}
	}
	if dp.Status.Phase != devpodv1alpha1.DevPodRunning || dp.Status.Endpoint == "" {
		http.Error(w, "the DevPod is not running", http.StatusServiceUnavailable)
		return
	}
	podIP, sshPort, err := net.SplitHostPort(dp.Status.Endpoint)
	if err != nil {
		http.Error(w, "invalid endpoint", http.StatusInternalServerError)
		return
	}
	if sshPort == strconv.Itoa(port) {
		http.Error(w, "this port is not an HTTP service", http.StatusForbidden)
		return
	}
	target := net.JoinHostPort(podIP, strconv.Itoa(port))
	slog.Info("http_proxy", "devpod", dp.Name, "port", port, "user", user, "public", !authed, "method", r.Method, "path", r.URL.Path)
	stripCookie(r)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = target
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, fmt.Sprintf("nothing answers on port %d of this DevPod (%v)", port, err), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// parseSubdomain extracts the id and port from "{id}-{port}".
// The port is the last hyphen-separated segment that is numeric.
func parseSubdomain(s string) (string, int, error) {
	idx := strings.LastIndex(s, "-")
	if idx < 1 {
		return "", 0, fmt.Errorf("invalid format: expected {id}-{port}, got %q", s)
	}
	port, err := strconv.Atoi(s[idx+1:])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port in %q", s)
	}
	return s[:idx], port, nil
}
