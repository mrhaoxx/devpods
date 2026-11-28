// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JobPhase is where a queue job is.
//
// +kubebuilder:validation:Enum=Queued;Starting;Running;Succeeded;Failed;TimedOut;Cancelled
type JobPhase string

const (
	JobQueued    JobPhase = "Queued"
	JobStarting  JobPhase = "Starting"
	JobRunning   JobPhase = "Running"
	JobSucceeded JobPhase = "Succeeded"
	JobFailed    JobPhase = "Failed"
	JobTimedOut  JobPhase = "TimedOut"
	JobCancelled JobPhase = "Cancelled"
)

// JobSource is what a job's ranks were made from — for showing and for
// submitting it again; the ranks themselves are made from the Template.
type JobSource struct {
	// DevPod: a DevPod of the owner's whose spec the ranks copy.
	//
	// +optional
	DevPod string `json:"devpod,omitempty"`
	// Config: a config (DevPodConfig) rendered with Vars.
	//
	// +optional
	Config string `json:"config,omitempty"`
	// +optional
	Vars map[string]string `json:"vars,omitempty"`
}

// JobSpec is one queue job (design 2026-11-26, the job queue): what to
// run, on how many machines with how much each, for how long at most, how
// urgently, and whether it may be stopped for others. The console keeps
// it in its own cluster and runs it in Cluster as run-to-completion
// DevPods (one per rank).
type JobSpec struct {
	// Cluster: where it runs ("" in a console of one cluster).
	//
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`
	// Title: what it is called, as its owner says.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Title string `json:"title,omitempty"`
	// +optional
	Source JobSource `json:"source,omitempty"`
	// Template: the ranks' Pod, resolved at submission from the source.
	Template PodWorkloadSpec `json:"template"`
	// Shell: the ranks' DevPod shell, as the source's.
	//
	// +optional
	Shell string `json:"shell,omitempty"`
	// Command: what each rank runs, in a login bash (bash -lc); Args
	// follow it ($1 …).
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	Command string `json:"command"`
	// +optional
	Args []string `json:"args,omitempty"`
	// Workdir: where it runs (the source's working directory when empty).
	//
	// +optional
	Workdir string `json:"workdir,omitempty"`
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Nodes: how many machines, one rank on each.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Nodes int32 `json:"nodes"`
	// GPUsPerNode and CPUsPerNode: what each rank takes.
	//
	// +kubebuilder:validation:Minimum=0
	GPUsPerNode int32             `json:"gpusPerNode"`
	CPUsPerNode resource.Quantity `json:"cpusPerNode"`
	// TimeLimit: it is ended when it has run this long.
	TimeLimit metav1.Duration `json:"timeLimit"`
	// Priority: higher runs first (ties: submitted first); at most the
	// owner's cap (JobPriorityRule) when it was submitted.
	Priority int32 `json:"priority"`
	// Preemptible: it may be stopped (and queued again) for a job of
	// higher priority or a reservation's start.
	//
	// +optional
	Preemptible bool `json:"preemptible,omitempty"`
	// Requeue: stopped by the system through no doing of its own — for a
	// job of higher priority, a reservation's start, its machine closed —
	// it waits in line again (unset or true), or ends (false).
	//
	// +optional
	Requeue *bool `json:"requeue,omitempty"`
	// Cancelled: asked to end — by whom, when.
	//
	// +optional
	Cancelled *ReservationCancel `json:"cancelled,omitempty"`
	// Attach: how its submitter is attached to its ranks while they run
	// (srun's way): stdin — each rank waits for the attach to start its
	// command, rank 0's stdin the submitter's; tty — rank 0 waits for it
	// and runs on a terminal (srun --pty). Empty: nobody attaches (its
	// output is followed, and kept).
	//
	// +kubebuilder:validation:Enum=stdin;tty
	// +optional
	Attach string `json:"attach,omitempty"`
	// Held: its owner (or an admin) holds it back — while it waits, it is
	// out of the queue's order and holds no machine; released, it waits in
	// its place again (by its submission). Only a waiting job is held.
	//
	// +optional
	Held bool `json:"held,omitempty"`
}

// JobRank is one rank of a job that has been started.
type JobRank struct {
	Rank   int32  `json:"rank"`
	Node   string `json:"node,omitempty"`
	DevPod string `json:"devpod,omitempty"`
	// Phase: its DevPod's.
	//
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Log: where its output was kept (the audit object store).
	//
	// +optional
	Log string `json:"log,omitempty"`
}

// JobStatus is the scheduler's record of a job.
type JobStatus struct {
	// +optional
	Phase JobPhase `json:"phase,omitempty"`
	// Reason: why it waits (capacity, behind, quota, preempting) or how
	// it ended; Detail the numbers and names that go with it.
	//
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Detail map[string]string `json:"detail,omitempty"`
	// Position: its place among the waiting jobs, from 1.
	//
	// +optional
	Position int32 `json:"position,omitempty"`
	// ExpectedStart: when it is expected to start (the queue's head only).
	//
	// +optional
	ExpectedStart *metav1.Time `json:"expectedStart,omitempty"`
	// AttemptAt: when its ranks were last made; StartedAt: when they all ran.
	//
	// +optional
	AttemptAt *metav1.Time `json:"attemptAt,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
	// +optional
	Preemptions int32 `json:"preemptions,omitempty"`
	// GangTimeouts: how many times its ranks did not all start in time.
	// After a few it fails (it is not going to start); after one it no
	// longer has others stopped for it.
	// +optional
	GangTimeouts int32 `json:"gangTimeouts,omitempty"`
	// PortBase: the first of the 16 ports its ranks may listen on
	// (MASTER_PORT), chosen when it starts so that no job running on the
	// same machines has them.
	// +optional
	PortBase int32 `json:"portBase,omitempty"`
	// +optional
	Ranks []JobRank `json:"ranks,omitempty"`
}

// Job is one queue job, kept by the console in its own cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JobSpec   `json:"spec,omitempty"`
	Status JobStatus `json:"status,omitempty"`
}

// JobList is a list of Job.
//
// +kubebuilder:object:root=true
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Job `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Job{}, &JobList{})
}

// Requeues: a job stopped by the system waits in line again (its
// Requeue unset or true).
func (s JobSpec) Requeues() bool { return s.Requeue == nil || *s.Requeue }
