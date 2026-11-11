// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func dockerSnapshotJob(t *testing.T, snap *devpodv1alpha1.DevPodSnapshot, id, node string, secret *string, image string, push bool) *batchv1.Job {
	t.Helper()
	snap = snap.DeepCopy()
	snap.Spec.Push = push
	if secret != nil {
		snap.Spec.PushSecretRef = &devpodv1alpha1.LocalObjectRef{Name: *secret}
	}
	job, err := render.SnapshotJob(snap, "docker", id, node, render.SnapshotOptions{DockerImage: image})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestSnapshotJob_BasicFields(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-1"
	snap.Namespace = "devpods"
	snap.Spec.DevPodName = "my-devpod"
	snap.Spec.Image = "registry.example.com/repo:v1"

	job := dockerSnapshotJob(t, snap, "abc123def", "node-1", nil, "docker:cli", true)

	if job.Name != "snapshot-snap-1" {
		t.Errorf("name = %q, want snapshot-snap-1", job.Name)
	}
	if job.Namespace != "devpods" {
		t.Errorf("namespace = %q, want devpods", job.Namespace)
	}
	if *job.Spec.TTLSecondsAfterFinished != int32(300) {
		t.Errorf("ttl = %d, want 300", *job.Spec.TTLSecondsAfterFinished)
	}
	if *job.Spec.BackoffLimit != int32(0) {
		t.Errorf("backoffLimit = %d, want 0", *job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.NodeName != "node-1" {
		t.Errorf("nodeName = %q, want node-1", job.Spec.Template.Spec.NodeName)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestSnapshotJob_ContainerEnv(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-2"
	snap.Namespace = "devpods"
	snap.Spec.DevPodName = "dp"
	snap.Spec.Image = "reg/img:tag"

	job := dockerSnapshotJob(t, snap, "containerid123", "n1", nil, "docker:cli", true)
	c := job.Spec.Template.Spec.Containers[0]

	envMap := map[string]string{}
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["CONTAINER_ID"] != "containerid123" {
		t.Errorf("CONTAINER_ID = %q, want containerid123", envMap["CONTAINER_ID"])
	}
	if envMap["TARGET_IMAGE"] != "reg/img:tag" {
		t.Errorf("TARGET_IMAGE = %q, want reg/img:tag", envMap["TARGET_IMAGE"])
	}
}

func TestSnapshotJob_DockerSocket(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-3"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"

	job := dockerSnapshotJob(t, snap, "cid", "n1", nil, "docker:cli", true)

	found := false
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "docker-sock" && v.HostPath != nil && v.HostPath.Path == "/var/run/docker.sock" {
			found = true
		}
	}
	if !found {
		t.Error("docker-sock volume not found or misconfigured")
	}

	c := job.Spec.Template.Spec.Containers[0]
	mountFound := false
	for _, m := range c.VolumeMounts {
		if m.Name == "docker-sock" && m.MountPath == "/var/run/docker.sock" {
			mountFound = true
		}
	}
	if !mountFound {
		t.Error("docker-sock volumeMount not found")
	}
}

func TestSnapshotJob_WithPushSecret(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-4"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"
	snap.Spec.PushSecretRef = &devpodv1alpha1.LocalObjectRef{Name: "my-creds"}

	secretName := "my-creds"
	job := dockerSnapshotJob(t, snap, "cid", "n1", &secretName, "docker:cli", true)

	var secretVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "push-secret" {
			secretVol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if secretVol == nil {
		t.Fatal("push-secret volume not found")
	}
	if secretVol.Secret.SecretName != "my-creds" {
		t.Errorf("secret name = %q, want my-creds", secretVol.Secret.SecretName)
	}

	c := job.Spec.Template.Spec.Containers[0]
	envMap := map[string]string{}
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["DOCKER_CONFIG"] != "/root/.docker" {
		t.Errorf("DOCKER_CONFIG = %q, want /root/.docker", envMap["DOCKER_CONFIG"])
	}
}

func TestSnapshotJob_WithoutPushSecret(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-5"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"

	job := dockerSnapshotJob(t, snap, "cid", "n1", nil, "docker:cli", true)

	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "push-secret" {
			t.Error("push-secret volume should not exist when no secret ref")
		}
	}

	c := job.Spec.Template.Spec.Containers[0]
	for _, e := range c.Env {
		if e.Name == "DOCKER_CONFIG" {
			t.Error("DOCKER_CONFIG env should not exist when no secret ref")
		}
	}
}

func TestSnapshotJob_Labels(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-6"
	snap.Namespace = "devpods"
	snap.Spec.DevPodName = "my-dp"
	snap.Spec.Image = "reg:tag"

	job := dockerSnapshotJob(t, snap, "cid", "n1", nil, "docker:cli", true)

	if job.Labels["devpod.io/snapshot"] != "snap-6" {
		t.Errorf("label devpod.io/snapshot = %q, want snap-6", job.Labels["devpod.io/snapshot"])
	}
	if job.Labels["devpod.io/devpod"] != "my-dp" {
		t.Errorf("label devpod.io/devpod = %q, want my-dp", job.Labels["devpod.io/devpod"])
	}
}

func TestSnapshotJob_TerminationMessage(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-7"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"

	job := dockerSnapshotJob(t, snap, "cid", "n1", nil, "docker:cli", true)
	c := job.Spec.Template.Spec.Containers[0]

	if c.TerminationMessagePath != "/dev/termination-log" {
		t.Errorf("terminationMessagePath = %q", c.TerminationMessagePath)
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("terminationMessagePolicy = %q, want FallbackToLogsOnError", c.TerminationMessagePolicy)
	}
}

func TestSnapshotJob_ReturnsJob(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "s"
	snap.Namespace = "devpods"
	snap.Spec.Image = "r:t"

	var _ *batchv1.Job = dockerSnapshotJob(t, snap, "c", "n", nil, "docker:cli", true)
}

func containerdSnapshotOptions() render.SnapshotOptions {
	return render.SnapshotOptions{
		DockerImage:                "docker:cli",
		ContainerdImage:            "registry.example.com/snapshot:nerdctl-2.2.1",
		ContainerdAddress:          "/run/k3s/containerd/containerd.sock",
		ContainerdNamespace:        "k8s.io",
		ContainerdInsecureRegistry: true,
	}
}

func TestSnapshotJob_Containerd(t *testing.T) {
	for _, push := range []bool{false, true} {
		for _, auth := range []bool{false, true} {
			snap := &devpodv1alpha1.DevPodSnapshot{}
			snap.Name, snap.Namespace = "snap", "devpods"
			snap.Spec.Image, snap.Spec.Push = "registry.example.com/dev:tag", push
			if auth {
				snap.Spec.PushSecretRef = &devpodv1alpha1.LocalObjectRef{Name: "creds"}
			}
			opts := containerdSnapshotOptions()
			job, err := render.SnapshotJob(snap, "containerd", "full-id", "arm-worker", opts)
			if err != nil {
				t.Fatal(err)
			}
			pod := job.Spec.Template
			c := pod.Spec.Containers[0]
			if pod.Spec.NodeName != "arm-worker" || c.Image != opts.ContainerdImage {
				t.Fatalf("wrong node/image: %s/%s", pod.Spec.NodeName, c.Image)
			}
			if pod.Labels[render.LabelSnapshotRuntime] != "containerd" || pod.Labels[render.LabelManaged] != "devpod-controller" {
				t.Fatalf("snapshot Pod missing network-policy labels: %v", pod.Labels)
			}
			if !reflect.DeepEqual(pod.Labels, job.Labels) {
				t.Fatal("Job and Pod labels differ")
			}
			volumes := map[string]corev1.Volume{}
			for _, v := range pod.Spec.Volumes {
				volumes[v.Name] = v
				if v.HostPath != nil && (v.HostPath.Path != opts.ContainerdAddress || *v.HostPath.Type != corev1.HostPathSocket) {
					t.Fatalf("unexpected host mount: %+v", v.HostPath)
				}
			}
			if volumes["containerd-sock"].HostPath == nil || volumes["work"].EmptyDir == nil {
				t.Fatal("missing containerd socket or work emptyDir")
			}
			mounts := map[string]string{}
			for _, m := range c.VolumeMounts {
				mounts[m.Name] = m.MountPath
			}
			if mounts["containerd-sock"] != opts.ContainerdAddress || mounts["work"] != "/work" {
				t.Fatalf("wrong mounts: %v", mounts)
			}
			secret := volumes["push-secret"].Secret
			if auth != (secret != nil) || (auth && secret.SecretName != "creds") {
				t.Fatalf("push=%v auth=%v: wrong credentials: %v", push, auth, secret)
			}
			if strings.Contains(c.Args[0], "nerdctl push") != push {
				t.Fatalf("push=%v: wrong script: %s", push, c.Args[0])
			}
		}
	}
}

func TestSnapshotJob_RejectsUnsupportedRuntime(t *testing.T) {
	if _, err := render.SnapshotJob(&devpodv1alpha1.DevPodSnapshot{}, "cri-o", "id", "node", containerdSnapshotOptions()); err == nil {
		t.Fatal("unsupported runtime accepted")
	}
	if _, err := render.SnapshotJob(&devpodv1alpha1.DevPodSnapshot{}, "containerd", "id", "node", render.SnapshotOptions{}); err == nil {
		t.Fatal("missing containerd configuration accepted")
	}
}

// Execute the rendered shell with stub CLIs: check command order, quoting,
// exit status, and the actual termination message without a container daemon.
func TestSnapshotJob_Scripts(t *testing.T) {
	for _, runtimeName := range []string{"docker", "containerd"} {
		for _, scenario := range []struct {
			name string
			push bool
			fail string
		}{
			{name: "commit-only"},
			{name: "commit-push", push: true},
			{name: "commit-failure", push: true, fail: "commit"},
			{name: "commit-only-failure", fail: "commit"},
			{name: "push-failure", push: true, fail: "push"},
			{name: "inspect-failure", push: true, fail: "image"},
		} {
			if runtimeName == "docker" && scenario.fail == "image" {
				continue
			}
			t.Run(runtimeName+"/"+scenario.name, func(t *testing.T) {
				dir := t.TempDir()
				stub := `#!/bin/sh
set -eu
printf '%s\n' "$0" "$@" >> "$CALLS"
if [ "${0##*/}" = nerdctl ]; then
  test "$1" = --address; test "$2" = /run/k3s/containerd/containerd.sock
  test "$3" = --namespace; test "$4" = custom-k8s
  test "$5" = --data-root; test "$6" = /work/nerdctl
  test "$7" = --insecure-registry=false
  shift 7
fi
if [ "$1" = "$FAIL" ]; then echo "test $1 failure" >&2; exit 17; fi
case "$1" in
  commit)
    shift
    if [ "${0##*/}" = nerdctl ]; then test "$1" = --format; test "$2" = oci; shift 2; fi
    test "$#" = 2; test "$1" = "$CONTAINER_ID"; test "$2" = "$TARGET_IMAGE"
    echo sha256:config
    ;;
  push)
    test "$#" = 2; test "$2" = "$TARGET_IMAGE"
    if [ "${0##*/}" = docker ]; then echo 'tag: digest: sha256:manifest size: 123'; fi
    ;;
  image)
    test "$#" = 7; test "$2" = inspect; test "$3" = --mode; test "$4" = native
    test "$5" = --format; test "$6" = '{{.Image.Target.Digest}}'; test "$7" = "$TARGET_IMAGE"
    echo sha256:manifest
    ;;
  *) exit 19 ;;
esac
`
				for _, cli := range []string{"docker", "nerdctl"} {
					if err := os.WriteFile(filepath.Join(dir, cli), []byte(stub), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				snap := &devpodv1alpha1.DevPodSnapshot{}
				// Values must remain single arguments and must never be evaluated as shell code.
				snap.Spec.Image, snap.Spec.Push = "registry/repo:tag; $(exit 99)", scenario.push
				opts := containerdSnapshotOptions()
				opts.ContainerdNamespace, opts.ContainerdInsecureRegistry = "custom-k8s", false
				job, err := render.SnapshotJob(snap, runtimeName, "id with spaces", "n", opts)
				if err != nil {
					t.Fatal(err)
				}
				c := job.Spec.Template.Spec.Containers[0]
				result := filepath.Join(dir, "result")
				script := strings.ReplaceAll(c.Args[0], "/dev/termination-log", result)
				script = strings.ReplaceAll(script, "/tmp/push.log", filepath.Join(dir, "push.log"))
				shell := "sh"
				if runtimeName == "docker" {
					// Like docker:cli's Alpine shell, bash supports pipefail.
					shell = "bash"
				}
				cmd := exec.Command(shell, "-c", script)
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+filepath.Join(dir, "calls"), "FAIL="+scenario.fail)
				for _, e := range c.Env {
					cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
				}
				out, err := cmd.CombinedOutput()
				if scenario.fail != "" {
					if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
						t.Fatalf("want CLI exit 17, got %v: %s", err, out)
					}
					calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
					if scenario.fail == "commit" && strings.Contains(string(calls), "\npush\n") {
						t.Fatal("push ran after commit failed")
					}
					if scenario.fail == "push" && strings.Contains(string(calls), "\nimage\n") {
						t.Fatal("inspect ran after push failed")
					}
					return
				}
				if err != nil {
					t.Fatalf("script: %v\n%s", err, out)
				}
				got, err := os.ReadFile(result)
				if err != nil {
					t.Fatal(err)
				}
				want := "sha256:config"
				if scenario.push {
					want = "sha256:manifest"
				}
				if strings.TrimSpace(string(got)) != want {
					t.Fatalf("result = %q, want %s", got, want)
				}
			})
		}
	}
}
