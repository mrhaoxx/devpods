// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/configvars"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

// The queue's API (design 2026-11-26): submit a job, list the queue, see
// one, cancel it, submit it again; the caller's limits; the priority caps
// (admins). Jobs live in the console's own cluster; each names the
// cluster it runs in.

// JobRequest is POST /api/jobs.
type JobRequest struct {
	Title   string   `json:"title"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Script: a script to run instead of Command (its title is its $0).
	Script           string                   `json:"script"`
	Workdir          string                   `json:"workdir"`
	From             devpodv1alpha1.JobSource `json:"from"`
	Nodes            int32                    `json:"nodes"`
	GPUs             int32                    `json:"gpus"`
	CPUs             *float64                 `json:"cpus"`
	TimeLimitSeconds int64                    `json:"timeLimitSeconds"`
	Priority         int32                    `json:"priority"`
	Preemptible      bool                     `json:"preemptible"`
	Requeue          *bool                    `json:"requeue"`
	Privileges       *JobPrivileges           `json:"privileges"`
	Env              map[string]string        `json:"env"`
	DryRun           bool                     `json:"dryRun"`
}

// JobPrivileges: what of the host a job's ranks ask, over their source's.
type JobPrivileges struct {
	HostNetwork *bool    `json:"hostNetwork"`
	Privileged  *bool    `json:"privileged"`
	HostPID     *bool    `json:"hostPID"`
	HostIPC     *bool    `json:"hostIPC"`
	CapAdd      []string `json:"capAdd"`
}

// jobView is a job as the API shows it; what it runs only to its owner
// and admins.
type jobView struct {
	Cluster          string                    `json:"cluster,omitempty"`
	ID               string                    `json:"id"`
	Title            string                    `json:"title,omitempty"`
	Owner            string                    `json:"owner"`
	Phase            devpodv1alpha1.JobPhase   `json:"phase"`
	Reason           string                    `json:"reason,omitempty"`
	Detail           map[string]string         `json:"detail,omitempty"`
	Position         int32                     `json:"position,omitempty"`
	Nodes            int32                     `json:"nodes"`
	GPUs             int32                     `json:"gpus"`
	CPUs             float64                   `json:"cpus"`
	TimeLimitSeconds int64                     `json:"timeLimitSeconds"`
	Priority         int32                     `json:"priority"`
	Preemptible      bool                      `json:"preemptible,omitempty"`
	Requeue          bool                      `json:"requeue"`
	Mine             bool                      `json:"mine"`
	CanManage        bool                      `json:"canManage"`
	Submitted        time.Time                 `json:"submitted"`
	ExpectedStart    *time.Time                `json:"expectedStart,omitempty"`
	StartedAt        *time.Time                `json:"startedAt,omitempty"`
	FinishedAt       *time.Time                `json:"finishedAt,omitempty"`
	Attempts         int32                     `json:"attempts,omitempty"`
	Preemptions      int32                     `json:"preemptions,omitempty"`
	Ranks            []devpodv1alpha1.JobRank  `json:"ranks,omitempty"`
	Command          string                    `json:"command,omitempty"`
	Args             []string                  `json:"args,omitempty"`
	Workdir          string                    `json:"workdir,omitempty"`
	Source           *devpodv1alpha1.JobSource `json:"source,omitempty"`
}

func timeOf(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func viewOfJob(j *devpodv1alpha1.Job, me string, admin bool) jobView {
	v := jobView{
		ID: j.Name, Title: j.Spec.Title, Owner: j.Spec.Owner, Phase: j.Status.Phase, Reason: j.Status.Reason, Detail: j.Status.Detail,
		Position: j.Status.Position, Nodes: j.Spec.Nodes, GPUs: j.Spec.GPUsPerNode, CPUs: float64(j.Spec.CPUsPerNode.MilliValue()) / 1000,
		TimeLimitSeconds: int64(j.Spec.TimeLimit.Duration / time.Second), Priority: j.Spec.Priority, Preemptible: j.Spec.Preemptible,
		Requeue: j.Spec.Requeues(), Mine: j.Spec.Owner == me, Submitted: j.CreationTimestamp.Time.UTC(), ExpectedStart: timeOf(j.Status.ExpectedStart),
		StartedAt: timeOf(j.Status.StartedAt), FinishedAt: timeOf(j.Status.FinishedAt), Attempts: j.Status.Attempts,
		Preemptions: j.Status.Preemptions, Ranks: j.Status.Ranks,
	}
	if v.Phase == "" {
		v.Phase = devpodv1alpha1.JobQueued
	}
	v.CanManage = v.Mine || admin
	if v.CanManage {
		v.Command, v.Args, v.Workdir = j.Spec.Command, j.Spec.Args, j.Spec.Workdir
		src := j.Spec.Source
		v.Source = &src
	} else {
		// others see where it runs, not what it keeps there
		for i := range v.Ranks {
			v.Ranks[i].Log = ""
		}
	}
	return v
}

// jobEnded: the job has ended, one way or another.
func jobEnded(p devpodv1alpha1.JobPhase) bool {
	switch p {
	case devpodv1alpha1.JobSucceeded, devpodv1alpha1.JobFailed, devpodv1alpha1.JobTimedOut, devpodv1alpha1.JobCancelled:
		return true
	}
	return false
}

// queueOK: the queue is on, and ctx's cluster takes jobs.
func (s *Server) queueOK(w http.ResponseWriter, ctx context.Context) bool {
	if s.Queue == nil {
		s.writeErr(w, http.StatusNotFound, "QUEUE_OFF", "the queue is not on in this console", nil)
		return false
	}
	if !s.takesJobs(ctx, s.resCluster(ctx)) {
		s.writeErr(w, http.StatusNotFound, "QUEUE_OFF", "this cluster takes no jobs (its controller does not run them)", nil)
		return false
	}
	return true
}

// takesJobs: cluster runs job ranks — its agent says "jobs"; in a console
// of one cluster, always.
func (s *Server) takesJobs(ctx context.Context, cluster string) bool {
	jobs, _ := s.controllerSays(ctx, cluster)
	return jobs
}

// controllerSays: what cluster's controller says it can do — run job
// ranks (and the cluster was given jobs) — and where owners' homes are
// shared ("" not). A console of several clusters hears it from their
// agents; a console of one reads its controller's word itself.
func (s *Server) controllerSays(ctx context.Context, cluster string) (jobs bool, sharedHomes string) {
	if s.Clusters != nil {
		in, ok := s.Clusters.Info(cluster)
		return ok && slices.Contains(in.Capabilities, agent.CapJobs), in.SharedHomes
	}
	caps, homes := agent.ControllerCaps([]string{agent.CapJobs}, "", s.localControllerWord(ctx))
	return slices.Contains(caps, agent.CapJobs), homes
}

// localControllerWord: this cluster's controller's devpod-controller
// ConfigMap (nil: none), read at most every 10 s.
func (s *Server) localControllerWord(ctx context.Context) *corev1.ConfigMap {
	s.ctrlWordMu.Lock()
	defer s.ctrlWordMu.Unlock()
	if time.Since(s.ctrlWordAt) < 10*time.Second {
		return s.ctrlWord
	}
	var cm corev1.ConfigMap
	s.ctrlWord, s.ctrlWordAt = nil, time.Now()
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: devpodv1alpha1.ControllerFeaturesConfigMap}, &cm); err == nil {
		s.ctrlWord = &cm
	} else if !apierrors.IsNotFound(err) {
		slog.Warn("controller_word", "err", err)
	}
	return s.ctrlWord
}

// sharedDir: dir is on storage every machine has — under owner's shared
// home (sharedHomes/<owner>), or under a mount of the ranks' container
// of a volume that is not the Pod's own (a host directory, NFS, a
// claim, CephFS, GlusterFS, CSI).
func sharedDir(dir, owner, sharedHomes string, spec *corev1.PodSpec) bool {
	under := func(root string) bool {
		root = strings.TrimRight(root, "/")
		return root != "" && (dir == root || strings.HasPrefix(dir, root+"/"))
	}
	if sharedHomes != "" && under(path.Join(sharedHomes, owner)) {
		return true
	}
	if len(spec.Containers) == 0 {
		return false
	}
	vols := map[string]corev1.Volume{}
	for _, v := range spec.Volumes {
		vols[v.Name] = v
	}
	for _, m := range spec.Containers[0].VolumeMounts {
		v, ok := vols[m.Name]
		if !ok {
			continue
		}
		if (v.HostPath != nil || v.NFS != nil || v.PersistentVolumeClaim != nil || v.CephFS != nil || v.Glusterfs != nil || v.CSI != nil) && under(m.MountPath) {
			return true
		}
	}
	return false
}

func validTitle(t string) (string, error) {
	t = strings.TrimSpace(t)
	if utf8.RuneCountInString(t) > 63 {
		return "", fmt.Errorf("the title is longer than 63 characters")
	}
	if strings.IndexFunc(t, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("the title is one line, without control characters")
	}
	return t, nil
}

// resolveTemplate: the Pod a job's ranks are made from — a DevPod of the
// caller's (any, for an admin), or a config rendered with its variables
// — and the DevPod shell that goes with it. Machine pins and the
// console's allow-list go (the scheduler places ranks); a source with
// its own disk is refused (ranks run on the shared home).
func (s *Server) resolveTemplate(ctx context.Context, sess Session, from devpodv1alpha1.JobSource) (devpodv1alpha1.PodWorkloadSpec, string, *serverErr) {
	var src *devpodv1alpha1.DevPod
	switch {
	case from.DevPod != "" && from.Config != "":
		return devpodv1alpha1.PodWorkloadSpec{}, "", badRequest("a job comes from a DevPod or from a config, not both")
	case from.DevPod != "":
		var dp devpodv1alpha1.DevPod
		err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: from.DevPod}, &dp)
		if apierrors.IsNotFound(err) && !strings.HasPrefix(from.DevPod, sess.User+"-") {
			err = s.kc(ctx).Client.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: sess.User + "-" + from.DevPod}, &dp)
		}
		if err != nil {
			return devpodv1alpha1.PodWorkloadSpec{}, "", &serverErr{http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("no DevPod %q", from.DevPod), nil}
		}
		if dp.Spec.Owner != sess.User && !sess.Admin {
			return devpodv1alpha1.PodWorkloadSpec{}, "", &serverErr{http.StatusForbidden, "FORBIDDEN", "a job is made from a DevPod of your own", nil}
		}
		src = &dp
	case from.Config != "":
		c, apiErr := s.visibleConfig(ctx, sess, from.Config)
		if apiErr != nil {
			return devpodv1alpha1.PodWorkloadSpec{}, "", apiErr
		}
		vars := map[string]string{}
		for k, v := range from.Vars {
			vars[k] = v
		}
		for k, v := range map[string]string{configvars.Owner: sess.User, configvars.Name: sess.User + "-job", configvars.Index: "0", configvars.Count: "1", configvars.Leader: "", configvars.Node: "job"} {
			if configvars.Uses(c.Spec.Content, k) {
				vars[k] = v
			}
		}
		text, err := configvars.Render(c.Spec.Content, vars)
		if err != nil {
			return devpodv1alpha1.PodWorkloadSpec{}, "", &serverErr{http.StatusBadRequest, "BAD_VARIABLES", err.Error(), nil}
		}
		dp, apiErr := s.buildDevPod(sess, CreateRequest{YAML: text})
		if apiErr != nil {
			return devpodv1alpha1.PodWorkloadSpec{}, "", apiErr
		}
		src = dp
	default:
		return devpodv1alpha1.PodWorkloadSpec{}, "", badRequest("say where the ranks come from: a DevPod (from.devpod) or a config (from.config)")
	}
	if src.Spec.Pod == nil {
		return devpodv1alpha1.PodWorkloadSpec{}, "", badRequest("a job's ranks are Pods: %s is a VM", src.Name)
	}
	// the ranks are the console's writing of what the submitter wrote:
	// the same guards as a DevPod of theirs (no Secrets, no service
	// account, no admin's annotations)
	if apiErr := s.checkWriting(sess, src); apiErr != nil {
		return devpodv1alpha1.PodWorkloadSpec{}, "", apiErr
	}
	if src.Spec.Persistence != nil {
		return devpodv1alpha1.PodWorkloadSpec{}, "", badRequest("a job runs on the shared home: %s has a disk of its own (spec.persistence)", src.Name)
	}
	tmp := src.DeepCopy()
	access.SetAllowed(tmp, nil) // the console's allow-list
	delete(tmp.Spec.Pod.Spec.NodeSelector, hostnameLabel)
	tmp.Spec.Pod.Spec.NodeName = ""
	return *tmp.Spec.Pod, src.Spec.Shell, nil
}

// applyPrivileges asks of the host what p says, over the template's.
func applyPrivileges(spec *corev1.PodSpec, p *JobPrivileges) {
	if p == nil {
		return
	}
	if p.HostNetwork != nil {
		spec.HostNetwork = *p.HostNetwork
	}
	if p.HostPID != nil {
		spec.HostPID = *p.HostPID
	}
	if p.HostIPC != nil {
		spec.HostIPC = *p.HostIPC
	}
	c := &spec.Containers[0]
	if p.Privileged != nil || len(p.CapAdd) > 0 {
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
	}
	if p.Privileged != nil {
		c.SecurityContext.Privileged = p.Privileged
	}
	if len(p.CapAdd) > 0 {
		if c.SecurityContext.Capabilities == nil {
			c.SecurityContext.Capabilities = &corev1.Capabilities{}
		}
		for _, cap := range p.CapAdd {
			c.SecurityContext.Capabilities.Add = append(c.SecurityContext.Capabilities.Add, corev1.Capability(cap))
		}
	}
}

// jobMachines: the reservable machines of ctx's cluster (the queue's)
// the access rules let owner run a rank with needs on; and how many of
// them the rules let owner use at all.
func (s *Server) jobMachines(ctx context.Context, owner string, needs access.Needs) ([]corev1.Node, int, error) {
	var nodes corev1.NodeList
	if err := s.kc(ctx).Client.List(ctx, &nodes); err != nil {
		return nil, 0, err
	}
	cluster := s.resCluster(ctx)
	oa, err := s.ownerAccess(ctx, owner) // read once for every machine
	if err != nil {
		return nil, 0, err
	}
	var ok []corev1.Node
	usable := 0
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" {
			continue
		}
		d, admin := oa.decide(cluster, access.Node{Name: n.Name, Labels: n.Labels})
		if !admin && !d.Allow {
			continue
		}
		usable++
		if admin || d.Admits(needs) == nil {
			ok = append(ok, *n)
		}
	}
	return ok, usable, nil
}

// newJobID: a free job id — five letters and digits, a letter first.
func (s *Server) newJobID(ctx context.Context) (string, error) {
	for range 20 {
		id := randSuffix(5)
		if id[0] < 'a' || id[0] > 'z' {
			continue
		}
		var j devpodv1alpha1.Job
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: id}, &j); apierrors.IsNotFound(err) {
			return id, nil
		}
	}
	return "", fmt.Errorf("no free job id")
}

// handleSubmitJob serves POST /api/jobs (the request's cluster).
func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !s.queueOK(w, ctx) {
		return
	}
	var req JobRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	job, apiErr := s.buildJob(ctx, sess, req)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if req.DryRun {
		s.writeJSON(w, http.StatusOK, s.estimate(ctx, job))
		return
	}
	if apiErr := s.createJob(ctx, job); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	s.recordJob(ctx, eventFrom(r, sess), job, "submit", jobSummary(job))
	v := viewOfJob(job, sess.User, sess.Admin)
	v.Cluster = s.clusterLabel(s.kc(ctx))
	s.writeJSON(w, http.StatusCreated, v)
}

// buildJob checks a request and makes the job it asks for (not yet kept).
func (s *Server) buildJob(ctx context.Context, sess Session, req JobRequest) (*devpodv1alpha1.Job, *serverErr) {
	title, err := validTitle(req.Title)
	if err != nil {
		return nil, badRequest("%s", err.Error())
	}
	command := req.Command
	if req.Script != "" {
		if command != "" {
			return nil, badRequest("a command or a script, not both")
		}
		command = req.Script
	}
	switch {
	case strings.TrimSpace(command) == "":
		return nil, badRequest("say what to run (command or script)")
	case len(command) > 65536:
		return nil, badRequest("the command is longer than 64 KiB")
	case req.Nodes < 1 || req.Nodes > 64:
		return nil, badRequest("a job runs on 1 to 64 machines")
	case req.GPUs < 0:
		return nil, badRequest("gpus must be 0 or more")
	case req.CPUs != nil && !(*req.CPUs > 0):
		return nil, badRequest("cpus must be more than 0")
	}
	limit := s.Queue.DefaultTimeLimit
	if req.TimeLimitSeconds < 0 {
		return nil, badRequest("the time limit must be more than 0")
	}
	if req.TimeLimitSeconds > 0 {
		limit = time.Duration(req.TimeLimitSeconds) * time.Second
	}
	cluster := s.resCluster(ctx)
	if max := s.maxTimeLimit(ctx, cluster); limit > max {
		return nil, badRequest("a job runs at most %s here", max)
	}
	_, groups, err := s.accessFor(ctx, sess.User)
	if err != nil {
		return nil, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	priorityCap, unlimited, err := s.priorityCap(ctx, sess.User, groups, cluster)
	if err != nil {
		return nil, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	if !unlimited && req.Priority > priorityCap {
		return nil, &serverErr{http.StatusForbidden, "PRIORITY_TOO_HIGH", fmt.Sprintf("you may give at most priority %d here", priorityCap), map[string]any{"max": priorityCap}}
	}
	tmpl, shell, apiErr := s.resolveTemplate(ctx, sess, req.From)
	if apiErr != nil {
		return nil, apiErr
	}
	applyPrivileges(&tmpl.Spec, req.Privileges)
	owner := sess.User
	// where the ranks run: on storage the machines share, or they (on
	// other machines than the DevPod's) would not find it
	if wd := req.Workdir; wd != "" {
		if !path.IsAbs(wd) {
			return nil, badRequest("the working directory is a full path (it is %q)", wd)
		}
		_, homes := s.controllerSays(ctx, s.resCluster(ctx))
		if !sharedDir(path.Clean(wd), owner, homes, &tmpl.Spec) {
			where := "this cluster's homes are not shared: work on a shared volume of the DevPod (a host directory, NFS, a claim), or give no working directory"
			if homes != "" {
				where = fmt.Sprintf("work under %s (your shared home) or on a shared volume of the DevPod", path.Join(homes, owner))
			}
			return nil, &serverErr{http.StatusBadRequest, "WORKDIR_NOT_SHARED",
				fmt.Sprintf("the job would run in %s, which the machines do not share: its ranks, on other machines, would not find it — %s", wd, where), nil}
		}
	}
	machines, usable, err := s.jobMachines(ctx, owner, access.NeedsOf(&tmpl.Spec))
	if err != nil {
		return nil, &serverErr{http.StatusServiceUnavailable, "CLUSTER_UNAVAILABLE", err.Error(), nil}
	}
	switch {
	case usable == 0:
		return nil, &serverErr{http.StatusForbidden, "ACL_DENIED", "the access rules let you use none of this cluster's reservable machines (the queue's)", nil}
	case len(machines) == 0:
		return nil, &serverErr{http.StatusForbidden, "PRIVILEGE_DENIED", "the access rules let none of this cluster's reservable machines run these ranks (their privileges)", nil}
	}
	// cards: of the kind the ranks ask for (a machine of another kind is
	// none of theirs)
	kind := corev1.ResourceName(gpures.NVIDIA)
	if len(tmpl.Spec.Containers) > 0 {
		kind = gpuKindOf(&tmpl.Spec.Containers[0])
	}
	capFor := func(n *corev1.Node) amount {
		c := capacityOf(n)
		q := n.Status.Allocatable[kind]
		c.GPU = q.Value()
		return c
	}
	// cores: as asked, else each machine's share for the cards, the least
	var cpuMilli int64
	if req.CPUs != nil {
		cpuMilli = int64(math.Round(*req.CPUs * 1000))
	} else {
		if req.GPUs == 0 {
			return nil, badRequest("say how many cores (cpus) each rank takes")
		}
		for _, n := range machines {
			c := capFor(&n)
			if c.GPU < int64(req.GPUs) {
				continue
			}
			share := c.CPU * int64(req.GPUs) / c.GPU / 1000 * 1000
			if cpuMilli == 0 || share < cpuMilli {
				cpuMilli = share
			}
		}
	}
	ask := amount{GPU: int64(req.GPUs), CPU: cpuMilli}
	fit := 0
	for _, n := range machines {
		// one its own placement and taints let it on, with the room
		if ask.fits(capFor(&n)) && ask.CPU > 0 && placementAdmits(&tmpl.Spec, &n) && taintsTolerated(&tmpl.Spec, req.GPUs > 0, &n) {
			fit++
		}
	}
	if fit < int(req.Nodes) {
		return nil, &serverErr{http.StatusConflict, "NO_MACHINE", fmt.Sprintf("only %d machine(s) here could ever hold one rank of %s", fit, ask), map[string]any{"machines": fit}}
	}
	if len(rankName(owner, "xxxxx", req.Nodes-1)) > 22 {
		return nil, badRequest("the user name is too long for job DevPod names (%s-<job>-<rank>: at most 22 characters)", owner)
	}
	// the environment it takes along (a submitting shell's, often): bounded
	size := 0
	for k, v := range req.Env {
		size += len(k) + len(v)
	}
	if len(req.Env) > maxJobEnvVars || size > maxJobEnvBytes {
		return nil, badRequest("the job's environment is %d variables, %d bytes: at most %d variables, %d bytes (submit with --export NONE and give what it needs with --env)",
			len(req.Env), size, maxJobEnvVars, maxJobEnvBytes)
	}
	env := make([]corev1.EnvVar, 0, len(req.Env))
	for k, v := range req.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	sort.Slice(env, func(i, j int) bool { return env[i].Name < env[j].Name })
	job := &devpodv1alpha1.Job{
		Spec: devpodv1alpha1.JobSpec{
			Cluster: cluster, Owner: owner, Title: title, Source: req.From, Template: tmpl, Shell: shell,
			Command: command, Args: req.Args, Workdir: req.Workdir, Env: env, Nodes: req.Nodes, GPUsPerNode: req.GPUs,
			CPUsPerNode: *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI), TimeLimit: metav1.Duration{Duration: limit},
			Priority: req.Priority, Preemptible: req.Preemptible, Requeue: req.Requeue,
		},
	}
	if apiErr := s.jobQuota(ctx, job); apiErr != nil {
		return nil, apiErr
	}
	return job, nil
}

// jobQuota: the job's ranks fit its owner's quota when nothing else of
// theirs runs (more than that, it waits in the queue).
func (s *Server) jobQuota(ctx context.Context, job *devpodv1alpha1.Job) *serverErr {
	if s.ownerIsAdmin(ctx, job.Spec.Owner) {
		return nil
	}
	var u devpodv1alpha1.User
	var up *devpodv1alpha1.User
	if err := s.Client.Get(ctx, types.NamespacedName{Name: job.Spec.Owner}, &u); err == nil {
		up = &u
	}
	cc := s.kc(ctx)
	q := EffectiveQuota(up, cc.Name, s.clusterQuota(ctx, cc))
	var ranks []devpodv1alpha1.DevPod
	for i := int32(0); i < job.Spec.Nodes-1; i++ {
		ranks = append(ranks, *rankDevPod(job, i, "x", s.NS, ""))
	}
	if qErr := CheckQuota(q, ranks, rankDevPod(job, job.Spec.Nodes-1, "x", s.NS, "")); qErr != nil {
		return &serverErr{http.StatusConflict, "QUOTA_EXCEEDED", "the job's ranks together are over your quota: " + qErr.Error(), qErr.Violations}
	}
	return nil
}

// createJob keeps job, Queued.
func (s *Server) createJob(ctx context.Context, job *devpodv1alpha1.Job) *serverErr {
	if apiErr := s.jobCountErr(ctx, job.Spec.Owner, job.Spec.Cluster); apiErr != nil {
		return apiErr
	}
	id, err := s.newJobID(ctx)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	job.Name = id
	if err := s.Client.Create(ctx, job); err != nil {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", rejected("submit the job", err), nil}
	}
	job.Status.Phase = devpodv1alpha1.JobQueued
	if err := s.Client.Status().Update(ctx, job); err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	return nil
}

// jobCountErr: owner may not have another job in cluster — they have as
// many waiting or running there as they may (jobCap; nil: they may). Two
// submitted at once may both pass: the cap is a guard, not an account.
func (s *Server) jobCountErr(ctx context.Context, owner, cluster string) *serverErr {
	if s.Queue == nil {
		return nil
	}
	_, groups, err := s.accessFor(ctx, owner)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	max, err := s.jobCap(ctx, owner, groups, cluster)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	if max <= 0 {
		return nil
	}
	var all devpodv1alpha1.JobList
	if err := s.Reader.List(ctx, &all); err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	n := 0
	for i := range all.Items {
		if j := &all.Items[i]; j.Spec.Owner == owner && j.Spec.Cluster == cluster && !jobEnded(j.Status.Phase) && j.DeletionTimestamp == nil {
			n++
		}
	}
	if n >= max {
		return &serverErr{http.StatusTooManyRequests, "JOB_LIMIT",
			fmt.Sprintf("you have %d jobs waiting or running here, and may have at most %d: wait for one to end, or cancel one", n, max),
			map[string]any{"jobs": n, "max": max}}
	}
	return nil
}

// estimate: where job would stand if submitted now (a dry run) — the
// planner run over its cluster's queue with it in.
func (s *Server) estimate(ctx context.Context, job *devpodv1alpha1.Job) map[string]any {
	out := map[string]any{"cpus": float64(job.Spec.CPUsPerNode.MilliValue()) / 1000}
	var all devpodv1alpha1.JobList
	if err := s.Reader.List(ctx, &all); err != nil {
		return out
	}
	var active, queued []*devpodv1alpha1.Job
	for i := range all.Items {
		o := &all.Items[i]
		switch {
		case o.Spec.Cluster != job.Spec.Cluster || o.DeletionTimestamp != nil || jobEnded(o.Status.Phase):
		case o.Status.Phase == devpodv1alpha1.JobStarting || o.Status.Phase == devpodv1alpha1.JobRunning:
			active = append(active, o)
		default:
			queued = append(queued, o)
		}
	}
	now := s.Queue.now()
	probe := job.DeepCopy()
	probe.Name, probe.CreationTimestamp = "(new)", metav1.NewTime(now)
	entries := make([]qEntry, 0, len(queued)+1)
	for _, j := range append(queued, probe) {
		entries = append(entries, jobEntry(j))
	}
	v, _, err := s.queueViewFor(ctx, s.kc(ctx), active, entries, now)
	if err != nil {
		return out
	}
	plan := planQueue(v)
	for _, st := range plan.Starts {
		if st.Entry.Key == probe.Name {
			out["position"], out["startsNow"] = 0, true
			return out
		}
	}
	if w, ok := plan.Waits[probe.Name]; ok {
		out["position"], out["reason"], out["detail"] = w.Position, w.Reason, w.Detail
		if w.Expected != nil {
			out["expectedStart"] = w.Expected.UTC()
		}
	}
	return out
}

// handleListJobs serves GET /api/jobs[?mine=1][&ended=1]: the queues of
// the clusters the caller may see (ended jobs only when asked).
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	if s.Queue == nil {
		s.writeErr(w, http.StatusNotFound, "QUEUE_OFF", "the queue is not on in this console", nil)
		return
	}
	q := r.URL.Query()
	mine, ended := q.Get("mine") == "1" || q.Get("mine") == "true", q.Get("ended") == "1" || q.Get("ended") == "true"
	var all devpodv1alpha1.JobList
	if err := s.Reader.List(r.Context(), &all); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	shown := s.clusterGate(r.Context(), sess)
	items := []jobView{}
	for _, cc := range s.ks(r) {
		cluster := s.resCluster(s.in(r.Context(), cc))
		for i := range all.Items {
			j := &all.Items[i]
			if j.Spec.Cluster != cluster || j.DeletionTimestamp != nil {
				continue
			}
			v := viewOfJob(j, sess.User, sess.Admin)
			if (mine && !v.Mine) || (!v.CanManage && !shown(cc.Name)) || jobEnded(v.Phase) != ended {
				continue
			}
			v.Cluster = s.clusterLabel(cc)
			items = append(items, v)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Submitted.Before(items[j].Submitted) })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// jobFor loads job id for the caller: their own, an admin's, or one of a
// cluster they may see (manage: their own or an admin's only).
func (s *Server) jobFor(w http.ResponseWriter, r *http.Request, sess Session, manage bool) (*devpodv1alpha1.Job, bool) {
	if s.Queue == nil {
		s.writeErr(w, http.StatusNotFound, "QUEUE_OFF", "the queue is not on in this console", nil)
		return nil, false
	}
	var j devpodv1alpha1.Job
	if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: r.PathValue("id")}, &j); err != nil {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such job", nil)
		return nil, false
	}
	own := j.Spec.Owner == sess.User || sess.Admin
	if manage && !own {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "this is someone else's job", nil)
		return nil, false
	}
	if !own && !s.clusterGate(r.Context(), sess)(j.Spec.Cluster) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such job", nil)
		return nil, false
	}
	return &j, true
}

// handleGetJob serves GET /api/jobs/{id}.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	j, ok := s.jobFor(w, r, sess, false)
	if !ok {
		return
	}
	v := viewOfJob(j, sess.User, sess.Admin)
	v.Cluster = j.Spec.Cluster
	s.writeJSON(w, http.StatusOK, v)
}

// handleCancelJob serves DELETE /api/jobs/{id}: one waiting ends at once;
// one under way is asked to end (the scheduler stops its ranks).
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	j, ok := s.jobFor(w, r, sess, true)
	if !ok {
		return
	}
	if jobEnded(j.Status.Phase) {
		s.writeErr(w, http.StatusConflict, "JOB_ENDED", "this job has ended already", nil)
		return
	}
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur devpodv1alpha1.Job
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: j.Name}, &cur); err != nil {
			return err
		}
		if cur.Status.Phase == devpodv1alpha1.JobQueued || cur.Status.Phase == "" {
			cur.Status.Phase, cur.Status.Reason, cur.Status.FinishedAt = devpodv1alpha1.JobCancelled, "cancelled", &now
			cur.Status.Position, cur.Status.ExpectedStart = 0, nil
			return s.Client.Status().Update(r.Context(), &cur)
		}
		cur.Spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: sess.User, At: now}
		return s.Client.Update(r.Context(), &cur)
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.recordJob(r.Context(), eventFrom(r, sess), j, "cancel", "")
	w.WriteHeader(http.StatusNoContent)
}

// handleResubmitJob serves POST /api/jobs/{id}/resubmit: the same job,
// anew (its owner's cap and the time limits checked again).
func (s *Server) handleResubmitJob(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	j, ok := s.jobFor(w, r, sess, true)
	if !ok {
		return
	}
	ctx := s.inCluster(r.Context(), j.Spec.Cluster)
	if ctx == nil {
		s.writeErr(w, http.StatusServiceUnavailable, "CLUSTER_OFFLINE", "the job's cluster is away", nil)
		return
	}
	if !s.queueOK(w, ctx) {
		return
	}
	_, groups, err := s.accessFor(ctx, j.Spec.Owner)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if c, unlimited, err := s.priorityCap(ctx, j.Spec.Owner, groups, j.Spec.Cluster); err == nil && !unlimited && j.Spec.Priority > c {
		s.writeErr(w, http.StatusForbidden, "PRIORITY_TOO_HIGH", fmt.Sprintf("its owner may give at most priority %d here now", c), map[string]any{"max": c})
		return
	}
	if max := s.maxTimeLimit(ctx, j.Spec.Cluster); j.Spec.TimeLimit.Duration > max {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("a job runs at most %s here now", max), nil)
		return
	}
	again := &devpodv1alpha1.Job{Spec: *j.Spec.DeepCopy()}
	again.Spec.Cancelled = nil
	if apiErr := s.createJob(ctx, again); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	s.recordJob(ctx, eventFrom(r, sess), again, "resubmit", "again of "+j.Name+"; "+jobSummary(again))
	v := viewOfJob(again, sess.User, sess.Admin)
	v.Cluster = again.Spec.Cluster
	s.writeJSON(w, http.StatusCreated, v)
}

// inCluster: ctx scoped to the cluster jobs name ("" the console's own);
// nil when it is away.
func (s *Server) inCluster(ctx context.Context, cluster string) context.Context {
	cc := s.clientsOf(cluster)
	if cc == nil {
		return nil
	}
	return s.in(ctx, cc)
}

// handleJobLimits serves GET /api/jobs/limits: what the caller may ask in
// the request's cluster.
func (s *Server) handleJobLimits(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !s.queueOK(w, ctx) {
		return
	}
	_, groups, err := s.accessFor(ctx, sess.User)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	cluster := s.resCluster(ctx)
	c, unlimited, err := s.priorityCap(ctx, sess.User, groups, cluster)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"maxPriority": c, "unlimitedPriority": unlimited,
		"defaultTimeLimitSeconds": int64(s.Queue.DefaultTimeLimit / time.Second),
		"maxTimeLimitSeconds":     int64(s.maxTimeLimit(ctx, cluster) / time.Second),
	})
}

type priorityRuleView struct {
	Condition   string `json:"condition"`
	MaxPriority int32  `json:"maxPriority"`
	MaxJobs     *int32 `json:"maxJobs"` // null: the rule says nothing of it
}

// handleGetPriorityRules serves GET /api/admin/job-priority-rules.
func (s *Server) handleGetPriorityRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var l devpodv1alpha1.JobPriorityRuleList
	if err := s.Reader.List(r.Context(), &l); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	items := []priorityRuleView{}
	for _, it := range l.Items {
		items = append(items, priorityRuleView{it.Spec.Condition, it.Spec.MaxPriority, it.Spec.MaxJobs})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handlePutPriorityRules serves PUT /api/admin/job-priority-rules {items}:
// the caps become these, in this order (rule-000, rule-001, …).
func (s *Server) handlePutPriorityRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req struct {
		Items []priorityRuleView `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "send {items: [{condition, maxPriority, maxJobs}]}", nil)
		return
	}
	for i, it := range req.Items {
		if _, err := access.Parse(it.Condition); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_CONDITION", fmt.Sprintf("rule %d: %v", i+1, err), map[string]any{"index": i})
			return
		}
		if it.MaxJobs != nil && *it.MaxJobs < 1 {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("rule %d: the most jobs is at least 1 (leave it empty for the default)", i+1), map[string]any{"index": i})
			return
		}
	}
	ctx := r.Context()
	keep := map[string]bool{}
	for i, it := range req.Items {
		name := fmt.Sprintf("rule-%03d", i)
		keep[name] = true
		var cur devpodv1alpha1.JobPriorityRule
		err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &cur)
		switch {
		case apierrors.IsNotFound(err):
			err = s.Client.Create(ctx, &devpodv1alpha1.JobPriorityRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: devpodv1alpha1.JobPriorityRuleSpec{Condition: it.Condition, MaxPriority: it.MaxPriority, MaxJobs: it.MaxJobs}})
		case err == nil:
			cur.Spec = devpodv1alpha1.JobPriorityRuleSpec{Condition: it.Condition, MaxPriority: it.MaxPriority, MaxJobs: it.MaxJobs}
			err = s.Client.Update(ctx, &cur)
		}
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
	}
	var l devpodv1alpha1.JobPriorityRuleList
	if err := s.Reader.List(ctx, &l); err == nil {
		for i := range l.Items {
			if !keep[l.Items[i].Name] {
				_ = s.Client.Delete(ctx, &l.Items[i])
			}
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": req.Items})
}

// maxJobEnvVars / maxJobEnvBytes bound a job's environment.
const (
	maxJobEnvVars  = 512
	maxJobEnvBytes = 64 << 10
)
