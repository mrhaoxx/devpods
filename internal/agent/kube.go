// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"k8s.io/client-go/rest"
)

// kubeHandler forwards requests to the cluster's API server as the agent
// itself. Whatever identity the request claims is dropped first: here the
// console is the agent's service account, and can be nobody else.
func kubeHandler(cfg *rest.Config) (http.Handler, error) {
	target, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, err
	}
	if target.Scheme == "" {
		target, err = url.Parse("https://" + cfg.Host)
		if err != nil {
			return nil, err
		}
	}
	// the agent's own credentials and the API server's CA
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			r.Out.Header.Del("Authorization")
			for name := range r.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(name), "Impersonate-") {
					r.Out.Header.Del(name)
				}
			}
		},
		Transport:     transport,
		FlushInterval: -1, // watches and logs: every chunk at once
	}, nil
}
