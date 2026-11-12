// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/coder/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// Clusters this console manages. An admin registers one and is shown a
// join token once; the cluster's agent joins with it (recording its own
// public key) and from then on connects by proving that key. Everything
// here needs Server.Clusters: without it this is a console of the one
// cluster it runs in.

// MaxClusterNameLen: a cluster's name is typed after a DevPod's in an
// SSH login, so it stays short.
const MaxClusterNameLen = 32

// clusterView is what everyone may know of a cluster.
type clusterView struct {
	Name         string     `json:"name"`
	DisplayName  string     `json:"displayName,omitempty"`
	Connected    bool       `json:"connected"`
	Paused       bool       `json:"paused,omitempty"`
	LastSeen     *time.Time `json:"lastSeen,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
}

// adminClusterView adds what an admin manages.
type adminClusterView struct {
	clusterView
	Joined            bool                      `json:"joined"`
	TokenExpires      *time.Time                `json:"tokenExpires,omitempty"`
	AgentVersion      string                    `json:"agentVersion,omitempty"`
	ControllerVersion string                    `json:"controllerVersion,omitempty"`
	KubernetesVersion string                    `json:"kubernetesVersion,omitempty"`
	DefaultQuota      *devpodv1alpha1.UserQuota `json:"defaultQuota,omitempty"`
	CreatedAt         time.Time                 `json:"createdAt"`
}

func (s *Server) clusterView(c *devpodv1alpha1.Cluster) clusterView {
	v := clusterView{
		Name: c.Name, DisplayName: c.Spec.DisplayName, Paused: c.Spec.Paused,
		Connected: c.Status.Connected && !c.Spec.Paused, Capabilities: c.Status.Capabilities,
	}
	if c.Status.LastSeen != nil {
		t := c.Status.LastSeen.Time
		v.LastSeen = &t
	}
	// what this instance holds right now is fresher than the object
	if here, _ := s.Clusters.Connected(c.Name); here {
		now := time.Now().Truncate(time.Second)
		v.Connected, v.LastSeen = true, &now
		if in, ok := s.Clusters.Info(c.Name); ok {
			v.Capabilities = in.Capabilities
		}
	}
	return v
}

func (s *Server) adminClusterView(c *devpodv1alpha1.Cluster) adminClusterView {
	v := adminClusterView{
		clusterView: s.clusterView(c), Joined: c.Spec.AgentPublicKey != "",
		AgentVersion: c.Status.AgentVersion, ControllerVersion: c.Status.ControllerVersion,
		KubernetesVersion: c.Status.KubernetesVersion, DefaultQuota: c.Spec.DefaultQuota,
		CreatedAt: c.CreationTimestamp.Time,
	}
	if in, ok := s.Clusters.Info(c.Name); ok {
		v.AgentVersion, v.ControllerVersion, v.KubernetesVersion = in.AgentVersion, in.ControllerVersion, in.KubernetesVersion
	}
	if !v.Joined {
		if t, err := time.Parse(time.RFC3339, c.Annotations[clusters.TokenExpiresAnnotation]); err == nil {
			v.TokenExpires = &t
		}
	}
	return v
}

func (s *Server) listClusters(r *http.Request) ([]devpodv1alpha1.Cluster, error) {
	var list devpodv1alpha1.ClusterList
	if err := s.Reader.List(r.Context(), &list); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

// handleClusters: GET /api/clusters.
func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFrom(w, r); !ok {
		return
	}
	items := []clusterView{}
	if s.Clusters != nil {
		list, err := s.listClusters(r)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		for i := range list {
			items = append(items, s.clusterView(&list[i]))
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// clusterAdmin gates the admin routes: an admin, and a console that
// manages clusters.
func (s *Server) clusterAdmin(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.requireAdmin(w, r); !ok {
		return false
	}
	if s.Clusters == nil {
		s.writeErr(w, http.StatusNotFound, "NOT_ENABLED", "this console does not manage other clusters", nil)
		return false
	}
	return true
}

// handleAdminClusters: GET /api/admin/clusters.
func (s *Server) handleAdminClusters(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	list, err := s.listClusters(r)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := make([]adminClusterView, 0, len(list))
	for i := range list {
		items = append(items, s.adminClusterView(&list[i]))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// joinCommand is what the admin runs in the new cluster.
func (s *Server) joinCommand(cluster, token string) string {
	console := s.Origin
	if console == "" {
		console = "<console address>"
	}
	return fmt.Sprintf("helm upgrade --install devpod <chart> -n devpod-system --create-namespace \\\n"+
		"  --set agent.enabled=true --set agent.cluster=%s \\\n  --set agent.console=%s \\\n  --set agent.joinToken=%s",
		cluster, console, token)
}

// handleCreateCluster: POST /api/admin/clusters.
func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName,omitempty"`
	}
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if req.Name == "devpod" || len(req.Name) > MaxClusterNameLen || !dns1123Label.MatchString(req.Name) {
		s.writeServerErr(w, badRequest("a cluster name is 1 to %d lower-case letters, digits and hyphens, and cannot be \"devpod\"", MaxClusterNameLen))
		return
	}
	c := &devpodv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       devpodv1alpha1.ClusterSpec{DisplayName: req.DisplayName},
	}
	token := clusters.IssueToken(c, time.Now())
	if err := s.Client.Create(r.Context(), c); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeErr(w, http.StatusConflict, "ALREADY_EXISTS", fmt.Sprintf("a cluster named %q is already registered", req.Name), nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("register", err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"cluster": s.adminClusterView(c), "joinToken": token, "command": s.joinCommand(c.Name, token),
	})
}

// editCluster changes a Cluster, again if its status was written in
// between. A missing cluster is answered here.
func (s *Server) editCluster(w http.ResponseWriter, r *http.Request, edit func(*devpodv1alpha1.Cluster)) (*devpodv1alpha1.Cluster, bool) {
	var c devpodv1alpha1.Cluster
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name")}, &c); err != nil {
			return err
		}
		edit(&c)
		return s.Client.Update(r.Context(), &c)
	})
	switch {
	case apierrors.IsNotFound(err):
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such cluster", nil)
		return nil, false
	case err != nil:
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("update", err), nil)
		return nil, false
	}
	return &c, true
}

// handleClusterToken: POST /api/admin/clusters/{name}/token — a new join
// token. The agent that had joined is no longer accepted.
func (s *Server) handleClusterToken(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	var token string
	c, ok := s.editCluster(w, r, func(c *devpodv1alpha1.Cluster) { token = clusters.IssueToken(c, time.Now()) })
	if !ok {
		return
	}
	s.Clusters.Disconnect(c.Name)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"cluster": s.adminClusterView(c), "joinToken": token, "command": s.joinCommand(c.Name, token),
	})
}

// handlePatchCluster: PATCH /api/admin/clusters/{name}.
func (s *Server) handlePatchCluster(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	var req struct {
		DisplayName  *string     `json:"displayName,omitempty"`
		Paused       *bool       `json:"paused,omitempty"`
		DefaultQuota *quotaPatch `json:"defaultQuota,omitempty"`
	}
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	var quota *devpodv1alpha1.UserQuota
	if req.DefaultQuota != nil {
		q, err := req.DefaultQuota.build()
		if err != nil {
			s.writeServerErr(w, badRequest("%s", err.Error()))
			return
		}
		quota = q
	}
	c, ok := s.editCluster(w, r, func(c *devpodv1alpha1.Cluster) {
		if req.DisplayName != nil {
			c.Spec.DisplayName = *req.DisplayName
		}
		if req.Paused != nil {
			c.Spec.Paused = *req.Paused
		}
		if req.DefaultQuota != nil {
			c.Spec.DefaultQuota = quota.DeepCopy() // empty = none of its own
		}
	})
	if !ok {
		return
	}
	if c.Spec.Paused {
		s.Clusters.Disconnect(c.Name)
	}
	s.writeJSON(w, http.StatusOK, s.adminClusterView(c))
}

// handleDeleteCluster: DELETE /api/admin/clusters/{name}. The cluster's
// DevPods are not touched: it is only no longer managed from here.
func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	err := s.Client.Delete(r.Context(), &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}})
	switch {
	case apierrors.IsNotFound(err):
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such cluster", nil)
		return
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.Clusters.Disconnect(name)
	w.WriteHeader(http.StatusNoContent)
}

/* ---- the agents' side: no session, the token or the key is the credential ---- */

// handleAgentJoin: POST /agent/join.
func (s *Server) handleAgentJoin(w http.ResponseWriter, r *http.Request) {
	if s.Clusters == nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Cluster   string `json:"cluster"`
		Token     string `json:"token"`
		PublicKey string `json:"publicKey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body", nil)
		return
	}
	switch err := s.Clusters.Join(r.Context(), req.Cluster, req.Token, req.PublicKey); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, clusters.ErrJoinRefused):
		// which of cluster, token and key was wrong is not said
		s.writeErr(w, http.StatusForbidden, "JOIN_REFUSED", "the join token is wrong, used or expired, or the cluster already has an agent; ask an admin for a new token", nil)
	default:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	}
}

// handleAgentConnect: GET /agent/connect — one connection of an agent,
// control or data, as a WebSocket. Who it is is settled in the tunnel's
// handshake, on the connection itself.
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	if s.Clusters == nil {
		http.NotFound(w, r)
		return
	}
	// an agent is not a browser: a request with an Origin from elsewhere
	// is refused by Accept, and no cookie is looked at anyway
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept wrote the response
	}
	c.SetReadLimit(-1) // a stream carries whatever its two ends say
	conn := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
	s.Clusters.Accept(r.Context(), conn)
}
