// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/clientutil"
	"github.com/containerd/nerdctl/v2/pkg/config"
	"github.com/containerd/nerdctl/v2/pkg/imgutil/commit"
	"github.com/containerd/nerdctl/v2/pkg/imgutil/dockerconfigresolver"
	"github.com/containerd/nerdctl/v2/pkg/referenceutil"
)

// containerdBackend talks to containerd directly, the way nerdctl does
// (nerdctl is a CLI over the same client library):
//
//   - commit: nerdctl's pkg/imgutil/commit.Commit, as `nerdctl commit` calls it
//     (pkg/cmd/container/commit.go);
//   - push: client.Push with a resolver built by nerdctl's
//     dockerconfigresolver ($DOCKER_CONFIG/config.json credentials, the
//     node's hosts.d) and a docker.StatusTracker read for progress, as
//     `nerdctl push` does (pkg/cmd/image/push.go, pkg/imgutil/push).
type containerdBackend struct {
	client *containerd.Client
	ctx    context.Context // carries the containerd namespace
	cancel context.CancelFunc
	gopts  types.GlobalCommandOptions
	e      *emitter

	containerID string
	ref         string // normalised target reference
	domain      string // its registry host
	plainHTTP   bool   // that registry is one of the named plain-HTTP ones
	pause       bool

	prev      *ocispec.Descriptor // what the target name pointed at before ("" = nothing)
	committed bool
	wasPaused bool // the container was already paused before we touched it
}

func newContainerdBackend(ctx context.Context, o options, e *emitter) (backend, error) {
	// the libraries log through containerd/log to stderr; keep the Job's
	// log to the protocol and real errors
	_ = log.SetLevel("error")
	parsed, err := referenceutil.Parse(o.image)
	if err != nil {
		return nil, fmt.Errorf("target image %q: %w", o.image, err)
	}
	if o.dockerConfig != "" {
		// nerdctl's credential store reads $DOCKER_CONFIG
		if err := os.Setenv("DOCKER_CONFIG", o.dockerConfig); err != nil {
			return nil, err
		}
	}
	client, nsCtx, cancel, err := clientutil.NewClient(ctx, o.namespace, o.socket)
	if err != nil {
		return nil, err
	}
	// nerdctl's defaults (data root, hosts.d directories, …) with our
	// socket and namespace
	cfg := config.New()
	cfg.Address, cfg.Namespace = o.socket, o.namespace
	return &containerdBackend{
		client: client, ctx: nsCtx, cancel: cancel, gopts: types.GlobalCommandOptions(*cfg), e: e,
		containerID: o.container, ref: parsed.String(), domain: parsed.Domain, plainHTTP: o.usesPlainHTTP(parsed.Domain), pause: o.pause,
	}, nil
}

func (c *containerdBackend) Close() {
	c.cancel()
	_ = c.client.Close()
}

// ns moves the containerd namespace onto a context of the caller's
// (cleanup runs on a fresh one after a cancel).
func (c *containerdBackend) ns(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, c.gopts.Namespace)
}

func (c *containerdBackend) taskStatus(ctx context.Context) (containerd.Task, containerd.ProcessStatus, error) {
	container, err := c.client.LoadContainer(ctx, c.containerID)
	if err != nil {
		return nil, "", err
	}
	task, err := container.Task(ctx, cio.Load)
	if err != nil {
		return nil, "", err
	}
	st, err := task.Status(ctx)
	if err != nil {
		return nil, "", err
	}
	return task, st.Status, nil
}

func (c *containerdBackend) Commit(ctx context.Context) error {
	ctx = c.ns(ctx)
	container, err := c.client.LoadContainer(ctx, c.containerID)
	if err != nil {
		return fmt.Errorf("container %s: %w", c.containerID, err)
	}
	if img, err := c.client.ImageService().Get(ctx, c.ref); err == nil {
		t := img.Target
		c.prev = &t
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	if _, st, err := c.taskStatus(ctx); err == nil {
		c.wasPaused = st == containerd.Paused
	}
	opts := &commit.Opts{
		Ref:         c.ref,
		Pause:       c.pause,
		Compression: types.Gzip,
		Format:      types.ImageFormatDocker,
		Timeout:     time.Hour,
	}
	if _, err := commit.Commit(ctx, c.client, container, opts, c.gopts); err != nil {
		// nerdctl resumes the container with the context it was given;
		// after a cancel that context is dead, so make sure here
		c.resume()
		return err
	}
	c.committed = true
	return nil
}

// resume un-pauses the container if this run left it paused (never one
// that was paused before we started).
func (c *containerdBackend) resume() {
	if !c.pause || c.wasPaused {
		return
	}
	ctx, cancel := context.WithTimeout(c.ns(context.Background()), 10*time.Second)
	defer cancel()
	task, st, err := c.taskStatus(ctx)
	if err != nil || st != containerd.Paused {
		return
	}
	if err := task.Resume(ctx); err != nil {
		c.e.Log(fmt.Sprintf("could not resume the container: %v", err))
	}
}

func (c *containerdBackend) Size(ctx context.Context) (int64, error) {
	ctx = c.ns(ctx)
	img, err := c.client.ImageService().Get(ctx, c.ref)
	if err != nil {
		return 0, err
	}
	// the size of the image's content (compressed layers, config,
	// manifest): what the registry will hold
	return containerd.NewImage(c.client, img).Size(ctx)
}

// pushJobs remembers the blobs client.Push handles, like nerdctl's
// pushjobs, with their sizes.
type pushJobs struct {
	mu    sync.Mutex
	sizes map[string]int64
}

func (j *pushJobs) add(ref string, size int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.sizes[ref] = size
}

func (j *pushJobs) snapshot() map[string]int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]int64, len(j.sizes))
	for k, v := range j.sizes {
		out[k] = v
	}
	return out
}

func (c *containerdBackend) Push(ctx context.Context, progress func(pushed, total int64), layer func(id string, size int64)) (string, error) {
	ctx = c.ns(ctx)
	img, err := c.client.ImageService().Get(ctx, c.ref)
	if err != nil {
		return "", fmt.Errorf("unable to resolve image to manifest: %w", err)
	}
	desc := img.Target

	// a fresh tracker per push, as nerdctl does (pushImageWithLocal)
	tracker := docker.NewInMemoryTracker()
	hostOpts := []dockerconfigresolver.Opt{dockerconfigresolver.WithHostsDirs(c.gopts.HostsDir)}
	if c.plainHTTP {
		hostOpts = append(hostOpts, dockerconfigresolver.WithPlainHTTP(true))
	}
	ho, err := dockerconfigresolver.NewHostOptions(ctx, c.domain, hostOpts...)
	if err != nil {
		return "", err
	}
	resolver := docker.NewResolver(docker.ResolverOptions{Tracker: tracker, Hosts: dockerconfig.ConfigureHosts(ctx, *ho)})

	jobs := &pushJobs{sizes: map[string]int64{}}
	handler := images.HandlerFunc(func(ctx context.Context, d ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if !images.IsNonDistributable(d.MediaType) {
			jobs.add(remotes.MakeRefKey(ctx, d), d.Size)
		}
		return nil, nil
	})
	status := func(ref string) (int64, int64, bool) {
		st, err := tracker.GetStatus(ref)
		if err != nil {
			return 0, 0, false
		}
		return st.Offset, st.Total, true
	}
	lp := newLayerProgress(layer)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				progress(report(jobs.snapshot(), status, lp))
			case <-done:
				return
			}
		}
	}()
	err = c.client.Push(ctx, c.ref, desc,
		containerd.WithResolver(resolver),
		containerd.WithImageHandler(remotes.SkipNonDistributableBlobs(handler)),
		containerd.WithPlatformMatcher(platforms.All),
	)
	close(done)
	wg.Wait()
	if err != nil {
		return "", err
	}
	// everything is up: count every blob in full
	for ref := range jobs.snapshot() {
		lp.Finish(ref)
	}
	progress(lp.Sum())
	return desc.Digest.String(), nil
}

func (c *containerdBackend) Rollback(ctx context.Context) error {
	ctx = c.ns(ctx)
	c.resume()
	is := c.client.ImageService()
	cur, err := is.Get(ctx, c.ref)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.prev == nil {
		// the name is ours (it was not there before): remove it
		return is.Delete(ctx, c.ref, images.SynchronousDelete())
	}
	if cur.Target.Digest == c.prev.Digest {
		return nil // the commit never replaced it
	}
	cur.Target = *c.prev
	_, err = is.Update(ctx, cur, "target")
	return err
}

func (c *containerdBackend) Release(ctx context.Context) error {
	if c.prev != nil {
		return nil // the name was there before us: it stays, on the new image
	}
	err := c.client.ImageService().Delete(c.ns(ctx), c.ref, images.SynchronousDelete())
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}
