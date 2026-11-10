// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Server carries the webui backend's dependencies. Handlers run the
// fixed sequence: session → ownership → (mutations) quota → execute
// under the webui ServiceAccount.
type Server struct {
	Client       client.Client // cached manager client
	Reader       client.Reader // uncached APIReader: events, live Pod readback
	Cache        cache.Cache   // informers for the SSE watch
	NS           string        // devpods namespace
	Sessions     *SessionManager
	OAuth        *OAuth
	DefaultQuota devpodv1alpha1.UserQuota
	KoreEnabled  bool
	Origin       string // allowed Origin for mutating requests

	// PubkeySelfService gates the pubkey management surface. Disable
	// on deployments where LDAP (not the User CRD) owns SSH keys.
	PubkeySelfService bool
	// SSHHost/SSHPort are advertised to the frontend so it can render
	// copy-pastable `ssh [-p port] user+pod@host` command lines.
	SSHHost string
	SSHPort int
	// SSHLoginSuffix, when set, is appended to the login as
	// "<owner>+<pod>+<suffix>" (e.g. a proxy/routing token). Empty =
	// the plain "<owner>+<pod>" form.
	SSHLoginSuffix string

	// PasswordAuth enables the built-in username+password login path.
	PasswordAuth bool
	// PasswordMinLength gates password create/reset/change.
	PasswordMinLength int
	// Admins is the --admins username allowlist; effective admin also
	// includes User.spec.admin (kubectl-managed).
	Admins map[string]bool
	// SecureCookies sets the Secure attribute on session cookies (https
	// deployments true; plain-http dev/e2e false).
	SecureCookies bool

	// Audit holds the audit store backends; nil disables every audit
	// endpoint and hides the audit UI.
	Audit *AuditDeps
	// GatewayConfigName is the GatewayConfig whose audit policy is the
	// cluster default ("" = "default").
	GatewayConfigName string

	// Live is the phase-2 live telemetry layer; nil hides it.
	Live *LiveDeps
}

type apiBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, status int, code, msg string, detail any) {
	s.writeJSON(w, status, apiBody{Code: code, Message: msg, Detail: detail})
}

// sessionFrom authenticates the request; on failure it has already
// written the 401.
func (s *Server) sessionFrom(w http.ResponseWriter, r *http.Request) (Session, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not logged in", nil)
		return Session{}, false
	}
	sess, err := s.Sessions.Verify(c.Value, time.Now())
	if err != nil {
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error(), nil)
		return Session{}, false
	}
	return sess, true
}

// ownedDevPods lists the namespace and filters by owner in-process
// (no field index needed at this scale).
func (s *Server) ownedDevPods(r *http.Request, owner string) ([]devpodv1alpha1.DevPod, error) {
	var list devpodv1alpha1.DevPodList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(s.NS)); err != nil {
		return nil, err
	}
	var out []devpodv1alpha1.DevPod
	for _, dp := range list.Items {
		if dp.Spec.Owner == owner {
			out = append(out, dp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// getOwned fetches one DevPod and enforces ownership. Non-owners get
// 404 (not 403) to avoid existence leaks.
func (s *Server) getOwned(w http.ResponseWriter, r *http.Request, sess Session, name string) (*devpodv1alpha1.DevPod, bool) {
	var dp devpodv1alpha1.DevPod
	err := s.Client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.NS}, &dp)
	if apierrors.IsNotFound(err) || (err == nil && dp.Spec.Owner != sess.User && !sess.Admin) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "devpod not found", nil)
		return nil, false
	}
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return nil, false
	}
	return &dp, true
}

// rejectKore enforces the stamping invariant for non-admins: no
// kore.zjusct.io/* annotations anywhere in the submitted object.
func (s *Server) rejectKore(w http.ResponseWriter, sess Session, dp *devpodv1alpha1.DevPod) bool {
	if sess.Admin {
		return true
	}
	offending := KoreAnnotationKeys(dp.Annotations)
	if dp.Spec.Pod != nil {
		offending = append(offending, KoreAnnotationKeys(dp.Spec.Pod.Metadata.Annotations)...)
	}
	if len(offending) > 0 {
		s.writeErr(w, http.StatusForbidden, "KORE_ANNOTATIONS_FORBIDDEN",
			"CPU-binding annotations can only come from a template (pick one via templateRef)", offending)
		return false
	}
	return true
}

// checkQuotaFor runs the quota gate for proposed. exclude names a
// DevPod to leave out of "existing" (the one being updated).
func (s *Server) checkQuotaFor(w http.ResponseWriter, r *http.Request, sess Session, proposed *devpodv1alpha1.DevPod, exclude string) bool {
	if apiErr := s.quotaErrFor(r, sess, proposed, exclude); apiErr != nil {
		s.writeErr(w, apiErr.status, apiErr.Code, apiErr.Message, apiErr.Detail)
		return false
	}
	return true
}

// quotaErrFor is checkQuotaFor without writing the response.
func (s *Server) quotaErrFor(r *http.Request, sess Session, proposed *devpodv1alpha1.DevPod, exclude string) *serverErr {
	if sess.Admin {
		return nil
	}
	var u devpodv1alpha1.User
	userPtr := &u
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		userPtr = nil // no User CR: defaults apply
	}
	owned, err := s.ownedDevPods(r, sess.User)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	var existing []devpodv1alpha1.DevPod
	for _, dp := range owned {
		if dp.Name != exclude {
			existing = append(existing, dp)
		}
	}
	if qErr := CheckQuota(EffectiveQuota(userPtr, s.DefaultQuota), existing, proposed); qErr != nil {
		return &serverErr{http.StatusConflict, "QUOTA_EXCEEDED", qErr.Error(), qErr.Violations}
	}
	return nil
}

// CreateRequest is the POST /api/devpods body. Either YAML (raw
// DevPod manifest) or the structured fields; TemplateRef composes
// with both (preset templates require no pod, overlays require one).
type CreateRequest struct {
	Name          string                          `json:"name,omitempty"` // suffix; full name = "<owner>-<name>"
	TemplateRef   string                          `json:"templateRef,omitempty"`
	YAML          string                          `json:"yaml,omitempty"`
	Image         string                          `json:"image,omitempty"`
	CPU           string                          `json:"cpu,omitempty"`
	Memory        string                          `json:"memory,omitempty"`
	Shell         string                          `json:"shell,omitempty"`
	Persistence   *devpodv1alpha1.PersistenceSpec `json:"persistence,omitempty"`
	GPU           string                          `json:"gpu,omitempty"` // nvidia.com/gpu limit
	Collaborators []string                        `json:"collaborators,omitempty"`
}

func (s *Server) handleListDevPods(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	items, err := s.ownedDevPods(r, sess.User)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if items == nil {
		items = []devpodv1alpha1.DevPod{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items, "pods": s.podInfos(r.Context(), items)})
}

// BindingInfo is the Kore readback shown on the detail page: desired
// (pool/pool-size from the stamped annotations) + actual (allocated
// cpuset / reserved NUMA written back by Kore onto the live Pod).
type BindingInfo struct {
	AllocatedCpuset string `json:"allocatedCpuset,omitempty"`
	ReservedNUMA    string `json:"reservedNuma,omitempty"`
	Pool            string `json:"pool,omitempty"`
	PoolSize        string `json:"poolSize,omitempty"`
}

type DevPodDetail struct {
	DevPod  devpodv1alpha1.DevPod `json:"devpod"`
	Binding *BindingInfo          `json:"binding,omitempty"`
	Pod     *PodInfo              `json:"pod,omitempty"`
}

func (s *Server) bindingInfo(r *http.Request, dp *devpodv1alpha1.DevPod) *BindingInfo {
	if !s.KoreEnabled || dp.Spec.Pod == nil {
		return nil
	}
	desired := dp.Spec.Pod.Metadata.Annotations
	if len(KoreAnnotationKeys(desired)) == 0 {
		return nil // unbound DevPod: no panel
	}
	info := &BindingInfo{
		Pool:     desired[KorePrefix+"pool"],
		PoolSize: desired[KorePrefix+"pool-size"],
	}
	if ref := dp.Status.WorkloadRef; ref != nil && ref.Kind == "Pod" {
		var pod corev1.Pod
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: ref.Name, Namespace: s.NS}, &pod); err == nil {
			info.AllocatedCpuset = pod.Annotations[KorePrefix+"allocated-cpuset"]
			info.ReservedNUMA = pod.Annotations[KorePrefix+"reserved-numa"]
		}
	}
	return info
}

func (s *Server) handleGetDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, DevPodDetail{DevPod: *dp, Binding: s.bindingInfo(r, dp), Pod: s.podInfo(r.Context(), dp)})
}

func (s *Server) handleCreateDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var req CreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}

	dp, apiErr := s.buildDevPod(sess, req)
	if apiErr != nil {
		s.writeErr(w, apiErr.status, apiErr.Code, apiErr.Message, apiErr.Detail)
		return
	}
	if !s.rejectKore(w, sess, dp) {
		return
	}

	if req.TemplateRef != "" {
		var tpl devpodv1alpha1.DevPodTemplate
		if err := s.Client.Get(r.Context(), types.NamespacedName{Name: req.TemplateRef}, &tpl); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("template %q not found", req.TemplateRef), nil)
			return
		}
		if !s.KoreEnabled && tpl.Spec.Binding != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "binding templates are unavailable: Kore is not installed", nil)
			return
		}
		if err := ApplyTemplate(dp, &tpl); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
	}

	if !sess.Admin {
		if dp.Spec.VM != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "vm workloads cannot be metered for quota; admins only", nil)
			return
		}
		if dp.Spec.Pod == nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "spec.pod is required (or pick a preset template)", nil)
			return
		}
		if err := RequireLimits(&dp.Spec.Pod.Spec); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
	}
	if !s.checkQuotaFor(w, r, sess, dp, "") {
		return
	}

	if err := s.Client.Create(r.Context(), dp); err != nil {
		// k8s Status errors (CEL rejections etc.) pass through verbatim.
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "create rejected", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, dp)
}

type serverErr struct {
	status  int
	Code    string
	Message string
	Detail  any
}

// buildDevPod turns a CreateRequest into the pre-stamping DevPod.
func (s *Server) buildDevPod(sess Session, req CreateRequest) (*devpodv1alpha1.DevPod, *serverErr) {
	if req.YAML != "" {
		var dp devpodv1alpha1.DevPod
		if err := yaml.UnmarshalStrict([]byte(req.YAML), &dp); err != nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "invalid YAML: " + err.Error(), nil}
		}
		dp.Namespace = s.NS
		if dp.Spec.Owner == "" {
			dp.Spec.Owner = sess.User
		}
		if !sess.Admin {
			if dp.Spec.Owner != sess.User {
				return nil, &serverErr{http.StatusForbidden, "FORBIDDEN", "owner must be yourself", nil}
			}
			if !strings.HasPrefix(dp.Name, sess.User+"-") {
				return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST",
					fmt.Sprintf("name must start with %q (budget: %d chars after the prefix)", sess.User+"-", NameBudget(sess.User)), nil}
			}
		}
		return &dp, nil
	}

	if req.Name == "" {
		return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "name is required", nil}
	}
	name := sess.User + "-" + req.Name
	if len(name) > MaxDevPodNameLen || !dns1123Label.MatchString(name) {
		return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST",
			fmt.Sprintf("name %q invalid or too long (max %d chars for the suffix)", name, NameBudget(sess.User)), nil}
	}
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.NS},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:         sess.User,
			Running:       true,
			Shell:         req.Shell,
			Persistence:   req.Persistence,
			Collaborators: req.Collaborators,
		},
	}
	if req.Image != "" { // absent for preset-template creates
		limits := corev1.ResourceList{}
		for res, val := range map[corev1.ResourceName]string{corev1.ResourceCPU: req.CPU, corev1.ResourceMemory: req.Memory, resourceGPU: req.GPU} {
			if err := setLimit(limits, res, val); err != nil {
				return nil, err
			}
		}
		dp.Spec.Pod = &devpodv1alpha1.PodWorkloadSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      "dev",
					Image:     req.Image,
					Resources: corev1.ResourceRequirements{Limits: limits},
				}},
			},
		}
	}
	return dp, nil
}

// setLimit parses val into limits[res]; "" (and "0" for the GPU) drops
// the limit.
func setLimit(limits corev1.ResourceList, res corev1.ResourceName, val string) *serverErr {
	if val == "" || (res == resourceGPU && val == "0") {
		delete(limits, res)
		return nil
	}
	q, err := resource.ParseQuantity(val)
	if err != nil || q.Sign() < 0 {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("invalid %s quantity %q", res, val), nil}
	}
	limits[res] = q
	return nil
}

// PatchRequest is the PATCH body: the running flag plus the editable
// fields of the edit drawer (UI spec §5.6). Absent fields stay as they
// are; persistence is immutable after creation.
type PatchRequest struct {
	Running       *bool                           `json:"running,omitempty"`
	Image         *string                         `json:"image,omitempty"`
	CPU           *string                         `json:"cpu,omitempty"`
	Memory        *string                         `json:"memory,omitempty"`
	GPU           *string                         `json:"gpu,omitempty"`
	Shell         *string                         `json:"shell,omitempty"`
	Collaborators *[]string                       `json:"collaborators,omitempty"`
	Persistence   *devpodv1alpha1.PersistenceSpec `json:"persistence,omitempty"`
}

// PatchResponse: Rebuild reports that a running DevPod's Pod is being
// recreated (its sessions drop).
type PatchResponse struct {
	DevPod  *devpodv1alpha1.DevPod `json:"devpod"`
	Rebuild bool                   `json:"rebuild"`
}

func (req *PatchRequest) editsSpec() bool {
	return req.Image != nil || req.CPU != nil || req.Memory != nil || req.GPU != nil
}

// applyEdits applies req's spec edits to dp.
func applyEdits(dp *devpodv1alpha1.DevPod, req *PatchRequest) *serverErr {
	if req.Persistence != nil && !equality.Semantic.DeepEqual(req.Persistence, dp.Spec.Persistence) {
		return &serverErr{http.StatusBadRequest, "IMMUTABLE", "persistence cannot change after creation", nil}
	}
	if req.Shell != nil {
		dp.Spec.Shell = *req.Shell
	}
	if req.Collaborators != nil {
		dp.Spec.Collaborators = *req.Collaborators
	}
	if !req.editsSpec() {
		return nil
	}
	if dp.Spec.Pod == nil {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", "only pod workloads can be edited", nil}
	}
	name := targetContainerName(dp)
	var c *corev1.Container
	for i := range dp.Spec.Pod.Spec.Containers {
		if dp.Spec.Pod.Spec.Containers[i].Name == name {
			c = &dp.Spec.Pod.Spec.Containers[i]
		}
	}
	if c == nil {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("target container %q not found", name), nil}
	}
	if req.Image != nil {
		if *req.Image == "" {
			return &serverErr{http.StatusBadRequest, "BAD_REQUEST", "image cannot be empty", nil}
		}
		c.Image = *req.Image
	}
	if c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}
	for res, val := range map[corev1.ResourceName]*string{corev1.ResourceCPU: req.CPU, corev1.ResourceMemory: req.Memory, resourceGPU: req.GPU} {
		if val == nil {
			continue
		}
		if err := setLimit(c.Resources.Limits, res, *val); err != nil {
			return err
		}
		// requests above the new limit would be rejected by the apiserver
		if q, ok := c.Resources.Requests[res]; ok {
			if lim, has := c.Resources.Limits[res]; !has || q.Cmp(lim) > 0 {
				delete(c.Resources.Requests, res)
			}
		}
	}
	return nil
}

func (s *Server) handlePatchDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var req PatchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	// The cached copy can trail the controller's status writes; on a
	// version conflict re-read from the API server and apply again.
	for attempt := 0; ; attempt++ {
		resp, apiErr := s.patchDevPod(r, sess, dp, &req)
		if apiErr == nil {
			s.writeJSON(w, http.StatusOK, resp)
			return
		}
		if apiErr.Code != "CONFLICT" || attempt >= 4 {
			s.writeErr(w, apiErr.status, apiErr.Code, apiErr.Message, apiErr.Detail)
			return
		}
		fresh := &devpodv1alpha1.DevPod{}
		if err := s.Reader.Get(r.Context(), client.ObjectKeyFromObject(dp), fresh); err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		dp = fresh
	}
}

// patchDevPod applies req to dp and writes it; a version conflict comes
// back as code CONFLICT.
func (s *Server) patchDevPod(r *http.Request, sess Session, dp *devpodv1alpha1.DevPod, req *PatchRequest) (PatchResponse, *serverErr) {
	before := dp.DeepCopy()
	dp = dp.DeepCopy()
	if req.Running != nil {
		dp.Spec.Running = *req.Running
	}
	if apiErr := applyEdits(dp, req); apiErr != nil {
		return PatchResponse{}, apiErr
	}
	if equality.Semantic.DeepEqual(before.Spec, dp.Spec) {
		return PatchResponse{DevPod: dp}, nil // no-op
	}
	if !sess.Admin && req.editsSpec() {
		if err := RequireLimits(&dp.Spec.Pod.Spec); err != nil {
			return PatchResponse{}, &serverErr{http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil}
		}
	}
	// waking, or resizing a running DevPod, re-enters the compute quota
	if dp.Spec.Running && (!before.Spec.Running || req.editsSpec()) {
		if apiErr := s.quotaErrFor(r, sess, dp, dp.Name); apiErr != nil {
			return PatchResponse{}, apiErr
		}
	}
	// the Pod is recreated when its rendered spec changes while running
	rebuild := before.Spec.Running && dp.Spec.Running &&
		(!equality.Semantic.DeepEqual(before.Spec.Pod, dp.Spec.Pod) || before.Spec.Shell != dp.Spec.Shell)
	if err := s.Client.Update(r.Context(), dp); err != nil {
		if apierrors.IsConflict(err) {
			return PatchResponse{}, &serverErr{http.StatusConflict, "CONFLICT", "the DevPod changed meanwhile", err.Error()}
		}
		return PatchResponse{}, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "update rejected", err.Error()}
	}
	return PatchResponse{DevPod: dp, Rebuild: rebuild}, nil
}

func (s *Server) handleDeleteDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	if err := s.Client.Delete(r.Context(), dp); err != nil && !apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDevPodEvents(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var events corev1.EventList
	if err := s.Reader.List(r.Context(), &events,
		client.InNamespace(s.NS),
		client.MatchingFields{"involvedObject.name": dp.Name}); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	sort.Slice(events.Items, func(i, j int) bool {
		return events.Items[j].LastTimestamp.Before(&events.Items[i].LastTimestamp)
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"items": events.Items})
}
