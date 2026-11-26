// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"hash/fnv"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

// A job runs as one DevPod per rank (stage 2's job ranks): its owner's,
// pinned to the machine the scheduler chose, running the job's command
// once in a login bash (the supervisor's bash: there in every image),
// with what a rank needs to find the others.

// jobBash: the shell a rank's command runs in.
const jobBash = "/opt/devpod/bin/bash"

// jobPortBlock: how many ports a job has on each machine (MASTER_PORT is
// the first): jobs of the host's network on one machine do not clash.
const jobPortBlock = 16

func itoa(n int32) string { return strconv.Itoa(int(n)) }

// rankName: rank's DevPod's name — <owner>-<job>-<rank>, as the console
// names what it makes (an SSH login <owner>+<job>-<rank> reaches it).
func rankName(owner, id string, rank int32) string {
	return owner + "-" + id + "-" + itoa(rank)
}

// jobPorts: the block of ports a job's ranks may listen on.
func jobPorts(id string) (lo, hi int32) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	lo = 20000 + jobPortBlock*int32(h.Sum32()%2000)
	return lo, lo + jobPortBlock - 1
}

// masterAddr: where the ranks find rank 0 — on the host's network its
// machine's address; else its DevPod's (headless) Service.
func masterAddr(_ *devpodv1alpha1.Job, rank0, ns string, hostNet bool, rank0IP string) string {
	if hostNet {
		return rank0IP
	}
	return rank0 + "." + ns + ".svc"
}

// jobAsk: what each rank takes of a machine.
func jobAsk(job *devpodv1alpha1.Job) amount {
	return amount{GPU: int64(job.Spec.GPUsPerNode), CPU: job.Spec.CPUsPerNode.MilliValue()}
}

// rankDevPod: rank of job, on node, in ns, finding rank 0 at master.
func rankDevPod(job *devpodv1alpha1.Job, rank int32, node, ns, master string) *devpodv1alpha1.DevPod {
	pod := job.Spec.Template.DeepCopy()
	spec := &pod.Spec
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	spec.NodeSelector[hostnameLabel] = node
	dropHostnameAffinity(spec)

	c := &spec.Containers[0]
	c.Command = append([]string{jobBash, "-lc", job.Spec.Command, jobArg0(job)}, job.Spec.Args...)
	c.Args = nil
	if job.Spec.Workdir != "" {
		c.WorkingDir = job.Spec.Workdir
	}
	lo, hi := jobPorts(job.Name)
	env := append([]corev1.EnvVar{}, c.Env...)
	env = append(env, job.Spec.Env...)
	for _, kv := range [][2]string{
		{"RANK", itoa(rank)}, {"NODE_RANK", itoa(rank)}, {"LOCAL_RANK", "0"}, {"WORLD_SIZE", itoa(job.Spec.Nodes)},
		{"MASTER_ADDR", master}, {"MASTER_PORT", itoa(lo)}, {"DEVPOD_JOB", job.Name}, {"DEVPOD_JOB_PORTS", itoa(lo) + "-" + itoa(hi)},
	} {
		env = append(env, corev1.EnvVar{Name: kv[0], Value: kv[1]})
	}
	c.Env = env
	setJobResources(c, job)

	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: rankName(job.Spec.Owner, job.Name, rank), Namespace: ns,
			Labels: map[string]string{"devpod.io/job": job.Name}},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   job.Spec.Owner,
			Running: true,
			Shell:   job.Spec.Shell,
			Job:     &devpodv1alpha1.DevPodJob{Name: job.Name, Rank: rank},
			Pod:     pod,
		},
	}
}

// jobArg0: the command's $0 — its title, else its id.
func jobArg0(job *devpodv1alpha1.Job) string {
	if job.Spec.Title != "" {
		return job.Spec.Title
	}
	return job.Name
}

// setJobResources gives c the job's cards (of the type its source had,
// NVIDIA's by default) and cores, limits and requests alike.
func setJobResources(c *corev1.Container, job *devpodv1alpha1.Job) {
	if c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	gpu := corev1.ResourceName(gpures.NVIDIA)
	for _, t := range gpures.Types() {
		if _, ok := c.Resources.Limits[t.Resource]; ok {
			gpu = t.Resource
			break
		}
	}
	for _, t := range gpures.Types() {
		delete(c.Resources.Limits, t.Resource)
		delete(c.Resources.Requests, t.Resource)
	}
	if job.Spec.GPUsPerNode > 0 {
		n := *resource.NewQuantity(int64(job.Spec.GPUsPerNode), resource.DecimalSI)
		c.Resources.Limits[gpu], c.Resources.Requests[gpu] = n, n
	}
	if !job.Spec.CPUsPerNode.IsZero() {
		c.Resources.Limits[corev1.ResourceCPU] = job.Spec.CPUsPerNode
		c.Resources.Requests[corev1.ResourceCPU] = job.Spec.CPUsPerNode
	}
}

// dropHostnameAffinity takes the machine requirements by name out of a
// rank's required node affinity (an allow-list it was copied with): the
// scheduler pins it.
func dropHostnameAffinity(spec *corev1.PodSpec) {
	a := spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return
	}
	sel := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	var kept []corev1.NodeSelectorTerm
	for _, t := range sel.NodeSelectorTerms {
		t.MatchExpressions = slices.DeleteFunc(slices.Clone(t.MatchExpressions), func(r corev1.NodeSelectorRequirement) bool { return r.Key == hostnameLabel })
		if len(t.MatchExpressions) > 0 || len(t.MatchFields) > 0 {
			kept = append(kept, t)
		}
	}
	sel.NodeSelectorTerms = kept
	if len(kept) == 0 {
		a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
	}
	if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil && a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
		a.NodeAffinity = nil
	}
	if a.NodeAffinity == nil && a.PodAffinity == nil && a.PodAntiAffinity == nil {
		spec.Affinity = nil
	}
}
