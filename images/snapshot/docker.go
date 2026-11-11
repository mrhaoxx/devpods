// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// dockerAPIVersion pins the Engine API (v1.41 = Docker 20.10; newer
// daemons serve older versions).
const dockerAPIVersion = "v1.41"

// dockerBackend drives the Docker Engine API over the daemon's unix
// socket with plain net/http:
//
//	POST   /commit?container=&repo=&tag=&pause=    → {"Id": "sha256:…"}
//	GET    /images/{name}/json                     → {"Id", "Size", …}
//	POST   /images/{name}/push?tag=                (X-Registry-Auth) → a JSON progress stream
//	POST   /images/{name}/tag?repo=&tag=
//	DELETE /images/{name}
type dockerBackend struct {
	hc        *http.Client
	base      string // http://docker/<version>
	container string
	ref       string
	repo, tag string
	pause     bool
	configDir string

	prevID    string // what the target name pointed at before the commit ("" = nothing)
	committed bool
}

func newDockerBackend(o options) (*dockerBackend, error) {
	repo, tag, err := splitRef(o.image)
	if err != nil {
		return nil, err
	}
	socket := o.socket
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &dockerBackend{
		hc: &http.Client{Transport: tr}, base: "http://docker/" + dockerAPIVersion,
		container: o.container, ref: repo + ":" + tag, repo: repo, tag: tag, pause: o.pause, configDir: o.dockerConfig,
	}, nil
}

func (d *dockerBackend) Close() { d.hc.CloseIdleConnections() }

// imagePath escapes an image name for the URL path; the API takes names
// with their slashes.
func imagePath(name string) string {
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// do sends a request; a non-2xx answer becomes an error carrying the
// daemon's message.
func (d *dockerBackend) do(ctx context.Context, method, path string, q url.Values, hdr map[string]string) (*http.Response, error) {
	u := d.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return nil, &dockerError{status: resp.StatusCode, msg: m.Message}
	}
	return nil, &dockerError{status: resp.StatusCode, msg: strings.TrimSpace(string(body))}
}

type dockerError struct {
	status int
	msg    string
}

func (e *dockerError) Error() string {
	if e.msg == "" {
		return "docker: HTTP " + strconv.Itoa(e.status)
	}
	return e.msg
}

func isNotFound(err error) bool {
	var de *dockerError
	return errors.As(err, &de) && de.status == http.StatusNotFound
}

// inspect returns the image's ID and size; ok is false when there is no
// such image.
func (d *dockerBackend) inspect(ctx context.Context, name string) (id string, size int64, ok bool, err error) {
	resp, err := d.do(ctx, http.MethodGet, "/images/"+imagePath(name)+"/json", nil, nil)
	if isNotFound(err) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	defer resp.Body.Close()
	var img struct {
		ID   string `json:"Id"`
		Size int64  `json:"Size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&img); err != nil {
		return "", 0, false, err
	}
	return img.ID, img.Size, true, nil
}

func (d *dockerBackend) Commit(ctx context.Context) error {
	prev, _, existed, err := d.inspect(ctx, d.ref)
	if err != nil {
		return err
	}
	if existed {
		d.prevID = prev
	}
	q := url.Values{"container": {d.container}, "repo": {d.repo}, "tag": {d.tag}, "pause": {strconv.FormatBool(d.pause)}}
	resp, err := d.do(ctx, http.MethodPost, "/commit", q, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	resp.Body.Close()
	d.committed = true
	return nil
}

func (d *dockerBackend) Size(ctx context.Context) (int64, error) {
	_, size, ok, err := d.inspect(ctx, d.ref)
	if err != nil || !ok {
		return 0, err
	}
	return size, nil
}

// pushMessage is one line of the push stream.
type pushMessage struct {
	Status         string `json:"status"`
	ID             string `json:"id"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	Aux struct {
		Digest string `json:"Digest"`
	} `json:"aux"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

var statusDigest = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)

// readPushStream consumes the daemon's progress stream: per-layer
// progress goes into lp and is reported through progress; the digest
// comes from the aux message (or the final "…: digest: sha256:… size: N"
// status); an error line fails the push.
func readPushStream(r io.Reader, lp *layerProgress, progress func(pushed, total int64)) (string, error) {
	dec := json.NewDecoder(r)
	digest := ""
	for {
		var m pushMessage
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return digest, nil
			}
			return digest, err
		}
		switch {
		case m.Error != "" || m.ErrorDetail.Message != "":
			if m.ErrorDetail.Message != "" {
				return digest, errors.New(m.ErrorDetail.Message)
			}
			return digest, errors.New(m.Error)
		case m.Aux.Digest != "":
			digest = m.Aux.Digest
		case m.ID != "" && m.ProgressDetail.Total > 0:
			lp.Set(m.ID, m.ProgressDetail.Current, m.ProgressDetail.Total)
		case m.ID != "" && (m.Status == "Pushed" || m.Status == "Layer already exists" || strings.HasPrefix(m.Status, "Mounted from")):
			lp.Finish(m.ID)
		default:
			if sm := statusDigest.FindStringSubmatch(m.Status); sm != nil && digest == "" {
				digest = sm[1]
			}
			continue
		}
		if progress != nil {
			progress(lp.Sum())
		}
	}
}

func (d *dockerBackend) Push(ctx context.Context, progress func(pushed, total int64)) (string, error) {
	auth, err := loadRegistryAuth(d.configDir, d.ref)
	if err != nil {
		return "", err
	}
	resp, err := d.do(ctx, http.MethodPost, "/images/"+imagePath(d.repo)+"/push", url.Values{"tag": {d.tag}},
		map[string]string{"X-Registry-Auth": auth.header()})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	digest, err := readPushStream(resp.Body, newLayerProgress(), progress)
	if err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return digest, nil
}

// untag removes the target name from the node (the layers stay while
// anything else uses them).
func (d *dockerBackend) untag(ctx context.Context) error {
	resp, err := d.do(ctx, http.MethodDelete, "/images/"+imagePath(d.ref), nil, nil)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *dockerBackend) Rollback(ctx context.Context) error {
	if !d.committed {
		// the commit may have landed although we never saw its answer
		// (cancelled mid-request): only touch a name that changed
		id, _, ok, err := d.inspect(ctx, d.ref)
		if err != nil || !ok || id == d.prevID {
			return err
		}
	}
	if d.prevID == "" {
		return d.untag(ctx)
	}
	// the name existed before: point it back at what it was
	resp, err := d.do(ctx, http.MethodPost, "/images/"+imagePath(d.prevID)+"/tag", url.Values{"repo": {d.repo}, "tag": {d.tag}}, nil)
	if err != nil {
		return fmt.Errorf("restore %s: %w", d.ref, err)
	}
	resp.Body.Close()
	return nil
}

func (d *dockerBackend) Release(ctx context.Context) error {
	if d.prevID != "" {
		return nil // the name was there before us: it stays, on the new image
	}
	return d.untag(ctx)
}
