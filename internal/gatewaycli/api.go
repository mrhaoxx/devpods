// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// KeyPrefix marks the gateway's shared key in the Authorization header,
// so the console can tell it from a user's API token.
const KeyPrefix = "gwk_"

// UserHeader names the user the gateway authenticated by SSH key.
const UserHeader = "X-DevPod-User"

// API calls the console's REST API on a user's behalf.
type API struct {
	BaseURL string // e.g. http://devpod-webui.devpod-system.svc:80
	Key     string // shared with the console
	HTTP    *http.Client
	MaxBody int64 // response size cap; default 8 MiB
}

// NewAPI returns an API with a 10 s request timeout.
func NewAPI(baseURL, key string) *API {
	return &API{BaseURL: strings.TrimRight(baseURL, "/"), Key: key, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// APIError is the console's {code, message, detail} error.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (e *APIError) Error() string {
	msg := e.Message
	if d, ok := e.Detail.(string); ok && d != "" {
		msg += ": " + d
	}
	if e.Code == "" {
		return msg
	}
	return fmt.Sprintf("%s (%s)", msg, e.Code)
}

// Do sends one request as user and returns the response body; a non-2xx
// answer comes back as *APIError.
func (a *API) Do(ctx context.Context, user, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+KeyPrefix+a.Key)
	req.Header.Set(UserHeader, user)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, &APIError{Code: "UNAVAILABLE", Message: "the DevPod API cannot be reached: " + err.Error()}
	}
	defer resp.Body.Close()
	max := a.MaxBody
	if max <= 0 {
		max = 8 << 20
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, &APIError{Status: resp.StatusCode, Code: "UNAVAILABLE", Message: "reading the API response: " + err.Error()}
	}
	if int64(len(b)) > max {
		return nil, &APIError{Status: resp.StatusCode, Code: "TOO_LARGE", Message: "the API response is too large for the CLI"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(b, e) != nil || e.Message == "" {
			e.Code, e.Message = "HTTP_"+fmt.Sprint(resp.StatusCode), strings.TrimSpace(string(b))
			if e.Message == "" {
				e.Message = resp.Status
			}
		}
		return nil, e
	}
	return b, nil
}
