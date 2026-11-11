// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// The user's registry credentials (snapshot spec v2 §5): pending ones are
// merged into the Secret and cleared, removed hosts are pruned, and the
// Secret reaches new Pods without recreating running ones.
func TestUserRegistryCredentials(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	secKey := types.NamespacedName{Name: "devpod-registry-rita", Namespace: "devpods"}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	auths := func() map[string]map[string]string {
		var sec corev1.Secret
		if err := env.Client.Get(env.Ctx, secKey, &sec); err != nil {
			return nil
		}
		var cfg struct {
			Auths map[string]map[string]string `json:"auths"`
		}
		_ = json.Unmarshal(sec.Data[corev1.DockerConfigJsonKey], &cfg)
		if cfg.Auths == nil {
			cfg.Auths = map[string]map[string]string{}
		}
		return cfg.Auths
	}
	edit := func(fn func(u *devpodv1alpha1.User)) {
		t.Helper()
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var u devpodv1alpha1.User
			if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "rita"}, &u); err != nil {
				return err
			}
			fn(&u)
			return env.Client.Update(env.Ctx, &u)
		}); err != nil {
			t.Fatal(err)
		}
	}
	pendingCleared := func() bool {
		var u devpodv1alpha1.User
		return env.Client.Get(env.Ctx, types.NamespacedName{Name: "rita"}, &u) == nil && len(u.Spec.RegistryAuth) == 0
	}
	devpod := func(name string) *devpodv1alpha1.DevPod {
		return &devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{Owner: "rita", Running: true,
				Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}},
		}
	}
	podOf := func(name string) *corev1.Pod {
		var pod corev1.Pod
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: name, Namespace: "devpods"}, &pod); err != nil {
			return nil
		}
		return &pod
	}
	pulls := func(p *corev1.Pod) []string {
		var out []string
		for _, r := range p.Spec.ImagePullSecrets {
			out = append(out, r.Name)
		}
		return out
	}

	if err := env.Client.Create(env.Ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "rita"}}); err != nil {
		t.Fatal(err)
	}
	// a DevPod from before any credential
	if err := env.Client.Create(env.Ctx, devpod("rita-before")); err != nil {
		t.Fatal(err)
	}
	eventually("pod of rita-before", func() bool { return podOf("rita-before") != nil })
	beforeUID := podOf("rita-before").UID
	if got := pulls(podOf("rita-before")); len(got) != 0 {
		t.Fatalf("pull secrets without credentials: %v", got)
	}

	// a pending credential becomes the Secret and is cleared from the User
	edit(func(u *devpodv1alpha1.User) {
		u.Spec.Registries = []devpodv1alpha1.UserRegistry{{Host: "reg.example.com", Username: "rita"}}
		u.Spec.RegistryAuth = []devpodv1alpha1.UserRegistryAuth{{Host: "reg.example.com", Username: "rita", Password: "s3cret"}}
	})
	eventually("secret with the first host", func() bool { return auths()["reg.example.com"]["password"] == "s3cret" })
	eventually("registryAuth cleared", pendingCleared)
	var sec corev1.Secret
	if err := env.Client.Get(env.Ctx, secKey, &sec); err != nil {
		t.Fatal(err)
	}
	if sec.Type != corev1.SecretTypeDockerConfigJson || sec.Labels["devpod.io/owner"] != "rita" || sec.Labels["devpod.io/registry-credentials"] != "true" || auths()["reg.example.com"]["auth"] == "" {
		t.Fatalf("secret %s labels %v auths %v", sec.Type, sec.Labels, auths())
	}

	// a second host is merged in; Docker Hub is stored under its index URL
	edit(func(u *devpodv1alpha1.User) {
		u.Spec.Registries = append(u.Spec.Registries, devpodv1alpha1.UserRegistry{Host: "docker.io", Username: "ritahub"})
		u.Spec.RegistryAuth = []devpodv1alpha1.UserRegistryAuth{{Host: "docker.io", Username: "ritahub", Password: "hubpw"}}
	})
	eventually("both hosts", func() bool {
		a := auths()
		return a["https://index.docker.io/v1/"]["password"] == "hubpw" && a["reg.example.com"]["password"] == "s3cret"
	})
	eventually("registryAuth cleared again", pendingCleared)

	// a DevPod created now pulls with the Secret; the earlier Pod is untouched
	if err := env.Client.Create(env.Ctx, devpod("rita-after")); err != nil {
		t.Fatal(err)
	}
	eventually("pod of rita-after with the pull secret", func() bool {
		p := podOf("rita-after")
		return p != nil && slices.Contains(pulls(p), "devpod-registry-rita")
	})
	afterUID := podOf("rita-after").UID

	// a host removed from the list is pruned; the last one removes the Secret
	edit(func(u *devpodv1alpha1.User) { u.Spec.Registries = u.Spec.Registries[1:] })
	eventually("first host pruned", func() bool {
		a := auths()
		_, has := a["reg.example.com"]
		return a != nil && !has && len(a) == 1
	})
	edit(func(u *devpodv1alpha1.User) { u.Spec.Registries = nil })
	eventually("secret deleted", func() bool {
		var s corev1.Secret
		return apierrors.IsNotFound(env.Client.Get(env.Ctx, secKey, &s))
	})

	// neither adding nor removing credentials recreated a Pod: poke both
	// DevPods so they reconcile again, then compare
	for _, name := range []string{"rita-before", "rita-after"} {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var dp devpodv1alpha1.DevPod
			if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: name, Namespace: "devpods"}, &dp); err != nil {
				return err
			}
			if dp.Annotations == nil {
				dp.Annotations = map[string]string{}
			}
			dp.Annotations["test/poke"] = "1"
			return env.Client.Update(env.Ctx, &dp)
		}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(2 * time.Second)
	if p := podOf("rita-before"); p == nil || p.UID != beforeUID || len(pulls(p)) != 0 {
		t.Fatalf("rita-before was recreated or changed: %+v", p)
	}
	if p := podOf("rita-after"); p == nil || p.UID != afterUID || !slices.Contains(pulls(p), "devpod-registry-rita") {
		t.Fatalf("rita-after was recreated or lost its pull secret: %+v", p)
	}
}
