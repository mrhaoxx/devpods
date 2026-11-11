// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func testSnap() *devpodv1alpha1.DevPodSnapshot {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-1"
	snap.Namespace = "devpods"
	snap.Spec.DevPodName = "my-devpod"
	snap.Spec.Image = "registry.example.com/repo:v1"
	snap.Spec.Push = true
	return snap
}

func envOf(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

func hostPaths(vols []corev1.Volume) map[string]string {
	m := map[string]string{}
	for _, v := range vols {
		if v.HostPath != nil {
			m[v.Name] = v.HostPath.Path
		}
	}
	return m
}

func TestSplitContainerID(t *testing.T) {
	for in, want := range map[string][2]string{
		"containerd://abc": {"containerd", "abc"},
		"docker://def":     {"docker", "def"},
	} {
		rt, id, err := render.SplitContainerID(in)
		if err != nil || rt != want[0] || id != want[1] {
			t.Errorf("%s: %q %q %v", in, rt, id, err)
		}
	}
	for _, in := range []string{"", "abc", "cri-o://abc", "containerd://"} {
		if _, _, err := render.SplitContainerID(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestSnapshotJob_Shape(t *testing.T) {
	job := render.SnapshotJob(testSnap(), render.SnapshotTarget{Runtime: render.RuntimeContainerd, ContainerID: "abc123", NodeName: "node-1", Image: "reg/devpod-snapshot:v1"})
	if job.Name != "snapshot-snap-1" || job.Namespace != "devpods" {
		t.Errorf("name %s/%s", job.Namespace, job.Name)
	}
	if *job.Spec.TTLSecondsAfterFinished != 300 || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil {
		t.Errorf("job spec %+v", job.Spec)
	}
	ps := job.Spec.Template.Spec
	if ps.NodeName != "node-1" || ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod spec node %q restart %q", ps.NodeName, ps.RestartPolicy)
	}
	// a cancel needs time to stop the CLI and remove the tag
	if ps.TerminationGracePeriodSeconds == nil || *ps.TerminationGracePeriodSeconds < 30 {
		t.Errorf("grace period %v", ps.TerminationGracePeriodSeconds)
	}
	for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		if labels[render.LabelSnapshot] != "snap-1" || labels[render.LabelDevPod] != "my-devpod" || labels[render.LabelManaged] != "devpod-controller" {
			t.Errorf("labels %v", labels)
		}
	}
	c := ps.Containers[0]
	if c.Name != render.SnapshotContainer || c.Image != "reg/devpod-snapshot:v1" {
		t.Errorf("container %s %s", c.Name, c.Image)
	}
	// the image's entrypoint (devpod-snapshot) runs, configured by env
	if len(c.Command) != 0 || len(c.Args) != 0 {
		t.Errorf("command %v args %v", c.Command, c.Args)
	}
	if c.TerminationMessagePath != "/dev/termination-log" || c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("termination message %s %s", c.TerminationMessagePath, c.TerminationMessagePolicy)
	}
}

func TestSnapshotJob_Containerd(t *testing.T) {
	job := render.SnapshotJob(testSnap(), render.SnapshotTarget{Runtime: render.RuntimeContainerd, ContainerID: "abc123", NodeName: "n1", Image: "img"})
	ps := job.Spec.Template.Spec
	env := envOf(ps.Containers[0])
	want := map[string]string{
		"RUNTIME": "containerd", "CONTAINER_ID": "abc123", "TARGET_IMAGE": "registry.example.com/repo:v1", "PUSH": "true", "PAUSE": "true",
		"RUNTIME_SOCKET": "/run/containerd/containerd.sock", "CONTAINERD_NAMESPACE": "k8s.io",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	if _, has := env["DOCKER_CONFIG"]; has {
		t.Error("DOCKER_CONFIG set without a push secret")
	}
	if hp := hostPaths(ps.Volumes); len(hp) != 1 || hp["runtime-sock"] != "/run/containerd/containerd.sock" {
		t.Errorf("host paths %v", hp)
	}
	if m := ps.Containers[0].VolumeMounts; len(m) != 1 || m[0].MountPath != "/run/containerd/containerd.sock" {
		t.Errorf("mounts %v", m)
	}
	if *ps.Volumes[0].HostPath.Type != corev1.HostPathSocket {
		t.Errorf("hostPath type %v", *ps.Volumes[0].HostPath.Type)
	}

	// a node with containerd's socket elsewhere
	job = render.SnapshotJob(testSnap(), render.SnapshotTarget{Runtime: render.RuntimeContainerd, ContainerID: "x", NodeName: "n1", Image: "img", ContainerdSocket: "/run/k3s/containerd/containerd.sock"})
	ps = job.Spec.Template.Spec
	if envOf(ps.Containers[0])["RUNTIME_SOCKET"] != "/run/k3s/containerd/containerd.sock" || hostPaths(ps.Volumes)["runtime-sock"] != "/run/k3s/containerd/containerd.sock" {
		t.Errorf("custom socket not used: %v", ps.Volumes)
	}
}

func TestSnapshotJob_Docker(t *testing.T) {
	job := render.SnapshotJob(testSnap(), render.SnapshotTarget{Runtime: render.RuntimeDocker, ContainerID: "def456", NodeName: "n1", Image: "docker:cli"})
	ps := job.Spec.Template.Spec
	env := envOf(ps.Containers[0])
	if env["RUNTIME"] != "docker" || env["CONTAINER_ID"] != "def456" || env["RUNTIME_SOCKET"] != "/var/run/docker.sock" {
		t.Errorf("env %v", env)
	}
	if _, has := env["CONTAINERD_NAMESPACE"]; has {
		t.Error("containerd env on a docker node")
	}
	if hp := hostPaths(ps.Volumes); len(hp) != 1 || hp["runtime-sock"] != "/var/run/docker.sock" {
		t.Errorf("host paths %v", hp)
	}
}

func TestSnapshotJob_PauseAndPush(t *testing.T) {
	snap := testSnap()
	snap.Spec.Pause = ptr.To(false)
	snap.Spec.Push = false
	secret := "my-push-secret"
	job := render.SnapshotJob(snap, render.SnapshotTarget{Runtime: render.RuntimeContainerd, ContainerID: "x", NodeName: "n1", Image: "img", PushSecret: &secret})
	ps := job.Spec.Template.Spec
	env := envOf(ps.Containers[0])
	if env["PAUSE"] != "false" || env["PUSH"] != "false" {
		t.Errorf("env %v", env)
	}
	// a local-only snapshot never mounts push credentials
	if len(ps.Volumes) != 1 {
		t.Errorf("volumes %v", ps.Volumes)
	}
}

func TestSnapshotJob_PushSecret(t *testing.T) {
	secret := "my-push-secret"
	job := render.SnapshotJob(testSnap(), render.SnapshotTarget{Runtime: render.RuntimeContainerd, ContainerID: "x", NodeName: "n1", Image: "img", PushSecret: &secret})
	ps := job.Spec.Template.Spec
	var vol *corev1.Volume
	for i := range ps.Volumes {
		if ps.Volumes[i].Name == "push-secret" {
			vol = &ps.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secret || len(vol.Secret.Items) != 1 ||
		vol.Secret.Items[0].Key != ".dockerconfigjson" || vol.Secret.Items[0].Path != "config.json" {
		t.Fatalf("push secret volume %+v", vol)
	}
	var mounted bool
	for _, m := range ps.Containers[0].VolumeMounts {
		if m.Name == "push-secret" && m.MountPath == "/root/.docker" && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted || envOf(ps.Containers[0])["DOCKER_CONFIG"] != "/root/.docker" {
		t.Errorf("push secret not wired: %v", ps.Containers[0].VolumeMounts)
	}
}

func TestParseSnapshotResult(t *testing.T) {
	cases := map[string]render.SnapshotResult{
		"digest=sha256:abc size=123\n":          {Digest: "sha256:abc", SizeBytes: 123},
		"size=42":                               {SizeBytes: 42},
		"digest= size=":                         {},
		"error=push failed\n":                   {Error: "push failed"},
		"error=cancelled":                       {Error: "cancelled"},
		"":                                      {},
		"pushing layer\n@error commit failed\n": {Error: "commit failed"},
		"some tool output\nfatal: no such container\n": {Error: "fatal: no such container"},
	}
	for in, want := range cases {
		if got := render.ParseSnapshotResult(in); got != want {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}
}
