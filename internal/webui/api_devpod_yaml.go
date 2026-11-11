// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"maps"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// A DevPod as YAML, to read and to edit as a whole (the detail page's
// YAML tab): what `kubectl get -o yaml` would show without the parts the
// cluster fills in — name and spec. Saving replaces the spec; the rules
// are those of creating from YAML and of editing: nothing can be done
// here that cannot be done there.

// devpodManifest is the part of a DevPod a person writes.
type devpodManifest struct {
	APIVersion string                    `json:"apiVersion"`
	Kind       string                    `json:"kind"`
	Metadata   devpodManifestMeta        `json:"metadata"`
	Spec       devpodv1alpha1.DevPodSpec `json:"spec"`
}

type devpodManifestMeta struct {
	Name string `json:"name"`
}

// DevPodYAML renders dp's manifest.
func DevPodYAML(dp *devpodv1alpha1.DevPod) (string, error) {
	b, err := yaml.Marshal(devpodManifest{
		APIVersion: devpodv1alpha1.GroupVersion.String(), Kind: "DevPod",
		Metadata: devpodManifestMeta{Name: dp.Name}, Spec: dp.Spec,
	})
	return string(b), err
}

// handleGetDevPodYAML serves GET /api/devpods/{name}/yaml.
func (s *Server) handleGetDevPodYAML(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	text, err := DevPodYAML(dp)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"yaml": text, "running": dp.Spec.Running})
}

// koreAnnotations picks the CPU-binding annotations out of a pod template.
func koreAnnotations(p *devpodv1alpha1.PodWorkloadSpec) map[string]string {
	out := map[string]string{}
	if p == nil {
		return out
	}
	for _, k := range KoreAnnotationKeys(p.Metadata.Annotations) {
		out[k] = p.Metadata.Annotations[k]
	}
	return out
}

// normalizeNodeName turns spec.nodeName into the hostname node selector:
// nodeName would bypass the scheduler and the node choice rule.
func normalizeNodeName(dp *devpodv1alpha1.DevPod) *serverErr {
	if dp.Spec.Pod == nil || dp.Spec.Pod.Spec.NodeName == "" {
		return nil
	}
	name := dp.Spec.Pod.Spec.NodeName
	if sel := dp.Spec.Pod.Spec.NodeSelector[hostnameLabel]; sel != "" && sel != name {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("nodeName %q and the %s node selector %q name different nodes", name, hostnameLabel, sel), nil}
	}
	if dp.Spec.Pod.Spec.NodeSelector == nil {
		dp.Spec.Pod.Spec.NodeSelector = map[string]string{}
	}
	dp.Spec.Pod.Spec.NodeSelector[hostnameLabel] = name
	dp.Spec.Pod.Spec.NodeName = ""
	return nil
}

// replaceDevPodSpec is a save from the YAML tab: dp with the spec of the
// manifest in text. The name and the owner are not the manifest's to
// change.
func (s *Server) replaceDevPodSpec(r *http.Request, sess Session, dp *devpodv1alpha1.DevPod, text string) (PatchResponse, *serverErr) {
	var in devpodv1alpha1.DevPod
	if err := yaml.UnmarshalStrict([]byte(text), &in); err != nil {
		return PatchResponse{}, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "invalid YAML: " + err.Error(), nil}
	}
	if in.Kind != "" && in.Kind != "DevPod" {
		return PatchResponse{}, badRequest("kind is %q: this is a DevPod", in.Kind)
	}
	if in.Name != "" && in.Name != dp.Name {
		return PatchResponse{}, badRequest("a DevPod cannot be renamed (metadata.name is %q, this is %q)", in.Name, dp.Name)
	}
	if in.Spec.Owner != "" && in.Spec.Owner != dp.Spec.Owner {
		return PatchResponse{}, badRequest("the owner cannot be changed (spec.owner is %q, this DevPod is %q's)", in.Spec.Owner, dp.Spec.Owner)
	}
	before := dp.DeepCopy()
	dp = dp.DeepCopy()
	dp.Spec = in.Spec
	dp.Spec.Owner = before.Spec.Owner

	if !sess.Admin {
		if dp.Spec.VM != nil {
			return PatchResponse{}, badRequest("vm workloads cannot be metered for quota; admins only")
		}
		if dp.Spec.Pod == nil {
			return PatchResponse{}, badRequest("spec.pod is required")
		}
		// what a template stamped stays as it was: not added to, changed or dropped
		if !maps.Equal(koreAnnotations(before.Spec.Pod), koreAnnotations(dp.Spec.Pod)) {
			return PatchResponse{}, &serverErr{http.StatusForbidden, "KORE_ANNOTATIONS_FORBIDDEN",
				"CPU-binding annotations come from a template and cannot be edited", nil}
		}
		if len(KoreAnnotationKeys(in.Annotations)) > 0 {
			return PatchResponse{}, &serverErr{http.StatusForbidden, "KORE_ANNOTATIONS_FORBIDDEN",
				"CPU-binding annotations come from a template and cannot be edited", nil}
		}
		// a pod definition that is being changed is the user's own writing
		// now, whoever wrote it first: it may not reach Secrets
		if !equality.Semantic.DeepEqual(before.Spec.Pod, dp.Spec.Pod) {
			if apiErr := guardUserPodSpec(&dp.Spec.Pod.Spec, dp.Spec.Owner); apiErr != nil {
				return PatchResponse{}, apiErr
			}
		}
	}
	if apiErr := normalizeNodeName(dp); apiErr != nil {
		return PatchResponse{}, apiErr
	}
	if equality.Semantic.DeepEqual(before.Spec, dp.Spec) {
		return PatchResponse{DevPod: dp}, nil // nothing changed
	}
	// a new node, or starting on the one it has: it has to be there, and
	// not held by someone else now
	if node := pinnedNode(dp); node != "" && (node != pinnedNode(before) || (dp.Spec.Running && !before.Spec.Running)) {
		var n corev1.Node
		if err := s.Client.Get(r.Context(), types.NamespacedName{Name: node}, &n); err != nil {
			return PatchResponse{}, badRequest("node %q not found", node)
		}
		if apiErr := s.pinErr(r.Context(), dp.Spec.Owner, node); apiErr != nil {
			return PatchResponse{}, apiErr
		}
	}
	if dp.Spec.Running {
		if apiErr := s.quotaErrFor(r, sess, dp, dp.Name); apiErr != nil {
			return PatchResponse{}, apiErr
		}
	}
	// the Pod is recreated when its rendered spec changes while running
	rebuild := before.Spec.Running && dp.Spec.Running &&
		(!equality.Semantic.DeepEqual(before.Spec.Pod, dp.Spec.Pod) || before.Spec.Shell != dp.Spec.Shell)
	if err := s.Client.Update(r.Context(), dp); err != nil {
		if apierrors.IsConflict(err) {
			return PatchResponse{}, &serverErr{http.StatusConflict, "CONFLICT", "the DevPod changed meanwhile: reload and try again", err.Error()}
		}
		return PatchResponse{}, &serverErr{http.StatusBadRequest, "BAD_REQUEST", rejected("update", err), err.Error()}
	}
	return PatchResponse{DevPod: dp, Rebuild: rebuild}, nil
}

// handlePutDevPodYAML serves PUT /api/devpods/{name}/yaml: {"yaml": …}.
func (s *Server) handlePutDevPodYAML(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var req struct {
		YAML string `json:"yaml"`
	}
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if req.YAML == "" {
		s.writeServerErr(w, badRequest("yaml is required"))
		return
	}
	// like every edit, on the object as the API server has it now
	fresh := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: dp.Name, Namespace: dp.Namespace}}
	if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: dp.Name, Namespace: dp.Namespace}, fresh); err == nil {
		dp = fresh
	}
	resp, apiErr := s.replaceDevPodSpec(r, sess, dp, req.YAML)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}
