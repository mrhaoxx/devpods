// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestRegistryHosts(t *testing.T) {
	for in, want := range map[string]string{
		"Harbor.Example.com":       "harbor.example.com",
		" reg.example.com:5000 ":   "reg.example.com:5000",
		"index.docker.io":          "docker.io",
		"localhost:5000":           "localhost:5000",
		"https://reg.example.com":  "",
		"reg.example.com/team":     "",
		"user@reg.example.com":     "",
		"reg.example.com:":         "",
		"":                         "",
		"-bad.example.com":         "",
		"reg.example.com:5000/x:y": "",
	} {
		if got := webui.NormalizeRegistryHostForTest(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"harbor.example.com/team/env:v1": "harbor.example.com",
		"reg.example.com:5000/env:v1":    "reg.example.com:5000",
		"ubuntu:24.04":                   "docker.io",
		"library/ubuntu:24.04":           "docker.io",
		"localhost/env:v1":               "localhost",
		"index.docker.io/u/env:v1":       "docker.io",
	} {
		if got := webui.ImageRegistryHostForTest(in); got != want {
			t.Errorf("imageHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every way a PodSpec can name a Secret is refused for a user's own
// PodSpec, with the field path; the one exception is the user's own
// registry credentials as an image pull secret.
func TestGuardUserPodSpec(t *testing.T) {
	ref := &corev1.LocalObjectReference{Name: "x"}
	sel := &corev1.SecretKeySelector{LocalObjectReference: *ref, Key: "k"}
	vol := func(src corev1.VolumeSource) corev1.PodSpec {
		return corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: src}}}
	}
	cases := []struct {
		name string
		spec corev1.PodSpec
		code string
		path string
	}{
		{"plain", corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Env: []corev1.EnvVar{{Name: "A", Value: "b"}}}}}, "", ""},
		{"emptyDir and configMap volumes", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "e", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}, "", ""},
		{"secret volume", vol(corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "x"}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].secret"},
		{"projected secret", vol(corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{}}, {Secret: &corev1.SecretProjection{LocalObjectReference: *ref}}}}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].projected.sources[1].secret"},
		{"csi", vol(corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "d", NodePublishSecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].csi.nodePublishSecretRef"},
		{"cephfs", vol(corev1.VolumeSource{CephFS: &corev1.CephFSVolumeSource{SecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].cephfs.secretRef"},
		{"rbd", vol(corev1.VolumeSource{RBD: &corev1.RBDVolumeSource{SecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].rbd.secretRef"},
		{"iscsi", vol(corev1.VolumeSource{ISCSI: &corev1.ISCSIVolumeSource{SecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].iscsi.secretRef"},
		{"flexVolume", vol(corev1.VolumeSource{FlexVolume: &corev1.FlexVolumeSource{SecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].flexVolume.secretRef"},
		{"scaleIO", vol(corev1.VolumeSource{ScaleIO: &corev1.ScaleIOVolumeSource{SecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].scaleIO.secretRef"},
		{"storageos", vol(corev1.VolumeSource{StorageOS: &corev1.StorageOSVolumeSource{SecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].storageos.secretRef"},
		{"cinder", vol(corev1.VolumeSource{Cinder: &corev1.CinderVolumeSource{SecretRef: ref}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].cinder.secretRef"},
		{"azureFile", vol(corev1.VolumeSource{AzureFile: &corev1.AzureFileVolumeSource{SecretName: "x"}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].azureFile.secretName"},
		{"env secretKeyRef", corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "dev", Env: []corev1.EnvVar{{Name: "A", Value: "b"}, {Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: sel}}}}}}, "SECRET_REFERENCE", "spec.pod.spec.containers[1].env[1].valueFrom.secretKeyRef"},
		{"envFrom secretRef", corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: *ref}}}}}}, "SECRET_REFERENCE", "spec.pod.spec.containers[0].envFrom[0].secretRef"},
		{"initContainer env", corev1.PodSpec{InitContainers: []corev1.Container{{Name: "i", Env: []corev1.EnvVar{{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: sel}}}}}}, "SECRET_REFERENCE", "spec.pod.spec.initContainers[0].env[0].valueFrom.secretKeyRef"},
		{"ephemeralContainer envFrom", corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "e", EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: *ref}}}}}}}, "SECRET_REFERENCE", "spec.pod.spec.ephemeralContainers[0].envFrom[0].secretRef"},
		{"someone's pull secret", corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "devpod-registry-bob"}}}, "SECRET_REFERENCE", "spec.pod.spec.imagePullSecrets[0]"},
		{"the platform push secret as a pull secret", corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "platform-push"}}}, "SECRET_REFERENCE", "spec.pod.spec.imagePullSecrets[0]"},
		{"own registry credentials as a pull secret", corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "devpod-registry-alice"}}}, "", ""},
		{"own registry credentials as a volume", vol(corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "devpod-registry-alice"}}), "SECRET_REFERENCE", "spec.pod.spec.volumes[0].secret"},
		{"service account", corev1.PodSpec{ServiceAccountName: "devpod-webui"}, "SERVICE_ACCOUNT", "spec.pod.spec.serviceAccountName"},
		{"default service account by name", corev1.PodSpec{ServiceAccountName: "default"}, "", ""},
		{"automount on", corev1.PodSpec{AutomountServiceAccountToken: ptr.To(true)}, "SERVICE_ACCOUNT", "spec.pod.spec.automountServiceAccountToken"},
		{"automount off", corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false)}, "", ""},
		{"projected service account token", vol(corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "t"}}}}}), "SERVICE_ACCOUNT", "spec.pod.spec.volumes[0].projected.sources[0].serviceAccountToken"},
	}
	for _, tc := range cases {
		if code, path := webui.GuardUserPodSpecForTest(&tc.spec, "alice"); code != tc.code || path != tc.path {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, code, path, tc.code, tc.path)
		}
	}
}

func TestRegistriesAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.Admins = map[string]bool{"rg-root": true}
	ctx := context.Background()
	for _, name := range []string{"rg-alice", "rg-bob", "rg-root"} {
		u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8sClient.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, u) })
	}
	t.Cleanup(func() { cleanupDevPods(t) })
	alice, bob, root := forge(sm, "rg-alice", false), forge(sm, "rg-bob", false), forge(sm, "rg-root", true)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	do := func(method, path string, auth any, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		switch a := auth.(type) {
		case *http.Cookie:
			req.AddCookie(a)
		case string:
			req.Header.Set("Authorization", "Bearer "+a)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, b
	}
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			code, b := do(method, path, auth, body)
			if code == want {
				var out map[string]any
				_ = json.Unmarshal(b, &out)
				return out
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s: %d (want %d) %s", method, path, code, want, b)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	user := func(name string) devpodv1alpha1.User {
		var u devpodv1alpha1.User
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}

	t.Run("save, replace, list and remove", func(t *testing.T) {
		if items := call("GET", "/api/me/registries", alice, "", http.StatusOK)["items"].([]any); len(items) != 0 {
			t.Fatalf("empty list: %v", items)
		}
		code, raw := do("PUT", "/api/me/registries/Harbor.Example.com", alice, `{"username":"alice","password":"p4ss-w0rd"}`)
		if code != http.StatusOK || strings.Contains(string(raw), "p4ss-w0rd") {
			t.Fatalf("put: %d %s", code, raw)
		}
		u := user("rg-alice")
		if len(u.Spec.Registries) != 1 || u.Spec.Registries[0].Host != "harbor.example.com" || u.Spec.Registries[0].Username != "alice" ||
			len(u.Spec.RegistryAuth) != 1 || u.Spec.RegistryAuth[0].Password != "p4ss-w0rd" || u.Spec.RegistryAuth[0].Host != "harbor.example.com" {
			t.Fatalf("stored: %+v / %d pending", u.Spec.Registries, len(u.Spec.RegistryAuth))
		}
		// replacing keeps one entry and one pending credential for the host
		call("PUT", "/api/me/registries/harbor.example.com", alice, `{"username":"alice2","password":"new"}`, http.StatusOK)
		call("PUT", "/api/me/registries/reg.example.com:5000", alice, `{"username":"a","password":"b"}`, http.StatusOK)
		u = user("rg-alice")
		if len(u.Spec.Registries) != 2 || len(u.Spec.RegistryAuth) != 2 || u.Spec.Registries[0].Username != "alice2" {
			t.Fatalf("after replace: %+v", u.Spec.Registries)
		}
		code, raw = do("GET", "/api/me/registries", alice, "")
		if code != http.StatusOK || strings.Contains(string(raw), "new") || strings.Contains(string(raw), "password") {
			t.Fatalf("list leaks: %s", raw)
		}
		var list struct {
			Items []struct {
				Host, Username string
				Pending        bool
			}
		}
		_ = json.Unmarshal(raw, &list)
		if len(list.Items) != 2 || list.Items[0].Host != "harbor.example.com" || !list.Items[0].Pending || list.Items[1].Host != "reg.example.com:5000" {
			t.Fatalf("list: %+v", list.Items)
		}
		// once the controller has cleared registryAuth nothing is pending
		u = user("rg-alice")
		u.Spec.RegistryAuth = nil
		if err := k8sClient.Update(ctx, &u); err != nil {
			t.Fatal(err)
		}
		_, raw = do("GET", "/api/me/registries", alice, "")
		_ = json.Unmarshal(raw, &list)
		if list.Items[0].Pending {
			t.Fatalf("still pending: %s", raw)
		}
		// other users see nothing of it
		if items := call("GET", "/api/me/registries", bob, "", http.StatusOK)["items"].([]any); len(items) != 0 {
			t.Fatalf("bob sees %v", items)
		}
		// host:port arrives escaped (browser) or raw (Go client), in any case
		code, raw = do("PUT", "/api/me/registries/Port.Example.com%3A5443", alice, `{"username":"p","password":"q"}`)
		if code != http.StatusOK || len(raw) == 0 || !strings.Contains(string(raw), `"port.example.com:5443"`) {
			t.Fatalf("escaped host: %d %s", code, raw)
		}
		call("DELETE", "/api/me/registries/port.example.com:5443", alice, "", http.StatusNoContent)
		// Docker Hub is one registry under all its names
		call("PUT", "/api/me/registries/index.docker.io", alice, `{"username":"hub","password":"q"}`, http.StatusOK)
		if u := user("rg-alice"); u.Spec.Registries[len(u.Spec.Registries)-1].Host != "docker.io" {
			t.Fatalf("docker hub host: %+v", u.Spec.Registries)
		}
		call("DELETE", "/api/me/registries/registry-1.docker.io", alice, "", http.StatusNoContent)
		call("DELETE", "/api/me/registries/harbor.example.com", alice, "", http.StatusNoContent)
		call("DELETE", "/api/me/registries/harbor.example.com", alice, "", http.StatusNotFound)
		if u := user("rg-alice"); len(u.Spec.Registries) != 1 {
			t.Fatalf("after delete: %+v", u.Spec.Registries)
		}
	})

	t.Run("validation and limits", func(t *testing.T) {
		call("PUT", "/api/me/registries/reg.example.com", alice, `{"username":"a"}`, http.StatusBadRequest)
		call("PUT", "/api/me/registries/reg.example.com", alice, `{"username":"","password":"x"}`, http.StatusBadRequest)
		call("PUT", "/api/me/registries/bad_host", alice, `{"username":"a","password":"b"}`, http.StatusBadRequest)
		// a malformed body is not echoed back
		if code, raw := do("PUT", "/api/me/registries/reg.example.com", alice, `{"username":"a","password":"leak-me",`); code != http.StatusBadRequest || strings.Contains(string(raw), "leak-me") {
			t.Fatalf("malformed: %d %s", code, raw)
		}
		for i := 0; i < 20; i++ {
			call("PUT", fmt.Sprintf("/api/me/registries/r%d.example.com", i), bob, `{"username":"b","password":"p"}`, http.StatusOK)
		}
		call("PUT", "/api/me/registries/one-more.example.com", bob, `{"username":"b","password":"p"}`, http.StatusConflict)
		call("PUT", "/api/me/registries/r3.example.com", bob, `{"username":"b2","password":"p"}`, http.StatusOK) // replacing is fine at the cap
	})

	t.Run("tokens need the registries scopes; DevPod-limited tokens cannot use them", func(t *testing.T) {
		mint := func(body string) string {
			return call("POST", "/api/me/tokens", alice, body, http.StatusCreated)["token"].(string)
		}
		ro := mint(`{"name":"ro","scopes":["registries:read"]}`)
		call("GET", "/api/me/registries", ro, "", http.StatusOK)
		call("PUT", "/api/me/registries/t.example.com", ro, `{"username":"a","password":"b"}`, http.StatusForbidden)
		rw := mint(`{"name":"rw","scopes":["registries:write"]}`)
		call("PUT", "/api/me/registries/t.example.com", rw, `{"username":"a","password":"b"}`, http.StatusOK)
		call("GET", "/api/me/registries", rw, "", http.StatusForbidden)
		limited := mint(`{"name":"lim","scopes":["registries:read","registries:write"],"devpods":["rg-alice-x"]}`)
		call("GET", "/api/me/registries", limited, "", http.StatusForbidden)
		call("PUT", "/api/me/registries/t.example.com", limited, `{"username":"a","password":"b"}`, http.StatusForbidden)
	})

	t.Run("a user's YAML cannot reach Secrets; admins and presets can", func(t *testing.T) {
		yamlFor := func(name, owner, extra string) string {
			return fmt.Sprintf(`{"yaml":%q}`, fmt.Sprintf(`
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: %s
spec:
  owner: %s
  running: false
  pod:
    spec:
      containers:
      - name: dev
        image: ubuntu:24.04
        resources:
          limits: {cpu: "1", memory: "1Gi"}
%s`, name, owner, extra))
		}
		secretVol := "      volumes:\n      - name: s\n        secret: {secretName: devpod-registry-rg-bob}\n"
		out := call("POST", "/api/devpods", alice, yamlFor("rg-alice-steal", "rg-alice", secretVol), http.StatusBadRequest)
		if out["code"] != "SECRET_REFERENCE" || !strings.Contains(fmt.Sprint(out["detail"]), "spec.pod.spec.volumes[0].secret") {
			t.Fatalf("secret volume: %v", out)
		}
		out = call("POST", "/api/devpods", alice, yamlFor("rg-alice-pull", "rg-alice", "      imagePullSecrets:\n      - name: platform-push\n"), http.StatusBadRequest)
		if out["code"] != "SECRET_REFERENCE" {
			t.Fatalf("pull secret: %v", out)
		}
		out = call("POST", "/api/devpods", alice, yamlFor("rg-alice-sa", "rg-alice", "      serviceAccountName: devpod-webui\n"), http.StatusBadRequest)
		if out["code"] != "SERVICE_ACCOUNT" {
			t.Fatalf("service account: %v", out)
		}
		// one's own registry credentials as a pull secret, and plain YAML, are fine
		call("POST", "/api/devpods", alice, yamlFor("rg-alice-own", "rg-alice", "      imagePullSecrets:\n      - name: devpod-registry-rg-alice\n"), http.StatusCreated)
		call("POST", "/api/devpods", alice, yamlFor("rg-alice-plain", "rg-alice", ""), http.StatusCreated)
		// admins are not restricted
		call("POST", "/api/devpods", root, yamlFor("rg-root-sec", "rg-root", secretVol), http.StatusCreated)

		// a preset an admin wrote may carry a Secret; a user creating from it is fine
		tpl := &devpodv1alpha1.DevPodTemplate{ObjectMeta: metav1.ObjectMeta{Name: "rg-preset-secret"}}
		tpl.Spec = devpodv1alpha1.DevPodTemplateSpec{DisplayName: "with a secret", PodPreset: &devpodv1alpha1.PodPresetSpec{Pod: devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}},
			Volumes: []corev1.Volume{{Name: "lic", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "site-license"}}}},
		}}}}
		if err := k8sClient.Create(ctx, tpl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, tpl) })
		call("POST", "/api/devpods", alice, `{"name":"frompreset","templateRef":"rg-preset-secret"}`, http.StatusCreated)
		var dp devpodv1alpha1.DevPod
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: "rg-alice-frompreset", Namespace: "devpods"}, &dp); err != nil || len(dp.Spec.Pod.Spec.Volumes) != 1 {
			t.Fatalf("preset devpod: %v %+v", err, dp.Spec.Pod)
		}
	})
}
