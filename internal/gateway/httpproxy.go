// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
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
	dir        Directory
	baseDomain string
	suffix     string
	// Key signs tickets and cookies (shared with the webui); without it
	// only public ports are served.
	Key []byte
	// LoginURL is the webui endpoint a browser is sent to for a ticket.
	LoginURL string
	Now      func() time.Time
	// Net, when set, is how a DevPod's address is reached: through the
	// agent of its cluster (see Dialer.Net). Nil: a plain TCP connection.
	Net func(ctx context.Context, t Target) (net.Conn, error)
	// Audit records each request to a DevPod (nil: none is).
	Audit HTTPAudit

	mu         sync.Mutex
	transports map[string]*http.Transport // per DevPod: idle connections are reused
}

// idsTTL bounds how stale the id index may be (a reset id stops working
// within it).
const (
	idsTTL         = 5 * time.Second
	idsMissRefresh = time.Second
)

// NewHTTPProxy serves the DevPods of the namespace read through c.
func NewHTTPProxy(c client.Reader, devpodNamespace, baseDomain, suffix string) *HTTPProxy {
	p := NewHTTPProxyWith(nil, baseDomain, suffix)
	// the id index ages by the proxy's clock
	p.dir = &kubeDirectory{c: c, ns: devpodNamespace, now: func() time.Time { return p.Now() }}
	return p
}

// NewHTTPProxyWith serves the DevPods dir knows (a gateway of several
// clusters asks the console).
func NewHTTPProxyWith(dir Directory, baseDomain, suffix string) *HTTPProxy {
	return &HTTPProxy{dir: dir, baseDomain: baseDomain, suffix: suffix, Now: time.Now}
}

// lookup finds the DevPod with the id, and its cluster.
func (p *HTTPProxy) lookup(r *http.Request, id string) (*devpodv1alpha1.DevPod, string, bool) {
	f, err := p.dir.ByHTTPID(r.Context(), id)
	if err != nil || f == nil {
		return nil, "", false
	}
	return f.DevPod, f.Cluster, true
}

// transport is how the DevPod of that uid in cluster is reached; nil is
// Go's own. One per DevPod: a kept connection is never one that was made
// to another DevPod, whatever became of the address.
func (p *HTTPProxy) transport(cluster, uid string) http.RoundTripper {
	if p.Net == nil {
		return nil
	}
	key := cluster + "/" + uid
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.transports[key]; t != nil {
		return t
	}
	if len(p.transports) > 2048 { // DevPods long gone: start over
		for _, old := range p.transports {
			old.CloseIdleConnections()
		}
		p.transports = nil
	}
	if p.transports == nil {
		p.transports = map[string]*http.Transport{}
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return p.Net(ctx, Target{Cluster: cluster, Addr: addr, Purpose: PurposeHTTP, DevPodUID: uid})
		},
		MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second,
	}
	p.transports[key] = t
	return t
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
// It returns the user signed in ("" when the ticket is refused).
func (p *HTTPProxy) handleAuth(w http.ResponseWriter, r *http.Request, id string, port int, dp *devpodv1alpha1.DevPod) string {
	c, err := httpauth.Verify(p.Key, r.URL.Query().Get("t"), httpauth.KindTicket, p.Now())
	if err != nil || c.ID != id || c.User == "" || !httpauth.MayOpen(dp, port, c.User, c.Admin) {
		http.Error(w, "this link has expired; open the port again from the DevPod console", http.StatusForbidden)
		return ""
	}
	c.Kind, c.Exp = httpauth.KindCookie, p.Now().Add(httpauth.CookieTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: httpauth.CookieName, Value: httpauth.Sign(p.Key, c), Path: "/",
		MaxAge: int(httpauth.CookieTTL / time.Second), HttpOnly: true, Secure: p.secure(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeReturn(r.URL.Query().Get("rd")), http.StatusFound)
	return c.User
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
	dp, cluster, ok := p.lookup(r, id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// whom a good cookie names, on any port (a public one asks no one)
	user, authed := p.user(r, id, port, dp)
	if p.Audit != nil {
		serve, record := p.Audit.Admit(r.Context(), user)
		if !serve {
			http.Error(w, "audit storage is full; try again later", http.StatusServiceUnavailable)
			return
		}
		if record {
			var done func(string)
			w, done = p.record(w, r, dp, cluster, port)
			defer func() { done(user) }()
		}
	}
	if r.URL.Path == httpauth.AuthPath {
		user = p.handleAuth(w, r, id, port, dp)
		return
	}
	if httpauth.Access(dp, port) != httpauth.AccessPublic {
		if !authed {
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
	slog.Info("http_proxy", "devpod", dp.Name, "port", port, "user", user, "access", httpauth.Access(dp, port), "method", r.Method, "path", r.URL.Path)
	stripCookie(r)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = target
		},
		Transport:     p.transport(cluster, string(dp.UID)),
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
