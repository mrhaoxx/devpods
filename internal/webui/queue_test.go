// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// queueOn: the queue switched on, with its defaults.
func queueOn(s *webui.Server) {
	s.Queue = &webui.QueueRules{DefaultTimeLimit: 24 * time.Hour, MaxTimeLimit: 72 * time.Hour, GangTimeout: 5 * time.Minute,
		PreemptGrace: 2 * time.Minute, Tick: 5 * time.Second, KeepEnded: 30 * 24 * time.Hour}
	controllerSays(devpodv1alpha1.FeatureJobs, "/home")
}

// controllerSays: the cluster's controller says it can do features, with
// homes shared at sharedHomes ("" not).
func controllerSays(features, sharedHomes string) {
	ctx := context.Background()
	data := map[string]string{devpodv1alpha1.ControllerVersionKey: "test", devpodv1alpha1.ControllerFeaturesKey: features}
	if sharedHomes != "" {
		data[devpodv1alpha1.ControllerSharedHomesKey] = sharedHomes
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.ControllerFeaturesConfigMap, Namespace: "devpods"}, Data: data}
	if err := k8sClient.Create(ctx, cm); apierrors.IsAlreadyExists(err) {
		var cur corev1.ConfigMap
		if k8sClient.Get(ctx, client.ObjectKeyFromObject(cm), &cur) == nil {
			cur.Data = data
			_ = k8sClient.Update(ctx, &cur)
		}
	}
}

// priorityRules makes the rules these, and waits until the console sees
// them.
func priorityRules(t *testing.T, rules map[string]devpodv1alpha1.JobPriorityRuleSpec) {
	t.Helper()
	ctx := context.Background()
	for name, spec := range rules {
		r := &devpodv1alpha1.JobPriorityRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
		if err := k8sClient.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, r) })
	}
	waitFor(t, func() bool {
		var l devpodv1alpha1.JobPriorityRuleList
		return k8sClient.List(ctx, &l) == nil && len(l.Items) >= len(rules)
	})
}

// A user's cap is the highest of the rules that hold for them there; none
// is 0; an admin has none; a broken rule gives nothing.
func TestPriorityCap(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	queueOn(s)
	s.Admins = map[string]bool{"pc-root": true}
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{
		"pc-alice":  {Condition: "user = pc-alice", MaxPriority: 200},
		"pc-lab":    {Condition: "group = pc-lab", MaxPriority: 100},
		"pc-st2":    {Condition: "cluster = st2", MaxPriority: 50},
		"pc-broken": {Condition: "user = = ", MaxPriority: 900},
	})
	ctx := context.Background()
	for _, c := range []struct {
		user      string
		groups    map[string]bool
		cluster   string
		want      int32
		unlimited bool
	}{
		{"pc-alice", nil, "st", 200, false},
		{"pc-bob", map[string]bool{"pc-lab": true}, "st", 100, false},
		{"pc-carol", nil, "st2", 50, false},
		{"pc-dave", nil, "st", 0, false},
		{"pc-root", nil, "st", 0, true},
	} {
		got, unlimited, err := s.PriorityCapForTest(ctx, c.user, c.groups, c.cluster)
		if err != nil || got != c.want || unlimited != c.unlimited {
			t.Errorf("%s in %s: %d unlimited=%v %v", c.user, c.cluster, got, unlimited, err)
		}
	}
}

// A cluster's own maximum time limit wins over the console's.
func TestMaxTimeLimit(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	queueOn(s)
	ctx := context.Background()
	if got := s.MaxTimeLimitForTest(ctx, ""); got != 72*time.Hour {
		t.Fatalf("console's: %s", got)
	}
	c := &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "mt-st"}, Spec: devpodv1alpha1.ClusterSpec{JobMaxTimeLimit: &metav1.Duration{Duration: 12 * time.Hour}}}
	if err := k8sClient.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, c) })
	waitFor(t, func() bool { return s.MaxTimeLimitForTest(ctx, "mt-st") == 12*time.Hour })
}

// /api/me says whether the queue is on.
func TestMeQueueFeature(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	for _, on := range []bool{false, true} {
		if on {
			queueOn(s)
		}
		rec := doJSON(t, s.HandleMeForTest(), "GET", "/api/me", nil, forge(sm, "mq-alice", false), "")
		var me struct{ Features map[string]bool }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &me) != nil || me.Features["queue"] != on {
			t.Fatalf("queue %v: %d %s", on, rec.Code, rec.Body)
		}
	}
}
