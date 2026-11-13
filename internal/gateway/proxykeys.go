// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import "sync"

// ProxyKeys are the keys of trusted proxies, SHA256 fingerprint → alias:
// those the GatewayConfig lists, and those the console says it logs in
// with (a gateway of several clusters). Both are trusted the same way.
type ProxyKeys struct {
	mu      sync.RWMutex
	config  map[string]string
	console map[string]string
}

// Alias is the alias of the proxy key with fingerprint fp, if it is one.
func (p *ProxyKeys) Alias(fp string) (string, bool) {
	if p == nil {
		return "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if a, ok := p.config[fp]; ok {
		return a, true
	}
	a, ok := p.console[fp]
	return a, ok
}

// SetConsole replaces the keys the console logs in with.
func (p *ProxyKeys) SetConsole(keys map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.console = keys
}
