// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
)

// The cluster's pull secrets go on every DevPod Pod, whatever else it
// carries: Kubernetes adds the ServiceAccount's only to a Pod naming none,
// and the supervisor may live in a registry only they open.
func TestClusterPullSecrets(t *testing.T) {
	setupSuiteWith(t, func(r *controllers.DevPodReconciler) { r.PullSecrets = []string{"ccr-pull"} })
	env := newTestEnv(t)
	pulls := func(name string) []string {
		deadline := time.Now().Add(20 * time.Second)
		for {
			var pod corev1.Pod
			if env.Client.Get(env.Ctx, types.NamespacedName{Name: name, Namespace: "devpods"}, &pod) == nil {
				var out []string
				for _, r := range pod.Spec.ImagePullSecrets {
					out = append(out, r.Name)
				}
				slices.Sort(out)
				return out
			}
			if time.Now().After(deadline) {
				t.Fatalf("no pod %s", name)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	devpod := func(name, owner string, own ...string) *devpodv1alpha1.DevPod {
		spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}
		for _, s := range own {
			spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
		}
		return &devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
			Spec:       devpodv1alpha1.DevPodSpec{Owner: owner, Running: true, Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: spec}},
		}
	}
	// nothing of its own
	if err := env.Client.Create(env.Ctx, devpod("pia-plain", "pia")); err != nil {
		t.Fatal(err)
	}
	if got := pulls("pia-plain"); !slices.Equal(got, []string{"ccr-pull"}) {
		t.Fatalf("plain: %v", got)
	}
	// one the user wrote (and the cluster's named again: once)
	if err := env.Client.Create(env.Ctx, devpod("pia-own", "pia", "mine", "ccr-pull")); err != nil {
		t.Fatal(err)
	}
	if got := pulls("pia-own"); !slices.Equal(got, []string{"ccr-pull", "mine"}) {
		t.Fatalf("the user's own: %v", got)
	}
	// the owner's registry credentials
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.RegistryCredentialsSecretName("quin"), Namespace: "devpods",
			Labels: map[string]string{devpodv1alpha1.RegistryCredentialsLabel: "true", devpodv1alpha1.OwnerLabel: "quin"}},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}
	if err := env.Client.Create(env.Ctx, sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Client.Delete(env.Ctx, sec) })
	if err := env.Client.Create(env.Ctx, devpod("quin-dev", "quin")); err != nil {
		t.Fatal(err)
	}
	if got := pulls("quin-dev"); !slices.Equal(got, []string{"ccr-pull", "devpod-registry-quin"}) {
		t.Fatalf("with the owner's credentials: %v", got)
	}
}
