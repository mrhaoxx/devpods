// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

func testJob() *devpodv1alpha1.Job {
	return &devpodv1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "abc12"},
		Spec: devpodv1alpha1.JobSpec{
			Owner: "alice", Title: "train", Command: "python t.py", Args: []string{"--lr", "1"}, Workdir: "/home/alice/x",
			Nodes: 2, GPUsPerNode: 4, CPUsPerNode: resource.MustParse("32"), TimeLimit: metav1.Duration{Duration: 2 * time.Hour},
			Env: []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
			Template: devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "dev", Image: "pytorch:2.6", Env: []corev1.EnvVar{{Name: "RANK", Value: "stale"}, {Name: "KEEP", Value: "1"}},
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{gpures.NVIDIA: resource.MustParse("8"), corev1.ResourceCPU: resource.MustParse("64")}}}},
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: hostnameLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"g1", "g2"}}}}}}}},
			}},
		},
	}
}

func envValue(c corev1.Container, name string) string {
	v := ""
	for _, e := range c.Env {
		if e.Name == name {
			v = e.Value // the last one counts
		}
	}
	return v
}

// A rank is a DevPod of its owner pinned to its machine, running the job's
// command in a login bash with what it needs to find the others.
func TestRankDevPod(t *testing.T) {
	job := testJob()
	master := masterAddr(job, rankName("alice", "abc12", 0), "devpods", false, "")
	dp := rankDevPod(job, 1, "g2", "devpods", master)
	if dp.Name != "alice-abc12-1" || dp.Namespace != "devpods" || dp.Spec.Owner != "alice" || !dp.Spec.Running || dp.Spec.Persistence != nil {
		t.Fatalf("rank: %s/%s owner %q", dp.Namespace, dp.Name, dp.Spec.Owner)
	}
	if dp.Spec.Job == nil || dp.Spec.Job.Name != "abc12" || dp.Spec.Job.Rank != 1 || dp.Labels["devpod.io/job"] != "abc12" {
		t.Fatalf("job %+v labels %v", dp.Spec.Job, dp.Labels)
	}
	spec := dp.Spec.Pod.Spec
	// Kubernetes ends it too if nobody else does: its time limit and ten
	// minutes more (the console's end comes first)
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 2*3600+600 {
		t.Fatalf("activeDeadlineSeconds %v", spec.ActiveDeadlineSeconds)
	}
	if spec.NodeSelector[hostnameLabel] != "g2" || spec.Affinity != nil {
		t.Fatalf("placement: %v %+v", spec.NodeSelector, spec.Affinity)
	}
	c := spec.Containers[0]
	if !slices.Equal(c.Command, []string{"/opt/devpod/bin/bash", "-lc", "python t.py", "train", "--lr", "1"}) || c.Args != nil || c.WorkingDir != "/home/alice/x" {
		t.Fatalf("command %q args %q dir %q", c.Command, c.Args, c.WorkingDir)
	}
	lo, hi := jobPortsOf(job)
	for k, want := range map[string]string{"RANK": "1", "NODE_RANK": "1", "LOCAL_RANK": "0", "WORLD_SIZE": "2", "MASTER_ADDR": "alice-abc12-0.devpods.svc",
		"MASTER_PORT": itoa(lo), "DEVPOD_JOB": "abc12", "DEVPOD_JOB_PORTS": itoa(lo) + "-" + itoa(hi), "FOO": "bar", "KEEP": "1"} {
		if got := envValue(c, k); got != want {
			t.Errorf("%s=%q, want %q", k, got, want)
		}
	}
	if q := c.Resources.Limits[gpures.NVIDIA]; q.Value() != 4 {
		t.Errorf("gpu limit %s", q.String())
	}
	if q := c.Resources.Limits[corev1.ResourceCPU]; q.String() != "32" {
		t.Errorf("cpu limit %s", q.String())
	}
	if q := c.Resources.Requests[corev1.ResourceCPU]; q.String() != "32" {
		t.Errorf("cpu request %s", q.String())
	}
	if a := jobAsk(job); a != (amount{4, 32000}) {
		t.Errorf("ask %+v", a)
	}
	// host network: rank 0's machine's address
	if m := masterAddr(job, "alice-abc12-0", "devpods", true, "10.0.0.7"); m != "10.0.0.7" {
		t.Errorf("host network master %q", m)
	}
	// the name budget (the submit check uses this)
	if n := rankName(strings.Repeat("u", 16), "abc12", 10); len(n) <= 22 {
		t.Errorf("%q fits 22", n)
	}
	if lo < 20000 || hi != lo+15 || hi > 29999 {
		t.Errorf("ports %d-%d", lo, hi)
	}
	if lo2, _ := jobPortsOf(job); lo2 != lo {
		t.Error("ports not stable")
	}
	// the block the scheduler chose, once chosen
	job.Status.PortBase = 20160
	if r := rankDevPod(job, 0, "g1", "devpods", ""); envValue(r.Spec.Pod.Spec.Containers[0], "MASTER_PORT") != "20160" ||
		envValue(r.Spec.Pod.Spec.Containers[0], "DEVPOD_JOB_PORTS") != "20160-20175" {
		t.Errorf("chosen block: %s", envValue(r.Spec.Pod.Spec.Containers[0], "DEVPOD_JOB_PORTS"))
	}
}

// Port blocks lie below the NodePort range, and a job takes one no job
// running on its machines has.
func TestPickPorts(t *testing.T) {
	for _, id := range []string{"a", "b", "zz9", "q1w2e", "abc12", "kkkkk"} {
		lo, hi := jobPorts(id)
		if lo < 20000 || hi > 29999 || (lo-20000)%16 != 0 {
			t.Errorf("%s: %d-%d", id, lo, hi)
		}
	}
	j := testJob()
	home, _ := jobPorts(j.Name)
	if got := pickPorts(j, []string{"g1", "g2"}, map[string]map[int32]bool{"g3": {home: true}}); got != home {
		t.Errorf("free elsewhere: %d, want %d", got, home)
	}
	used := map[string]map[int32]bool{"g2": {home: true, home + 16: true}}
	if got := pickPorts(j, []string{"g1", "g2"}, used); got == home || got == home+16 || got < 20000 || got > 29984 {
		t.Errorf("taken on g2: %d", got)
	}
}

// A rank takes only a machine it could run on: not cordoned, its taints
// tolerated, with the job's kind of card, enough of them.
func TestMachineFits(t *testing.T) {
	j := testJob() // 4 NVIDIA cards a rank
	node := func(alloc corev1.ResourceList, taints ...corev1.Taint) *corev1.Node {
		return &corev1.Node{Spec: corev1.NodeSpec{Taints: taints}, Status: corev1.NodeStatus{Allocatable: alloc}}
	}
	nv8 := corev1.ResourceList{gpures.NVIDIA: resource.MustParse("8")}
	if !machineFits(j, node(nv8)) {
		t.Error("8 NVIDIA cards")
	}
	if machineFits(j, node(corev1.ResourceList{"mthreads.com/gpu": resource.MustParse("8")})) {
		t.Error("another kind of card")
	}
	if machineFits(j, node(corev1.ResourceList{gpures.NVIDIA: resource.MustParse("2")})) {
		t.Error("too few cards")
	}
	cordoned := node(nv8)
	cordoned.Spec.Unschedulable = true
	if machineFits(j, cordoned) {
		t.Error("cordoned")
	}
	if !machineFits(j, node(nv8, corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule})) {
		t.Error("not-ready on a machine that says nothing of its readiness (judged apart)")
	}
	// a machine under pressure keeps Pods off: so does the queue (zjusct's
	// m701, disk pressure: a rank waited there until its job gave up)
	if machineFits(j, node(nv8, corev1.Taint{Key: "node.kubernetes.io/disk-pressure", Effect: corev1.TaintEffectNoSchedule})) {
		t.Error("disk pressure")
	}
	notReady := node(nv8, corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule})
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	if machineFits(j, notReady) {
		t.Error("not ready, and it says so")
	}
	if !machineFits(j, node(nv8, corev1.Taint{Key: string(gpures.NVIDIA), Effect: corev1.TaintEffectNoSchedule})) {
		t.Error("the cards' own taint")
	}
	taint := corev1.Taint{Key: "dedicated", Value: "infer", Effect: corev1.TaintEffectNoSchedule}
	if machineFits(j, node(nv8, taint)) {
		t.Error("an untolerated taint")
	}
	j.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "infer", Effect: corev1.TaintEffectNoSchedule}}
	if !machineFits(j, node(nv8, taint)) {
		t.Error("a tolerated taint")
	}
}

// A rank is the queue's only with its job's owner and the name the queue
// gives it.
func TestIsRankOf(t *testing.T) {
	j := testJob()
	r := rankDevPod(j, 1, "g1", "devpods", "")
	if !isRankOf(r, j) || attemptOf(r) != 1 {
		t.Fatalf("the queue's own: %v attempt %d", isRankOf(r, j), attemptOf(r))
	}
	other := r.DeepCopy()
	other.Spec.Owner, other.Name = "mallory", "mallory-abc12-1"
	if isRankOf(other, j) || !rankShaped(other) {
		t.Error("another's DevPod naming the job")
	}
	renamed := r.DeepCopy()
	renamed.Name = "alice-x"
	if isRankOf(renamed, j) || rankShaped(renamed) {
		t.Error("named otherwise")
	}
}

// A rank goes only where its source's own placement lets it: its
// nodeSelector and required node affinity (the machine they name aside:
// each rank is given its own).
func TestMachineFitsPlacement(t *testing.T) {
	node := func(arch string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n-" + arch, Labels: map[string]string{"kubernetes.io/arch": arch, hostnameLabel: "n-" + arch}},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{gpures.NVIDIA: resource.MustParse("8")}}}
	}
	j := testJob()
	j.Spec.Template.Spec.Affinity = nil
	j.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64", hostnameLabel: "somewhere-else"}
	if machineFits(j, node("amd64")) || !machineFits(j, node("arm64")) {
		t.Error("nodeSelector: arm64 only, the hostname aside")
	}
	j.Spec.Template.Spec.NodeSelector = nil
	j.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"amd64"}},
			{Key: hostnameLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"g1"}},
		}}}}}}
	if machineFits(j, node("amd64")) || !machineFits(j, node("arm64")) {
		t.Error("required affinity: not amd64, the hostname aside")
	}
	j.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(
		j.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
		corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}}}})
	if !machineFits(j, node("amd64")) {
		t.Error("terms are or'ed")
	}
}

// The shell's PATH a job took along goes in front of the ranks' own (their
// login shell sets PATH anew).
func TestRankDevPodSubmitPath(t *testing.T) {
	j := testJob()
	j.Spec.Env = append(j.Spec.Env, corev1.EnvVar{Name: "DEVPOD_SUBMIT_PATH", Value: "/opt/conda/bin:/usr/bin"})
	c := rankDevPod(j, 0, "g1", "devpods", "").Spec.Pod.Spec.Containers[0]
	if len(c.Command) < 3 || !strings.HasPrefix(c.Command[2], `export PATH="$DEVPOD_SUBMIT_PATH:$PATH"`+"\n") || !strings.HasSuffix(c.Command[2], "python t.py") {
		t.Fatalf("command %q", c.Command)
	}
}
