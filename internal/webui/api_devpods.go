// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
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

	// Clusters, when set, makes this the console of other clusters: their
	// agents connect here. Nil: the console of the cluster it runs in.
	Clusters *clusters.Registry
	// GatewayLinks: connections the gateways of several clusters keep to
	// this console, for watching and ending sessions they hold.
	GatewayLinks *GatewayLinks
	// gatewaysSeen: when each gateway's status was last written.
	gatewaysSeen map[string]time.Time
	// CLIKey lets the SSH gateway act for a user it authenticated (the
	// CLI over SSH); empty = off.
	CLIKey []byte
	// Reservations: node reservation rules; nil = off.
	Reservations *ReservationRules
	// Snapshots: saving a DevPod as an image; nil = off.
	Snapshots *Snapshots
	// HTTPProxy: DevPod ports published through the gateway; nil = off.
	HTTPProxy *HTTPProxy

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
	// LDAPAuth, when set, signs in users with their password in the
	// LDAP directory (the one the gateway takes SSH keys from). It needs
	// no User object.
	LDAPAuth PasswordChecker
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
	// LiveSessions relays live session views from the gateways (audit
	// spec §16); nil disables live viewing.
	LiveSessions *LiveSessions

	// Webshell bridges browser terminals to the SSH gateway; nil turns
	// the web terminal off.
	Webshell *Webshell
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
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return s.bearerSession(w, r, bearer)
	}
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

// devPods lists DevPods of the request's clusters — its one cluster, or
// every cluster when it named none: owner's, or everyone's when owner is
// empty. By cluster, then name.
func (s *Server) devPods(r *http.Request, owner string) ([]InCluster, error) {
	var out []InCluster
	for _, cc := range s.ks(r) {
		var list devpodv1alpha1.DevPodList
		if err := cc.Client.List(r.Context(), &list, client.InNamespace(s.NS)); err != nil {
			return nil, err
		}
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
		for _, dp := range list.Items {
			if owner == "" || dp.Spec.Owner == owner {
				out = append(out, InCluster{DevPod: dp, Cluster: cc.Name})
			}
		}
	}
	return out, nil
}

func plainDevPods(in []InCluster, err error) ([]devpodv1alpha1.DevPod, error) {
	if err != nil {
		return nil, err
	}
	var out []devpodv1alpha1.DevPod
	for i := range in {
		out = append(out, in[i].DevPod)
	}
	return out, nil
}

// ownedDevPods lists owner's DevPods in the request's clusters.
func (s *Server) ownedDevPods(r *http.Request, owner string) ([]devpodv1alpha1.DevPod, error) {
	return plainDevPods(s.devPods(r, owner))
}

// getOwned fetches one DevPod and enforces ownership. Non-owners get
// 404 (not 403) to avoid existence leaks. In a console of several
// clusters it also settles which cluster the request is about.
func (s *Server) getOwned(w http.ResponseWriter, r *http.Request, sess Session, name string) (*devpodv1alpha1.DevPod, bool) {
	return s.findDevPod(w, r, sess, name)
}

// koreErr enforces the stamping invariant for non-admins: no
// kore.zjusct.io/* annotations anywhere in the submitted object.
func koreErr(sess Session, dp *devpodv1alpha1.DevPod) *serverErr {
	if sess.Admin {
		return nil
	}
	offending := KoreAnnotationKeys(dp.Annotations)
	if dp.Spec.Pod != nil {
		offending = append(offending, KoreAnnotationKeys(dp.Spec.Pod.Metadata.Annotations)...)
	}
	if len(offending) > 0 {
		return &serverErr{http.StatusForbidden, "KORE_ANNOTATIONS_FORBIDDEN",
			"CPU-binding annotations can only come from a template (pick one via templateRef)", offending}
	}
	return nil
}

// rejectKore is koreErr, written to the response.
func (s *Server) rejectKore(w http.ResponseWriter, sess Session, dp *devpodv1alpha1.DevPod) bool {
	if apiErr := koreErr(sess, dp); apiErr != nil {
		s.writeErr(w, apiErr.status, apiErr.Code, apiErr.Message, apiErr.Detail)
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

// quotaErrFor is checkQuotaFor without writing the response. pending
// are DevPods about to be created with proposed: counted as existing.
func (s *Server) quotaErrFor(r *http.Request, sess Session, proposed *devpodv1alpha1.DevPod, exclude string, pending ...*devpodv1alpha1.DevPod) *serverErr {
	if sess.Admin {
		return nil
	}
	cc := s.k(r) // a quota is a cluster's: the DevPods counted are that cluster's
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
	for _, dp := range pending {
		existing = append(existing, *dp)
	}
	if qErr := CheckQuota(EffectiveQuota(userPtr, cc.Name, s.clusterQuota(r.Context(), cc)), existing, proposed); qErr != nil {
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
	// Node pins the DevPod to a node (form path; YAML sets the hostname
	// node selector itself). Refused while someone else holds the node.
	Node string `json:"node,omitempty"`
}

// listScope reads ?scope=mine|all; all is for admins (the console's
// 全部 DevPod table, UI spec §5.1).
func (s *Server) listScope(w http.ResponseWriter, r *http.Request, sess Session) (all bool, ok bool) {
	switch r.URL.Query().Get("scope") {
	case "", "mine":
		return false, true
	case "all":
		if !sess.Admin {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "admin only", nil)
			return false, false
		}
		return true, true
	default:
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "scope must be mine or all", nil)
		return false, false
	}
}

// allDevPods lists every DevPod of the request's clusters.
func (s *Server) allDevPods(r *http.Request) ([]devpodv1alpha1.DevPod, error) {
	return plainDevPods(s.devPods(r, ""))
}

func (s *Server) handleListDevPods(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	all, ok := s.listScope(w, r, sess)
	if !ok {
		return
	}
	owner := sess.User
	if all {
		owner = ""
	}
	items, err := s.devPods(r, owner)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	if sess.Token != nil && len(sess.Token.DevPods) > 0 { // a token limited to some DevPods sees only those
		items = slices.DeleteFunc(items, func(dp InCluster) bool { return !sess.AllowsDevPod(dp.Name) })
	}
	if items == nil {
		items = []InCluster{}
	}
	out := map[string]any{"items": items, "pods": s.podInfos(r.Context(), items)}
	if stale := s.staleOf(w, s.ks(r)); stale != nil {
		out["stale"] = stale // cluster → since when its rows are the last known state
	}
	s.writeJSON(w, http.StatusOK, out)
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
	Cluster string                `json:"cluster,omitempty"`
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
		if err := s.k(r).Reader.Get(r.Context(), types.NamespacedName{Name: ref.Name, Namespace: s.NS}, &pod); err == nil {
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
	s.writeJSON(w, http.StatusOK, DevPodDetail{DevPod: *dp, Cluster: s.k(r).Name, Binding: s.bindingInfo(r, dp), Pod: s.podInfo(r.Context(), dp)})
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

	dp, apiErr := s.prepareDevPod(r, sess, req, nil)
	if apiErr != nil {
		s.writeErr(w, apiErr.status, apiErr.Code, apiErr.Message, apiErr.Detail)
		return
	}
	if err := s.k(r).Client.Create(r.Context(), dp); err != nil {
		// k8s Status errors (CEL rejections etc.) pass through verbatim.
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("create", err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, dp)
}

// prepareDevPod is everything between a create request and the write:
// the DevPod it describes, with every rule applied — who may own it,
// the stamping and Secret guards, the template, limits, the node choice
// and the quota. pending are DevPods accepted earlier in the same batch
// and not yet created: they count toward the quota.
func (s *Server) prepareDevPod(r *http.Request, sess Session, req CreateRequest, pending []*devpodv1alpha1.DevPod) (*devpodv1alpha1.DevPod, *serverErr) {
	dp, apiErr := s.buildDevPod(sess, req)
	if apiErr != nil {
		return nil, apiErr
	}
	if apiErr := koreErr(sess, dp); apiErr != nil {
		return nil, apiErr
	}
	// What the user wrote themselves (the YAML path) may not reach
	// Secrets or a service account token. Checked before a template is
	// applied: an admin's preset is not the user's writing.
	if !sess.Admin && dp.Spec.Pod != nil {
		if apiErr := guardUserPodSpec(&dp.Spec.Pod.Spec, sess.User); apiErr != nil {
			return nil, apiErr
		}
	}

	if req.TemplateRef != "" {
		var tpl devpodv1alpha1.DevPodTemplate
		if err := s.k(r).Client.Get(r.Context(), types.NamespacedName{Name: req.TemplateRef}, &tpl); err != nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("template %q not found", req.TemplateRef), nil}
		}
		if !s.KoreEnabled && tpl.Spec.Binding != nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "binding templates are unavailable: Kore is not installed", nil}
		}
		if err := ApplyTemplate(dp, &tpl); err != nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil}
		}
	}

	if !sess.Admin {
		if dp.Spec.VM != nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "vm workloads cannot be metered for quota; admins only", nil}
		}
		if dp.Spec.Pod == nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "spec.pod is required (or pick a preset template)", nil}
		}
	}
	if apiErr := s.quotaErrFor(r, sess, dp, "", pending...); apiErr != nil {
		return nil, apiErr
	}
	if req.Node != "" {
		if dp.Spec.Pod == nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "only pod workloads can be pinned to a node", nil}
		}
		if dp.Spec.Pod.Spec.NodeSelector == nil {
			dp.Spec.Pod.Spec.NodeSelector = map[string]string{}
		}
		dp.Spec.Pod.Spec.NodeSelector[hostnameLabel] = req.Node
	}
	// spec.nodeName would bypass the scheduler and the node choice rule:
	// it becomes the same pin as a node picked in the form
	if dp.Spec.Pod != nil && dp.Spec.Pod.Spec.NodeName != "" {
		name := dp.Spec.Pod.Spec.NodeName
		if sel := dp.Spec.Pod.Spec.NodeSelector[hostnameLabel]; sel != "" && sel != name {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("nodeName %q and the %s node selector %q name different nodes", name, hostnameLabel, sel), nil}
		}
		if dp.Spec.Pod.Spec.NodeSelector == nil {
			dp.Spec.Pod.Spec.NodeSelector = map[string]string{}
		}
		dp.Spec.Pod.Spec.NodeSelector[hostnameLabel] = name
		dp.Spec.Pod.Spec.NodeName = ""
	}
	if node := pinnedNode(dp); node != "" {
		var n corev1.Node
		if err := s.k(r).Client.Get(r.Context(), types.NamespacedName{Name: node}, &n); err != nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("node %q not found", node), nil}
		}
		if apiErr := s.pinErr(r.Context(), dp.Spec.Owner, node); apiErr != nil {
			return nil, apiErr
		}
	}
	return dp, nil
}

// rejected is the message for something the API server refused: what was
// tried, and the server's reason — in the message, where a person sees
// it, not only in the detail. For a validation failure the reason is the
// rules that failed, without the server's packaging around them.
func rejected(what string, err error) string {
	reason := err.Error()
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		status := st.Status()
		reason = status.Message
		if d := status.Details; d != nil && len(d.Causes) > 0 {
			var parts []string
			for _, c := range d.Causes {
				msg := c.Message
				// a rule on the whole object is reported as: Invalid value: "object": <the rule's message>
				for _, wrap := range []string{`Invalid value: "object": `, `Invalid value: "string": `, `Invalid value: "integer": `} {
					msg = strings.TrimPrefix(msg, wrap)
				}
				if c.Field != "" && c.Field != "<nil>" && !strings.Contains(msg, c.Field) {
					msg = c.Field + ": " + msg
				}
				if !slices.Contains(parts, msg) {
					parts = append(parts, msg)
				}
			}
			reason = strings.Join(parts, "; ")
		}
	}
	return what + " rejected: " + reason
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
	Running       *bool     `json:"running,omitempty"`
	Image         *string   `json:"image,omitempty"`
	CPU           *string   `json:"cpu,omitempty"`
	Memory        *string   `json:"memory,omitempty"`
	GPU           *string   `json:"gpu,omitempty"`
	Shell         *string   `json:"shell,omitempty"`
	Collaborators *[]string `json:"collaborators,omitempty"`
	// Node migrates the DevPod (admin only): it pins the Pod to that
	// node through a hostname node selector; "" leaves the choice to
	// the scheduler again.
	Node        *string                         `json:"node,omitempty"`
	Persistence *devpodv1alpha1.PersistenceSpec `json:"persistence,omitempty"`
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
		if err := s.k(r).Reader.Get(r.Context(), client.ObjectKeyFromObject(dp), fresh); err != nil {
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
	if sess.Token != nil {
		edits := req.editsSpec() || req.Shell != nil || req.Collaborators != nil || req.Node != nil
		if (req.Running != nil && !sess.Token.has("devpods:power")) || (edits && !sess.Token.has("devpods:update")) {
			return PatchResponse{}, &serverErr{http.StatusForbidden, "SCOPE", "this token lacks the scope for this change (devpods:power to start / stop, devpods:update to edit)", nil}
		}
	}
	if apiErr := applyEdits(dp, req); apiErr != nil {
		return PatchResponse{}, apiErr
	}
	if req.Node != nil {
		if apiErr := s.applyNode(r, sess, dp, *req.Node); apiErr != nil {
			return PatchResponse{}, apiErr
		}
	}
	if equality.Semantic.DeepEqual(before.Spec, dp.Spec) {
		return PatchResponse{DevPod: dp}, nil // no-op
	}
	// starting a DevPod pinned to a node someone else holds now
	if dp.Spec.Running && !before.Spec.Running {
		if apiErr := s.pinErr(r.Context(), dp.Spec.Owner, pinnedNode(dp)); apiErr != nil {
			return PatchResponse{}, apiErr
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
	if err := s.k(r).Client.Update(r.Context(), dp); err != nil {
		if apierrors.IsConflict(err) {
			return PatchResponse{}, &serverErr{http.StatusConflict, "CONFLICT", "the DevPod changed meanwhile", err.Error()}
		}
		return PatchResponse{}, &serverErr{http.StatusBadRequest, "BAD_REQUEST", rejected("update", err), err.Error()}
	}
	return PatchResponse{DevPod: dp, Rebuild: rebuild}, nil
}

const hostnameLabel = "kubernetes.io/hostname"

// applyNode pins dp to node ("" unpins).
func (s *Server) applyNode(r *http.Request, sess Session, dp *devpodv1alpha1.DevPod, node string) *serverErr {
	if dp.Spec.Pod == nil {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", "only pod workloads can be migrated", nil}
	}
	sel := dp.Spec.Pod.Spec.NodeSelector
	if node == "" {
		delete(sel, hostnameLabel)
		if len(sel) == 0 {
			dp.Spec.Pod.Spec.NodeSelector = nil
		}
		return nil
	}
	var n corev1.Node
	if err := s.k(r).Client.Get(r.Context(), types.NamespacedName{Name: node}, &n); err != nil {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("node %q not found", node), nil}
	}
	if sel == nil {
		sel = map[string]string{}
		dp.Spec.Pod.Spec.NodeSelector = sel
	}
	sel[hostnameLabel] = node
	// the node rule is judged by the DevPod's owner, whoever asks
	return s.pinErr(r.Context(), dp.Spec.Owner, node)
}

// handleRedeployDevPod serves POST /api/devpods/{name}/redeploy: it
// deletes the DevPod's Pod and the controller creates a fresh one (the
// home volume stays; SSH sessions drop).
func (s *Server) handleRedeployDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	ref := dp.Status.WorkloadRef
	if !dp.Spec.Running || ref == nil || ref.Kind != "Pod" {
		s.writeErr(w, http.StatusConflict, "NOT_RUNNING", "the DevPod has no Pod to redeploy; start it instead", nil)
		return
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: s.NS}}
	if err := s.k(r).Client.Delete(r.Context(), pod); err != nil && !apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if err := s.k(r).Client.Delete(r.Context(), dp); err != nil && !apierrors.IsNotFound(err) {
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
	if err := s.k(r).Reader.List(r.Context(), &events,
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
