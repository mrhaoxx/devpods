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
	"strconv"
	"strings"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HTTPProxy is a reverse proxy that routes
// {devpod-name}-{port}{suffix}.{baseDomain} to the DevPod's pod IP.
//
// Example with suffix="-dev", baseDomain="ktaas.approaching-ai.com":
//
//	st43-dev-8080-dev.ktaas.approaching-ai.com → DevPod "st43-dev", port 8080
type HTTPProxy struct {
	c          client.Reader
	dpNS       string
	baseDomain string
	suffix     string
}

func NewHTTPProxy(c client.Reader, devpodNamespace, baseDomain, suffix string) *HTTPProxy {
	return &HTTPProxy{c: c, dpNS: devpodNamespace, baseDomain: baseDomain, suffix: suffix}
}

func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	domainSuffix := "." + p.baseDomain
	if !strings.HasSuffix(host, domainSuffix) {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	subdomain := strings.TrimSuffix(host, domainSuffix)
	if p.suffix != "" {
		if !strings.HasSuffix(subdomain, p.suffix) {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}
		subdomain = strings.TrimSuffix(subdomain, p.suffix)
	}

	name, port, err := parseSubdomain(subdomain)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var dp devpodv1alpha1.DevPod
	if err := p.c.Get(r.Context(), types.NamespacedName{Name: name, Namespace: p.dpNS}, &dp); err != nil {
		http.Error(w, "devpod not found", http.StatusNotFound)
		return
	}

	if dp.Status.Phase != devpodv1alpha1.DevPodRunning || dp.Status.Endpoint == "" {
		http.Error(w, "devpod not running", http.StatusServiceUnavailable)
		return
	}

	podIP, _, err := net.SplitHostPort(dp.Status.Endpoint)
	if err != nil {
		http.Error(w, "invalid endpoint", http.StatusInternalServerError)
		return
	}

	target := net.JoinHostPort(podIP, strconv.Itoa(port))
	slog.Info("http_proxy", "devpod", name, "port", port, "target", target, "path", r.URL.Path)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = target
		},
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

// parseSubdomain extracts devpod name and port from "{name}-{port}".
// The port is the last hyphen-separated segment that is numeric.
func parseSubdomain(s string) (string, int, error) {
	idx := strings.LastIndex(s, "-")
	if idx < 1 {
		return "", 0, fmt.Errorf("invalid format: expected {name}-{port}, got %q", s)
	}
	port, err := strconv.Atoi(s[idx+1:])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port in %q", s)
	}
	return s[:idx], port, nil
}
