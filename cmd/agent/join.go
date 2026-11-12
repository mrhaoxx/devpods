// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mrhaoxx/devpod/internal/agent"
)

// join registers the agent's public key at the console with the one-time
// token. A console that already has this very key answers "already
// joined", which is fine: the token file may still be there on a restart.
func join(ctx context.Context, console, cluster, tokenFile string, id *agent.Identity) error {
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"cluster": cluster, "token": strings.TrimSpace(string(token)), "publicKey": id.PublicKey()})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(console, "/")+"/agent/join", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("the console answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}
