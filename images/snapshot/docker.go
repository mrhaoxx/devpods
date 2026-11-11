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

// dockerAPIVersion is the Engine API version this code was written
// against (v1.41 = Docker 20.10). A daemon serves a range of versions
// and refuses requests outside it — Docker 29 no longer serves anything
// below v1.44 — so the version actually used is negotiated from
// GET /version (see pickAPIVersion). The five calls made here have the
// same shape in every version from v1.41 to the current one.
const dockerAPIVersion = "1.41"

// apiVersionLess compares "major.minor" API versions.
func apiVersionLess(a, b string) bool {
	parse := func(s string) (int, int) {
		maj, min, _ := strings.Cut(s, ".")
		x, _ := strconv.Atoi(maj)
		y, _ := strconv.Atoi(min)
		return x, y
	}
	am, an := parse(a)
	bm, bn := parse(b)
	return am < bm || (am == bm && an < bn)
}

// pickAPIVersion chooses the version to speak: the one this code was
// written against when the daemon serves it, else the nearest end of the
// daemon's range [min, max] (both from GET /version: MinAPIVersion and
// ApiVersion).
func pickAPIVersion(min, max string) string {
	switch {
	case min != "" && apiVersionLess(dockerAPIVersion, min):
		return min
	case max != "" && apiVersionLess(max, dockerAPIVersion):
		return max
	}
	return dockerAPIVersion
}

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
	base      string // http://docker/v<version>, set by negotiate
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
		hc:        &http.Client{Transport: tr},
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

// negotiate asks the daemon which API versions it serves (the
// unversioned GET /version) and fixes the prefix for every later call.
func (d *dockerBackend) negotiate(ctx context.Context) error {
	if d.base != "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return err
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the Docker daemon: %w", err)
	}
	defer resp.Body.Close()
	var v struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil || v.APIVersion == "" {
		return fmt.Errorf("the Docker daemon did not report its API version (HTTP %d)", resp.StatusCode)
	}
	d.base = "http://docker/v" + pickAPIVersion(v.MinAPIVersion, v.APIVersion)
	return nil
}

// do sends a request; a non-2xx answer becomes an error carrying the
// daemon's message ({"message": "…"}, the API's ErrorResponse).
func (d *dockerBackend) do(ctx context.Context, method, path string, q url.Values, hdr map[string]string) (*http.Response, error) {
	if err := d.negotiate(ctx); err != nil {
		return nil, err
	}
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
	// no container config in the body: the daemon takes an empty body
	// (and then needs no Content-Type)
	resp, err := d.do(ctx, http.MethodPost, "/commit", q, nil)
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

// pushMessage is one line of the push stream (moby's jsonmessage.JSONMessage).
// What the daemon sends, by image store:
//
//   - classic store: per layer "Preparing", "Waiting", "Pushing" with
//     progressDetail{current,total}, then "Pushed" / "Layer already
//     exists" / "Mounted from <repo>"; at the end the status
//     "<tag>: digest: sha256:… size: N" and an aux {Tag, Digest, Size}.
//   - containerd image store (the default from Docker 29): the same
//     layer statuses and the "<tag>: digest: …" status, but no aux with
//     the digest.
//
// A failure after the stream began is a line with errorDetail{message}
// (and the deprecated "error"); before that it is a plain HTTP error.
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
	if digest == "" {
		return "", errors.New("the Docker daemon ended the push without reporting a digest")
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
