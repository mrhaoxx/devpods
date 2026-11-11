// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// devpod-snapshot is the payload of a DevPod snapshot Job: it commits a
// running container to an image on its node and, unless told not to,
// pushes it — talking to the node's container runtime through its API
// (containerd: the containerd client plus nerdctl's commit package;
// Docker: the Engine API), not through a CLI. Progress goes to stdout in
// the snapshot protocol (see proto.go).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

// backend is one container runtime.
type backend interface {
	// Commit creates the target image from the container. It first notes
	// whether the target name already exists on the node, for Rollback.
	Commit(ctx context.Context) error
	// Size of the committed image in bytes (0 if unknown).
	Size(ctx context.Context) (int64, error)
	// Push uploads the image and returns its digest; progress receives
	// (pushedBytes, totalBytes) as often as the runtime reports.
	// layer hears once per blob that reached the registry (size 0: it
	// was there already).
	Push(ctx context.Context, progress func(pushed, total int64), layer func(id string, size int64)) (string, error)
	// Rollback undoes Commit after a failure or a cancel: an image name
	// this run created is removed; one that existed before is pointed
	// back at what it was. It never removes an image the run did not create.
	Rollback(ctx context.Context) error
	// Release runs after a successful push: the registry has the image,
	// so a name this run created is removed from the node (a name that
	// existed before keeps pointing at the new image).
	Release(ctx context.Context) error
	Close()
}

type options struct {
	runtime, socket, namespace string
	container, image           string
	pause, push                bool
	dockerConfig               string
	termPath                   string
	// plainHTTP are the registries (host or host:port, lower case) pushed
	// to without TLS — a registry inside the cluster's network. Named one
	// by one: a registry not listed is never spoken to in the clear.
	plainHTTP []string
}

// usesPlainHTTP reports whether registry host is one of the named ones.
func (o options) usesPlainHTTP(host string) bool {
	return host != "" && slices.Contains(o.plainHTTP, strings.ToLower(host))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch os.Getenv(key) {
	case "true", "1":
		return true
	case "false", "0":
		return false
	}
	return def
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("devpod-snapshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.runtime, "runtime", envOr("RUNTIME", ""), "container runtime: containerd or docker")
	fs.StringVar(&o.socket, "socket", envOr("RUNTIME_SOCKET", ""), "runtime socket path (default: the runtime's usual one)")
	fs.StringVar(&o.namespace, "namespace", envOr("CONTAINERD_NAMESPACE", "k8s.io"), "containerd namespace")
	fs.StringVar(&o.container, "container", envOr("CONTAINER_ID", ""), "ID of the container to commit")
	fs.StringVar(&o.image, "image", envOr("TARGET_IMAGE", ""), "image reference to create")
	fs.BoolVar(&o.pause, "pause", envBool("PAUSE", true), "pause the container while committing")
	fs.BoolVar(&o.push, "push", envBool("PUSH", true), "push the image after committing")
	fs.StringVar(&o.dockerConfig, "docker-config", envOr("DOCKER_CONFIG", ""), "directory holding config.json with the push credentials")
	plain := fs.String("plain-http-registries", envOr("PLAIN_HTTP_REGISTRIES", ""), "registries pushed to over plain HTTP, comma separated (host or host:port)")
	fs.StringVar(&o.termPath, "termination-log", envOr("TERMINATION_LOG", "/dev/termination-log"), "where the result line is written")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	for _, h := range strings.Split(*plain, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			o.plainHTTP = append(o.plainHTTP, h)
		}
	}
	if o.container == "" || o.image == "" {
		return o, errors.New("the container ID and the target image are required")
	}
	switch o.runtime {
	case "containerd":
		if o.socket == "" {
			o.socket = "/run/containerd/containerd.sock"
		}
	case "docker":
		if o.socket == "" {
			o.socket = "/var/run/docker.sock"
		}
	default:
		return o, fmt.Errorf("unknown runtime %q (containerd or docker)", o.runtime)
	}
	return o, nil
}

// wantsLogin reports whether a push failed because the registry refused
// whoever was pushing: no credentials, or wrong ones.
func wantsLogin(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, sign := range []string{"401", "unauthorized", "authorization", "authentication required", "denied", "no basic auth credentials"} {
		if strings.Contains(msg, sign) {
			return true
		}
	}
	return false
}

// rollbackTimeout bounds the cleanup after a failure or cancel; it has to
// fit in the Job pod's termination grace period.
const rollbackTimeout = 20 * time.Second

// run drives one snapshot; it returns the process exit code.
func run(ctx context.Context, o options, b backend, e *emitter) int {
	fail := func(what string, err error) int {
		msg, code := fmt.Sprintf("%s: %v", what, err), 1
		if ctx.Err() != nil { // SIGTERM: the snapshot was cancelled
			msg, code = "cancelled", 143
			e.Log("已取消，正在清理")
		}
		e.PushEnd()
		rctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		if rerr := b.Rollback(rctx); rerr != nil {
			e.Log(fmt.Sprintf("cleanup: %v", rerr))
		}
		e.Error(msg)
		e.Result("", 0, msg)
		return code
	}

	started := time.Now()
	e.Stage("commit")
	if o.pause {
		e.Logf("提交容器 %s（期间容器暂停）", shortDigest(o.container))
	} else {
		e.Logf("提交容器 %s（不暂停容器）", shortDigest(o.container))
	}
	if err := b.Commit(ctx); err != nil {
		return fail("commit failed", err)
	}
	if ctx.Err() != nil {
		return fail("commit", ctx.Err())
	}
	size, err := b.Size(ctx)
	if err != nil {
		e.Log(fmt.Sprintf("image size unknown: %v", err))
	} else if size > 0 {
		e.Size(size)
		e.Logf("已提交，镜像 %s", humanBytes(size))
	}
	if !o.push {
		e.Logf("完成（只提交，不推送），用时 %s", time.Since(started).Round(time.Second))
		e.Result("", size, "")
		e.Done()
		return 0
	}

	e.Stage("push")
	e.Logf("推送到 %s", o.image)
	digest, err := b.Push(ctx, e.Push, e.Layer)
	if err != nil {
		if wantsLogin(err) {
			return fail("push failed", fmt.Errorf("%w（仓库拒绝了这次推送：检查这个仓库的凭证 — 设置 → 镜像仓库凭证，或保存时填写用户名和密码）", err))
		}
		return fail("push failed", err)
	}
	e.PushEnd()
	if digest != "" {
		e.Digest(digest)
		e.Logf("推送完成，摘要 %s", digest)
	}
	rctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	if err := b.Release(rctx); err != nil {
		e.Log(fmt.Sprintf("cleanup: %v", err))
	}
	e.Logf("完成，用时 %s", time.Since(started).Round(time.Second))
	e.Result(digest, size, "")
	e.Done()
	return 0
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout))
}

func realMain(args []string, stdout io.Writer) int {
	o, err := parseFlags(args)
	e := newEmitter(stdout, o.termPath)
	if err != nil {
		e.Error(err.Error())
		e.Result("", 0, err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	b, err := newBackend(ctx, o, e)
	if err != nil {
		e.Error(err.Error())
		e.Result("", 0, err.Error())
		return 1
	}
	defer b.Close()
	return run(ctx, o, b, e)
}

func newBackend(ctx context.Context, o options, e *emitter) (backend, error) {
	if o.runtime == "docker" {
		return newDockerBackend(o)
	}
	return newContainerdBackend(ctx, o, e)
}
