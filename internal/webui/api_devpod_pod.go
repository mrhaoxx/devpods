// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mrhaoxx/devpod/internal/render"
)

// The Pod behind a DevPod, as `kubectl describe pod` would tell it: where
// it runs, what state each container is in and why, what is mounted. For
// the question "why does my DevPod not start". Environment variables are
// left out: they may carry what should not be shown.

// PodDetail is GET /api/devpods/{name}/pod.
type PodDetail struct {
	Name           string            `json:"name"`
	Phase          string            `json:"phase"`
	Reason         string            `json:"reason,omitempty"`
	Message        string            `json:"message,omitempty"`
	Ready          string            `json:"ready"` // "1/1"
	Restarts       int32             `json:"restarts"`
	QoS            string            `json:"qos,omitempty"`
	Node           string            `json:"node,omitempty"`
	HostIP         string            `json:"hostIP,omitempty"`
	PodIP          string            `json:"podIP,omitempty"`
	CreatedAt      *metav1.Time      `json:"createdAt,omitempty"`
	StartedAt      *metav1.Time      `json:"startedAt,omitempty"`
	DeletingSince  *metav1.Time      `json:"deletingSince,omitempty"`
	RestartPolicy  string            `json:"restartPolicy,omitempty"`
	ServiceAccount string            `json:"serviceAccount,omitempty"`
	GracePeriod    *int64            `json:"gracePeriodSeconds,omitempty"`
	HostNetwork    bool              `json:"hostNetwork,omitempty"`
	Conditions     []PodCondition    `json:"conditions"`
	InitContainers []PodContainer    `json:"initContainers"`
	Containers     []PodContainer    `json:"containers"`
	Volumes        []PodVolume       `json:"volumes"`
	Labels         map[string]string `json:"labels"`
	NodeSelector   map[string]string `json:"nodeSelector"`
	Tolerations    []string          `json:"tolerations"`
}

// PodCondition is one of the Pod's conditions.
type PodCondition struct {
	Type    string       `json:"type"`
	Status  string       `json:"status"`
	Reason  string       `json:"reason,omitempty"`
	Message string       `json:"message,omitempty"`
	Since   *metav1.Time `json:"since,omitempty"`
}

// PodContainer is a container with its state in words.
type PodContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// State: "running since …", "waiting: ImagePullBackOff — …",
	// "terminated: exit 1 (Error)"; "" before the kubelet reported one.
	State string `json:"state,omitempty"`
	// Tone of the state: ok, wait or fail.
	Tone      string            `json:"tone,omitempty"`
	LastState string            `json:"lastState,omitempty"`
	Ready     bool              `json:"ready"`
	Restarts  int32             `json:"restarts"`
	Requests  map[string]string `json:"requests,omitempty"`
	Limits    map[string]string `json:"limits,omitempty"`
	Mounts    []string          `json:"mounts,omitempty"` // "/models ← model (ro)"
}

// PodVolume is a volume and what backs it.
type PodVolume struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

func quantities(l corev1.ResourceList) map[string]string {
	if len(l) == 0 {
		return nil
	}
	out := make(map[string]string, len(l))
	for k, v := range l {
		out[string(k)] = v.String()
	}
	return out
}

// containerState puts a state in words, with its tone.
func containerState(s corev1.ContainerState) (string, string) {
	switch {
	case s.Running != nil:
		return "running since " + s.Running.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), "ok"
	case s.Waiting != nil:
		text := "waiting: " + s.Waiting.Reason
		if s.Waiting.Message != "" {
			text += " — " + s.Waiting.Message
		}
		tone := "wait"
		// the kubelet's own words for "this will not get better by waiting"
		for _, bad := range []string{"BackOff", "Err", "Invalid", "Failed", "Cannot"} {
			if strings.Contains(s.Waiting.Reason, bad) {
				tone = "fail"
			}
		}
		return text, tone
	case s.Terminated != nil:
		t := s.Terminated
		text := fmt.Sprintf("terminated: exit %d (%s)", t.ExitCode, t.Reason)
		if t.Signal != 0 {
			text = fmt.Sprintf("terminated: signal %d (%s)", t.Signal, t.Reason)
		}
		if t.Message != "" {
			text += " — " + t.Message
		}
		if t.ExitCode == 0 && t.Signal == 0 {
			return text, "ok"
		}
		return text, "fail"
	}
	return "", ""
}

func podContainers(specs []corev1.Container, statuses []corev1.ContainerStatus) []PodContainer {
	out := make([]PodContainer, 0, len(specs))
	for _, c := range specs {
		pc := PodContainer{Name: c.Name, Image: c.Image, Requests: quantities(c.Resources.Requests), Limits: quantities(c.Resources.Limits)}
		for _, m := range c.VolumeMounts {
			line := m.MountPath + " ← " + m.Name
			if m.SubPath != "" {
				line += "/" + m.SubPath
			}
			if m.ReadOnly {
				line += " (ro)"
			}
			pc.Mounts = append(pc.Mounts, line)
		}
		for _, st := range statuses {
			if st.Name != c.Name {
				continue
			}
			pc.State, pc.Tone = containerState(st.State)
			pc.LastState, _ = containerState(st.LastTerminationState)
			pc.Ready, pc.Restarts = st.Ready, st.RestartCount
		}
		out = append(out, pc)
	}
	return out
}

// volumeSource names what backs a volume.
func volumeSource(v corev1.Volume) (string, string) {
	s := v.VolumeSource
	switch {
	case s.HostPath != nil:
		return "hostPath", s.HostPath.Path
	case s.EmptyDir != nil:
		var parts []string
		if s.EmptyDir.Medium != "" {
			parts = append(parts, "medium "+string(s.EmptyDir.Medium))
		}
		if s.EmptyDir.SizeLimit != nil {
			parts = append(parts, "limit "+s.EmptyDir.SizeLimit.String())
		}
		return "emptyDir", strings.Join(parts, ", ")
	case s.PersistentVolumeClaim != nil:
		return "persistentVolumeClaim", s.PersistentVolumeClaim.ClaimName
	case s.Secret != nil:
		return "secret", s.Secret.SecretName
	case s.ConfigMap != nil:
		return "configMap", s.ConfigMap.Name
	case s.NFS != nil:
		return "nfs", s.NFS.Server + ":" + s.NFS.Path
	case s.Projected != nil:
		var kinds []string
		for _, src := range s.Projected.Sources {
			switch {
			case src.ServiceAccountToken != nil:
				kinds = append(kinds, "serviceAccountToken")
			case src.ConfigMap != nil:
				kinds = append(kinds, "configMap "+src.ConfigMap.Name)
			case src.Secret != nil:
				kinds = append(kinds, "secret "+src.Secret.Name)
			case src.DownwardAPI != nil:
				kinds = append(kinds, "downwardAPI")
			}
		}
		return "projected", strings.Join(kinds, ", ")
	case s.DownwardAPI != nil:
		return "downwardAPI", ""
	case s.CSI != nil:
		return "csi", s.CSI.Driver
	case s.Ephemeral != nil:
		return "ephemeral", ""
	}
	return "other", ""
}

// PodDetailFor describes pod.
func PodDetailFor(pod *corev1.Pod) *PodDetail {
	d := &PodDetail{
		Name: pod.Name, Phase: string(pod.Status.Phase), Reason: pod.Status.Reason, Message: pod.Status.Message,
		QoS: string(pod.Status.QOSClass), Node: pod.Spec.NodeName, HostIP: pod.Status.HostIP, PodIP: pod.Status.PodIP,
		StartedAt: pod.Status.StartTime, DeletingSince: pod.DeletionTimestamp,
		RestartPolicy: string(pod.Spec.RestartPolicy), ServiceAccount: pod.Spec.ServiceAccountName,
		GracePeriod: pod.Spec.TerminationGracePeriodSeconds, HostNetwork: pod.Spec.HostNetwork,
		Conditions:     []PodCondition{},
		InitContainers: podContainers(pod.Spec.InitContainers, pod.Status.InitContainerStatuses),
		Containers:     podContainers(pod.Spec.Containers, pod.Status.ContainerStatuses),
		Volumes:        []PodVolume{},
		Labels:         pod.Labels, NodeSelector: pod.Spec.NodeSelector,
		Tolerations: []string{},
	}
	if !pod.CreationTimestamp.IsZero() {
		d.CreatedAt = &pod.CreationTimestamp
	}
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if d.NodeSelector == nil {
		d.NodeSelector = map[string]string{}
	}
	ready := 0
	for _, c := range d.Containers {
		if c.Ready {
			ready++
		}
		d.Restarts += c.Restarts
	}
	d.Ready = fmt.Sprintf("%d/%d", ready, len(d.Containers))
	for _, c := range pod.Status.Conditions {
		pc := PodCondition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message}
		if !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime
			pc.Since = &t
		}
		d.Conditions = append(d.Conditions, pc)
	}
	// the order a Pod goes through them
	order := map[string]int{"PodScheduled": 0, "PodReadyToStartContainers": 1, "Initialized": 2, "ContainersReady": 3, "Ready": 4}
	sort.SliceStable(d.Conditions, func(i, j int) bool {
		a, okA := order[d.Conditions[i].Type]
		b, okB := order[d.Conditions[j].Type]
		if okA != okB {
			return okA
		}
		return a < b
	})
	for _, v := range pod.Spec.Volumes {
		kind, detail := volumeSource(v)
		d.Volumes = append(d.Volumes, PodVolume{Name: v.Name, Kind: kind, Detail: detail})
	}
	for _, t := range pod.Spec.Tolerations {
		line := t.Key
		if line == "" {
			line = "*"
		}
		if t.Operator == corev1.TolerationOpEqual || (t.Operator == "" && t.Value != "") {
			line += "=" + t.Value
		}
		if t.Effect != "" {
			line += ":" + string(t.Effect)
		}
		if t.TolerationSeconds != nil {
			line += fmt.Sprintf(" for %ds", *t.TolerationSeconds)
		}
		d.Tolerations = append(d.Tolerations, line)
	}
	return d
}

// handleDevPodPod serves GET /api/devpods/{name}/pod: {"pod": …}, or
// {"pod": null} when the DevPod has no Pod (it is stopped).
func (s *Server) handleDevPodPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var pod corev1.Pod
	// the API server, not the cache: this is looked at while things change
	err := s.k(r).Reader.Get(r.Context(), types.NamespacedName{Namespace: s.NS, Name: render.PodName(dp)}, &pod)
	switch {
	case apierrors.IsNotFound(err):
		s.writeJSON(w, http.StatusOK, map[string]any{"pod": nil})
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	default:
		s.writeJSON(w, http.StatusOK, map[string]any{"pod": PodDetailFor(&pod)})
	}
}
