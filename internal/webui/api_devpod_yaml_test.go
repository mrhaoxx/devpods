// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestDevPodYAML(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "yy-node"}}
	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	user := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "yy-alice"}}
	if err := k8sClient.Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node); _ = k8sClient.Delete(ctx, user); cleanupDevPods(t) })
	alice, bob, root := forge(sm, "yy-alice", false), forge(sm, "yy-bob", false), forge(sm, "yy-root", true)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		return apiCall(t, srv, method, path, auth, body, want)
	}
	call("POST", "/api/devpods", alice, createBody, http.StatusCreated)
	waitDevPod(t, "yy-alice-dev1")
	const path = "/api/devpods/yy-alice-dev1/yaml"
	live := func() devpodv1alpha1.DevPod {
		t.Helper()
		var dp devpodv1alpha1.DevPod
		if err := k8sManager.GetAPIReader().Get(ctx, types.NamespacedName{Name: "yy-alice-dev1", Namespace: "devpods"}, &dp); err != nil {
			t.Fatal(err)
		}
		return dp
	}
	// something the cluster keeps beside the spec, to see it survive a save
	dp := live()
	dp.Annotations = map[string]string{"devpod.io/http-id": "keepme"}
	if err := k8sClient.Update(ctx, &dp); err != nil {
		t.Fatal(err)
	}

	get := func(auth any) string {
		t.Helper()
		return call("GET", path, auth, "", http.StatusOK)["yaml"].(string)
	}
	text := get(alice)
	for _, want := range []string{"apiVersion: devpod.io/v1alpha1\n", "kind: DevPod\n", "  name: yy-alice-dev1\n", "  owner: yy-alice\n", "image: ubuntu:24.04\n", "  running: true\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("yaml lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"status:", "resourceVersion", "uid:", "managedFields", "keepme", "creationTimestamp"} {
		if strings.Contains(text, not) {
			t.Fatalf("yaml carries %q:\n%s", not, text)
		}
	}
	call("GET", path, bob, "", http.StatusNotFound)
	call("PUT", path, bob, jsonBody(map[string]string{"yaml": text}), http.StatusNotFound)
	if got := get(root); got != text {
		t.Fatalf("an admin reads something else:\n%s", got)
	}
	put := func(auth any, y string, want int) map[string]any {
		t.Helper()
		return call("PUT", path, auth, jsonBody(map[string]string{"yaml": y}), want)
	}

	t.Run("saving what was read changes nothing", func(t *testing.T) {
		before := live().ResourceVersion
		if out := put(alice, text, http.StatusOK); out["rebuild"] != false {
			t.Fatalf("out %v", out)
		}
		if live().ResourceVersion != before {
			t.Fatal("an unchanged manifest was written")
		}
	})

	t.Run("an edit replaces the spec and keeps the rest", func(t *testing.T) {
		edited := strings.Replace(text, "image: ubuntu:24.04", "image: ubuntu:25.04", 1)
		out := put(alice, edited, http.StatusOK)
		if out["rebuild"] != true { // running, and its pod definition changed
			t.Fatalf("out %v", out)
		}
		got := live()
		if got.Spec.Pod.Spec.Containers[0].Image != "ubuntu:25.04" || got.Annotations["devpod.io/http-id"] != "keepme" || got.Spec.Owner != "yy-alice" {
			t.Fatalf("after the save: %+v %v", got.Spec, got.Annotations)
		}
		// a field removed from the manifest is removed
		lim := got.Spec.Pod.Spec.Containers[0].Resources.Limits
		if lim.Cpu().Cmp(resource.MustParse("2")) != 0 {
			t.Fatalf("limits before: %v", lim)
		}
		noLimits := "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: yy-alice-dev1\nspec:\n  running: false\n  pod:\n    spec:\n      containers:\n      - name: dev\n        image: ubuntu:25.04\n"
		if out := put(alice, noLimits, http.StatusOK); out["rebuild"] != false { // stopped by the same save
			t.Fatalf("out %v", out)
		}
		got = live()
		if len(got.Spec.Pod.Spec.Containers[0].Resources.Limits) != 0 || got.Spec.Running || got.Spec.Owner != "yy-alice" {
			t.Fatalf("after dropping the limits: %+v", got.Spec)
		}
	})

	base := func(extra string) string {
		return "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: yy-alice-dev1\nspec:\n  owner: yy-alice\n  running: false\n  pod:\n" + extra
	}
	containers := "    spec:\n      containers:\n      - name: dev\n        image: ubuntu:25.04\n"

	t.Run("what a manifest may not do", func(t *testing.T) {
		code := func(y string, want int) string {
			t.Helper()
			c, _ := put(alice, y, want)["code"].(string)
			return c
		}
		msg := func(y string) string {
			t.Helper()
			m, _ := put(alice, y, http.StatusBadRequest)["message"].(string)
			return m
		}
		if m := msg("spec: [not, a, devpod"); !strings.Contains(m, "YAML") {
			t.Fatalf("bad yaml: %q", m)
		}
		if m := msg(base(containers) + "  nope: 1\n"); !strings.Contains(m, "nope") {
			t.Fatalf("unknown field: %q", m)
		}
		if m := msg(strings.Replace(base(containers), "name: yy-alice-dev1", "name: yy-alice-other", 1)); !strings.Contains(m, "renamed") {
			t.Fatalf("rename: %q", m)
		}
		if m := msg(strings.Replace(base(containers), "owner: yy-alice", "owner: yy-bob", 1)); !strings.Contains(m, "owner") {
			t.Fatalf("owner: %q", m)
		}
		if m := msg(strings.Replace(base(containers), "kind: DevPod", "kind: Pod", 1)); !strings.Contains(m, "DevPod") {
			t.Fatalf("kind: %q", m)
		}
		call("PUT", path, alice, `{"yaml":""}`, http.StatusBadRequest)
		// Secrets stay out of reach
		secret := base(containers + "      volumes:\n      - name: s\n        secret: {secretName: devpod-registry-yy-bob}\n")
		if c := code(secret, http.StatusBadRequest); c != "SECRET_REFERENCE" {
			t.Fatalf("secret volume: %q", c)
		}
		// CPU-binding annotations are a template's
		kore := base("    metadata:\n      annotations: {kore.zjusct.io/pin: 'true'}\n" + containers)
		if c := code(kore, http.StatusForbidden); c != "KORE_ANNOTATIONS_FORBIDDEN" {
			t.Fatalf("kore annotation: %q", c)
		}
		// the cluster's own refusals come with their reason
		if m := msg(base("    spec:\n      containers: []\n")); !strings.Contains(m, "update rejected: ") || !strings.Contains(m, "container") {
			t.Fatalf("no containers: %q", m)
		}
		// starting past the quota (8 CPUs)
		big := strings.Replace(base(containers+"        resources: {limits: {cpu: '100'}}\n"), "running: false", "running: true", 1)
		if c := code(big, http.StatusConflict); c != "QUOTA_EXCEEDED" {
			t.Fatalf("quota: %q", c)
		}
		if live().Spec.Pod.Spec.Containers[0].Image != "ubuntu:25.04" || live().Spec.Running {
			t.Fatalf("a refused save changed the DevPod: %+v", live().Spec)
		}
	})

	t.Run("nodeName becomes the node selector; the node has to exist", func(t *testing.T) {
		put(alice, base(containers+"      nodeName: yy-node\n"), http.StatusOK)
		got := live().Spec.Pod.Spec
		if got.NodeName != "" || got.NodeSelector["kubernetes.io/hostname"] != "yy-node" {
			t.Fatalf("spec %+v", got)
		}
		if m, _ := put(alice, base(containers+"      nodeSelector: {kubernetes.io/hostname: yy-nowhere}\n"), http.StatusBadRequest)["message"].(string); !strings.Contains(m, "yy-nowhere") {
			t.Fatalf("missing node: %q", m)
		}
	})

	t.Run("an admin may do what an admin may do elsewhere", func(t *testing.T) {
		secret := base(containers + "      volumes:\n      - name: s\n        secret: {secretName: some-secret}\n")
		put(root, secret, http.StatusOK)
		if v := live().Spec.Pod.Spec.Volumes; len(v) != 1 || v[0].Secret == nil {
			t.Fatalf("volumes %+v", v)
		}
		// and the owner cannot touch a pod definition that reaches a Secret without taking it out
		edited := strings.Replace(secret, "ubuntu:25.04", "ubuntu:26.04", 1)
		if c, _ := put(alice, edited, http.StatusBadRequest)["code"].(string); c != "SECRET_REFERENCE" {
			t.Fatalf("owner editing around an admin's Secret: %q", c)
		}
		// but may start and stop it from here: the pod definition is untouched
		put(alice, strings.Replace(secret, "running: false", "running: true", 1), http.StatusOK)
		if !live().Spec.Running {
			t.Fatal("not started")
		}
	})

	t.Run("a token reads with devpods:read and saves with devpods:update", func(t *testing.T) {
		mint := func(scope string) string {
			t.Helper()
			return call("POST", "/api/me/tokens", alice, jsonBody(map[string]any{"name": scope, "scopes": []string{scope}}), http.StatusCreated)["token"].(string)
		}
		read, update := mint("devpods:read"), mint("devpods:update")
		y := call("GET", path, read, "", http.StatusOK)["yaml"].(string)
		call("PUT", path, read, jsonBody(map[string]string{"yaml": y}), http.StatusForbidden)
		call("GET", path, update, "", http.StatusForbidden)
		call("PUT", path, update, jsonBody(map[string]string{"yaml": y}), http.StatusOK)
	})
	_ = time.Second
	_ = webui.SessionCookie
}

func TestPodDetail(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 11, 11, 18, 52, 29, 0, time.UTC))
	grace := int64(30)
	limit := resource.MustParse("1300Gi")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-x", Labels: map[string]string{"devpod.io/owner": "alice"}, CreationTimestamp: started},
		Spec: corev1.PodSpec{
			NodeName: "n1", RestartPolicy: corev1.RestartPolicyAlways, ServiceAccountName: "default", TerminationGracePeriodSeconds: &grace,
			HostNetwork:    true,
			NodeSelector:   map[string]string{"kubernetes.io/hostname": "n1"},
			Tolerations:    []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			InitContainers: []corev1.Container{{Name: "devpod-bootstrap", Image: "reg/supervisor:v1"}},
			Containers: []corev1.Container{{
				Name: "dev", Image: "ubuntu:24.04",
				Env: []corev1.EnvVar{{Name: "API_KEY", Value: "do-not-show"}},
				Resources: corev1.ResourceRequirements{
					Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
					Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "model", MountPath: "/models", ReadOnly: true}, {Name: "dshm", MountPath: "/dev/shm"}},
			}, {Name: "side", Image: "busybox"}},
			Volumes: []corev1.Volume{
				{Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data/models"}}},
				{Name: "dshm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &limit}}},
				{Name: "host", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "alice-x-hostkey"}}},
				{Name: "home", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "alice-x-home"}}},
				{Name: "tok", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{}}}}}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBestEffort, HostIP: "10.0.0.1", PodIP: "10.42.0.108", StartTime: &started,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", Message: "containers with unready status: [side]", LastTransitionTime: started},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: started},
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
			},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "devpod-bootstrap", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "dev", Ready: true, RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}},
				{Name: "side", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "busybox"`}}},
			},
		},
	}
	d := webui.PodDetailFor(pod)
	if d.Phase != "Running" || d.Ready != "1/2" || d.Restarts != 2 || d.QoS != "BestEffort" || d.Node != "n1" || d.HostIP != "10.0.0.1" || d.PodIP != "10.42.0.108" ||
		d.RestartPolicy != "Always" || d.ServiceAccount != "default" || *d.GracePeriod != 30 || !d.HostNetwork || d.StartedAt == nil || d.CreatedAt == nil {
		t.Fatalf("summary %+v", d)
	}
	var types_ []string
	for _, c := range d.Conditions {
		types_ = append(types_, c.Type+"="+c.Status)
	}
	if strings.Join(types_, ",") != "PodScheduled=True,Initialized=True,Ready=False" || d.Conditions[2].Reason != "ContainersNotReady" || d.Conditions[1].Since != nil {
		t.Fatalf("conditions %+v", d.Conditions)
	}
	if ic := d.InitContainers; len(ic) != 1 || ic[0].State != "terminated: exit 0 (Completed)" || ic[0].Tone != "ok" {
		t.Fatalf("init containers %+v", ic)
	}
	dev, side := d.Containers[0], d.Containers[1]
	if dev.State != "running since 2026-11-11T18:52:29Z" || dev.Tone != "ok" || dev.LastState != "terminated: exit 137 (OOMKilled)" || dev.Restarts != 2 || !dev.Ready ||
		dev.Limits["nvidia.com/gpu"] != "8" || dev.Requests["nvidia.com/gpu"] != "8" || strings.Join(dev.Mounts, "|") != "/models ← model (ro)|/dev/shm ← dshm" {
		t.Fatalf("dev %+v", dev)
	}
	if side.State != `waiting: ImagePullBackOff — Back-off pulling image "busybox"` || side.Tone != "fail" || side.Ready {
		t.Fatalf("side %+v", side)
	}
	var vols []string
	for _, v := range d.Volumes {
		vols = append(vols, v.Name+":"+v.Kind+":"+v.Detail)
	}
	if got := strings.Join(vols, "|"); got != "model:hostPath:/data/models|dshm:emptyDir:medium Memory, limit 1300Gi|host:secret:alice-x-hostkey|home:persistentVolumeClaim:alice-x-home|tok:projected:serviceAccountToken" {
		t.Fatalf("volumes %s", got)
	}
	if d.Labels["devpod.io/owner"] != "alice" || d.NodeSelector["kubernetes.io/hostname"] != "n1" || len(d.Tolerations) != 1 || d.Tolerations[0] != "nvidia.com/gpu:NoSchedule" {
		t.Fatalf("labels %v selector %v tolerations %v", d.Labels, d.NodeSelector, d.Tolerations)
	}
	// what a variable holds is nobody's to read here
	if b := jsonBody(d); strings.Contains(b, "do-not-show") || strings.Contains(b, "API_KEY") {
		t.Fatalf("environment in the view: %s", b)
	}
	// a pod the kubelet has not reported on yet
	bare := webui.PodDetailFor(&corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}, Status: corev1.PodStatus{Phase: corev1.PodPending}})
	if bare.Ready != "0/1" || bare.Containers[0].State != "" || bare.Conditions == nil || bare.Volumes == nil || bare.Labels == nil || bare.Tolerations == nil {
		t.Fatalf("bare %+v", bare)
	}
}

func TestPodDetailAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	t.Cleanup(func() { cleanupDevPods(t) })
	alice, bob := forge(sm, "pd-alice", false), forge(sm, "pd-bob", false)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	apiCall(t, srv, "POST", "/api/devpods", alice, createBody, http.StatusCreated)
	waitDevPod(t, "pd-alice-dev1")
	// no controller here: no Pod yet
	if out := apiCall(t, srv, "GET", "/api/devpods/pd-alice-dev1/pod", alice, "", http.StatusOK); out["pod"] != nil {
		t.Fatalf("no pod: %v", out)
	}
	apiCall(t, srv, "GET", "/api/devpods/pd-alice-dev1/pod", bob, "", http.StatusNotFound)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pd-alice-dev1", Namespace: "devpods"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}}},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	out := apiCall(t, srv, "GET", "/api/devpods/pd-alice-dev1/pod", alice, "", http.StatusOK)
	p, _ := out["pod"].(map[string]any)
	if p == nil || p["name"] != "pd-alice-dev1" || p["ready"] != "0/1" || len(p["containers"].([]any)) != 1 {
		t.Fatalf("pod: %v", out)
	}
}
