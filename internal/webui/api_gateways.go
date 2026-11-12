// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"strings"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gwlink"
	"github.com/mrhaoxx/devpod/internal/httpauth"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// Gateways of a console of several clusters. A gateway in a cluster
// reads that cluster; one that serves several has no cluster to read, so
// it asks the console — which DevPod a login or a port-link address
// means, a user's keys — with requests signed by the key it joined with
// (the same one-time-token flow as a cluster's agent). The gateway dials
// the console; the console never dials a gateway.

type gatewayView struct {
	Name           string `json:"name"`
	DisplayName    string `json:"displayName,omitempty"`
	SSHAddress     string `json:"sshAddress,omitempty"`
	HTTPBaseDomain string `json:"httpBaseDomain,omitempty"`
	HTTPSuffix     string `json:"httpSuffix,omitempty"`
	HTTPPort       int32  `json:"httpPort,omitempty"`
	Paused         bool   `json:"paused,omitempty"`
}

type adminGatewayView struct {
	gatewayView
	AgentAddress string     `json:"agentAddress,omitempty"`
	Joined       bool       `json:"joined"`
	TokenExpires *time.Time `json:"tokenExpires,omitempty"`
	LastSeen     *time.Time `json:"lastSeen,omitempty"`
	Version      string     `json:"version,omitempty"`
	Clusters     []string   `json:"clusters,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func viewGateway(g *devpodv1alpha1.Gateway) gatewayView {
	return gatewayView{Name: g.Name, DisplayName: g.Spec.DisplayName, SSHAddress: g.Spec.SSHAddress,
		HTTPBaseDomain: g.Spec.HTTPBaseDomain, HTTPSuffix: g.Spec.HTTPSuffix, HTTPPort: g.Spec.HTTPPort, Paused: g.Spec.Paused}
}

func viewAdminGateway(g *devpodv1alpha1.Gateway) adminGatewayView {
	v := adminGatewayView{gatewayView: viewGateway(g), AgentAddress: g.Spec.AgentAddress, Joined: g.Spec.PublicKey != "",
		Version: g.Status.Version, Clusters: g.Status.Clusters, CreatedAt: g.CreationTimestamp.Time}
	if g.Status.LastSeen != nil {
		t := g.Status.LastSeen.Time
		v.LastSeen = &t
	}
	if !v.Joined {
		if t, err := time.Parse(time.RFC3339, g.Annotations[clusters.TokenExpiresAnnotation]); err == nil {
			v.TokenExpires = &t
		}
	}
	return v
}

func (s *Server) listGateways(ctx context.Context) ([]devpodv1alpha1.Gateway, error) {
	var list devpodv1alpha1.GatewayList
	if err := s.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

// handleGateways: GET /api/gateways — where to connect.
func (s *Server) handleGateways(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFrom(w, r); !ok {
		return
	}
	items := []gatewayView{}
	if s.Clusters != nil {
		list, err := s.listGateways(r.Context())
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		for i := range list {
			items = append(items, viewGateway(&list[i]))
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleAdminGateways: GET /api/admin/gateways.
func (s *Server) handleAdminGateways(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	list, err := s.listGateways(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := make([]adminGatewayView, 0, len(list))
	for i := range list {
		items = append(items, viewAdminGateway(&list[i]))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// gatewayJoinCommand is what the admin runs where the gateway is installed.
func (s *Server) gatewayJoinCommand(name, token string) string {
	console := s.Origin
	if console == "" {
		console = "<console address>"
	}
	return fmt.Sprintf("helm upgrade --install devpod <chart> -n devpod-system --create-namespace \\\n"+
		"  --set gateway.hub.enabled=true --set gateway.hub.name=%s \\\n  --set gateway.hub.console=%s \\\n  --set gateway.hub.joinToken=%s",
		name, console, token)
}

// gatewayFields are what an admin sets on a gateway.
type gatewayFields struct {
	DisplayName    *string `json:"displayName,omitempty"`
	SSHAddress     *string `json:"sshAddress,omitempty"`
	HTTPBaseDomain *string `json:"httpBaseDomain,omitempty"`
	HTTPSuffix     *string `json:"httpSuffix,omitempty"`
	HTTPPort       *int32  `json:"httpPort,omitempty"`
	AgentAddress   *string `json:"agentAddress,omitempty"`
	Paused         *bool   `json:"paused,omitempty"`
}

func (f *gatewayFields) apply(g *devpodv1alpha1.Gateway) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&g.Spec.DisplayName, f.DisplayName)
	set(&g.Spec.SSHAddress, f.SSHAddress)
	set(&g.Spec.HTTPBaseDomain, f.HTTPBaseDomain)
	set(&g.Spec.HTTPSuffix, f.HTTPSuffix)
	set(&g.Spec.AgentAddress, f.AgentAddress)
	if f.HTTPPort != nil {
		g.Spec.HTTPPort = *f.HTTPPort
	}
	if f.Paused != nil {
		g.Spec.Paused = *f.Paused
	}
}

// handleCreateGateway: POST /api/admin/gateways.
func (s *Server) handleCreateGateway(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
		gatewayFields
	}
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if len(req.Name) > MaxClusterNameLen || !dns1123Label.MatchString(req.Name) {
		s.writeServerErr(w, badRequest("a gateway name is 1 to %d lower-case letters, digits and hyphens", MaxClusterNameLen))
		return
	}
	g := &devpodv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: req.Name}}
	req.gatewayFields.apply(g)
	token := clusters.IssueGatewayToken(g, time.Now())
	if err := s.Client.Create(r.Context(), g); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeErr(w, http.StatusConflict, "ALREADY_EXISTS", fmt.Sprintf("a gateway named %q is already registered", req.Name), nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("register", err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"gateway": viewAdminGateway(g), "joinToken": token, "command": s.gatewayJoinCommand(g.Name, token)})
}

func (s *Server) editGateway(w http.ResponseWriter, r *http.Request, edit func(*devpodv1alpha1.Gateway)) (*devpodv1alpha1.Gateway, bool) {
	var g devpodv1alpha1.Gateway
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name")}, &g); err != nil {
			return err
		}
		edit(&g)
		return s.Client.Update(r.Context(), &g)
	})
	switch {
	case apierrors.IsNotFound(err):
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such gateway", nil)
		return nil, false
	case err != nil:
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("update", err), nil)
		return nil, false
	}
	return &g, true
}

// handlePatchGateway: PATCH /api/admin/gateways/{name}.
func (s *Server) handlePatchGateway(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	var req gatewayFields
	if apiErr := decodeStrict(r, &req); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if g, ok := s.editGateway(w, r, req.apply); ok {
		s.writeJSON(w, http.StatusOK, viewAdminGateway(g))
	}
}

// handleGatewayToken: POST /api/admin/gateways/{name}/token.
func (s *Server) handleGatewayToken(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	var token string
	g, ok := s.editGateway(w, r, func(g *devpodv1alpha1.Gateway) { token = clusters.IssueGatewayToken(g, time.Now()) })
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"gateway": viewAdminGateway(g), "joinToken": token, "command": s.gatewayJoinCommand(g.Name, token)})
}

// handleDeleteGateway: DELETE /api/admin/gateways/{name}.
func (s *Server) handleDeleteGateway(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAdmin(w, r) {
		return
	}
	err := s.Client.Delete(r.Context(), &devpodv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("name")}})
	switch {
	case apierrors.IsNotFound(err):
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such gateway", nil)
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

/* ---- the gateways' side: no session; the token, then the key ---- */

// handleGatewayJoin: POST /gateway/join.
func (s *Server) handleGatewayJoin(w http.ResponseWriter, r *http.Request) {
	if s.Clusters == nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Gateway   string `json:"gateway"`
		Token     string `json:"token"`
		PublicKey string `json:"publicKey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body", nil)
		return
	}
	switch err := s.Clusters.JoinGateway(r.Context(), req.Gateway, req.Token, req.PublicKey); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, clusters.ErrJoinRefused):
		s.writeErr(w, http.StatusForbidden, "JOIN_REFUSED", "the join token is wrong, used or expired, or the gateway has already joined; ask an admin for a new token", nil)
	default:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	}
}

// asGateway authenticates a signed request of a gateway and returns its
// name and the request's body. One answer for every way of not being a
// gateway in good standing.
func (s *Server) asGateway(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	if s.Clusters == nil {
		http.NotFound(w, r)
		return "", nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "request too large", nil)
		return "", nil, false
	}
	name, err := gwlink.Verify(r, body, func(name string) (string, error) {
		var g devpodv1alpha1.Gateway
		if len(name) > MaxClusterNameLen || !dns1123Label.MatchString(name) {
			return "", errors.New("no such gateway")
		}
		if err := s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &g); err != nil || g.Spec.Paused || g.Spec.PublicKey == "" {
			return "", errors.New("no such gateway")
		}
		return g.Spec.PublicKey, nil
	}, time.Now())
	if err != nil {
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not a gateway this console knows", nil)
		return "", nil, false
	}
	s.gatewaySeen(name)
	return name, body, true
}

// gatewaySeen notes that a gateway asked something; written to its
// status at most every half minute.
var gatewaySeenMu sync.Mutex

func (s *Server) gatewaySeen(name string) {
	gatewaySeenMu.Lock()
	if s.gatewaysSeen == nil {
		s.gatewaysSeen = map[string]time.Time{}
	}
	due := time.Since(s.gatewaysSeen[name]) > 30*time.Second
	if due {
		s.gatewaysSeen[name] = time.Now()
	}
	gatewaySeenMu.Unlock()
	if due {
		go s.writeGatewayStatus(name, nil)
	}
}

func (s *Server) writeGatewayStatus(name string, report *gwlink.Report) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var g devpodv1alpha1.Gateway
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name}, &g); err != nil {
			return client.IgnoreNotFound(err)
		}
		now := metav1.NewTime(time.Now().Truncate(time.Second))
		g.Status.LastSeen = &now
		if report != nil {
			g.Status.Version = report.Version
			g.Status.Clusters = slices.Compact(slices.Sorted(slices.Values(report.Clusters)))
			// a key, and nothing else, becomes what DevPods trust; a
			// gateway of an earlier version says none and leaves it be
			if k, ok := authorizedKeyLine(report.InternalKey); ok {
				g.Status.InternalKey = k
			}
		}
		return s.Client.Status().Update(ctx, &g)
	})
}

// eachCluster calls fn for the clusters a gateway's question covers:
// those whose agent is connected. What the console holds of a cluster
// that is away (or paused) is its last known state, and nobody is let in
// on the strength of that.
func (s *Server) eachCluster(cluster string, fn func(*clusters.ClusterClients)) {
	s.Clusters.Each(func(cc *clusters.ClusterClients) {
		if _, stale := s.Clusters.Stale(cc.Name); stale {
			return
		}
		if cluster == "" || cluster == cc.Name {
			fn(cc)
		}
	})
}

// forGateway is a DevPod as a gateway needs it: which it is, who may log
// in, whether and where it runs, and which of its ports are open to
// whom. Not its Pod spec — images, environment, volumes are none of a
// gateway's business.
func forGateway(dp *devpodv1alpha1.DevPod) *devpodv1alpha1.DevPod {
	out := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: dp.Name, Namespace: dp.Namespace, UID: dp.UID},
		Spec:       devpodv1alpha1.DevPodSpec{Owner: dp.Spec.Owner, Collaborators: slices.Clone(dp.Spec.Collaborators), Running: dp.Spec.Running},
		Status:     devpodv1alpha1.DevPodStatus{Phase: dp.Status.Phase, Endpoint: dp.Status.Endpoint},
	}
	for _, k := range []string{httpauth.IDAnnotation, httpauth.PublicPortsAnnotation, httpauth.UserPortsAnnotation} {
		if v, ok := dp.Annotations[k]; ok {
			if out.Annotations == nil {
				out.Annotations = map[string]string{}
			}
			out.Annotations[k] = v
		}
	}
	return out
}

// handleGatewayLookup: POST /gateway/v1/lookup — the DevPods a login
// could mean: "<user>-<pod>", else the bare "<pod>", in each cluster.
func (s *Server) handleGatewayLookup(w http.ResponseWriter, r *http.Request) {
	_, body, ok := s.asGateway(w, r)
	if !ok {
		return
	}
	var q gwlink.Lookup
	if err := json.Unmarshal(body, &q); err != nil || !dns1123Label.MatchString(q.User) || !dns1123Label.MatchString(q.Pod) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed lookup", nil)
		return
	}
	out := gwlink.Founds{Found: []gwlink.Found{}}
	var failed error
	s.eachCluster(q.Cluster, func(cc *clusters.ClusterClients) {
		var dp devpodv1alpha1.DevPod
		err := cc.Client.Get(r.Context(), types.NamespacedName{Name: q.User + "-" + q.Pod, Namespace: s.NS}, &dp)
		if apierrors.IsNotFound(err) {
			err = cc.Client.Get(r.Context(), types.NamespacedName{Name: q.Pod, Namespace: s.NS}, &dp)
		}
		switch {
		case err == nil:
			out.Found = append(out.Found, gwlink.Found{Cluster: cc.Name, DevPod: forGateway(&dp)})
		case !apierrors.IsNotFound(err):
			failed = err
		}
	})
	// a cluster that cannot be read fails the question about that cluster,
	// not the logins to the others
	if failed != nil && q.Cluster != "" {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", failed.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleGatewayHTTP: GET /gateway/v1/http?id= — the DevPod a port-link
// id belongs to.
func (s *Server) handleGatewayHTTP(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.asGateway(w, r); !ok {
		return
	}
	id := r.URL.Query().Get("id")
	var found *gwlink.Found
	if httpauth.ValidID(id) {
		s.eachCluster("", func(cc *clusters.ClusterClients) {
			var list devpodv1alpha1.DevPodList
			if found != nil || cc.Client.List(r.Context(), &list, client.InNamespace(s.NS), client.UnsafeDisableDeepCopy) != nil {
				return
			}
			for i := range list.Items {
				if list.Items[i].Annotations[httpauth.IDAnnotation] == id {
					found = &gwlink.Found{Cluster: cc.Name, DevPod: forGateway(&list.Items[i])}
					return
				}
			}
		})
	}
	if found == nil {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no DevPod has this id", nil)
		return
	}
	s.writeJSON(w, http.StatusOK, found)
}

// handleGatewayKeys: GET /gateway/v1/keys?user= — the user's SSH keys
// registered at the console, and nothing else of the user.
func (s *Server) handleGatewayKeys(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.asGateway(w, r); !ok {
		return
	}
	var u devpodv1alpha1.User
	name := r.URL.Query().Get("user")
	if !dns1123Label.MatchString(name) || s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &u) != nil {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such user", nil)
		return
	}
	keys := u.Spec.Pubkeys
	if keys == nil {
		keys = []string{}
	}
	s.writeJSON(w, http.StatusOK, gwlink.Keys{Keys: keys})
}

// handleGatewayClusters: GET /gateway/v1/clusters — whose agents the
// gateway accepts, each with the key to check it by.
func (s *Server) handleGatewayClusters(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.asGateway(w, r); !ok {
		return
	}
	var list devpodv1alpha1.ClusterList
	if err := s.Client.List(r.Context(), &list); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	out := gwlink.Clusters{Clusters: []gwlink.Cluster{}}
	for i := range list.Items {
		if c := &list.Items[i]; !c.Spec.Paused && c.Spec.AgentPublicKey != "" {
			out.Clusters = append(out.Clusters, gwlink.Cluster{Name: c.Name, AgentPublicKey: c.Spec.AgentPublicKey})
		}
	}
	sort.Slice(out.Clusters, func(i, j int) bool { return out.Clusters[i].Name < out.Clusters[j].Name })
	s.writeJSON(w, http.StatusOK, out)
}

// handleGatewayReport: POST /gateway/v1/report.
func (s *Server) handleGatewayReport(w http.ResponseWriter, r *http.Request) {
	name, body, ok := s.asGateway(w, r)
	if !ok {
		return
	}
	var rep gwlink.Report
	if err := json.Unmarshal(body, &rep); err != nil || len(rep.Clusters) > 1024 {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed report", nil)
		return
	}
	s.writeGatewayStatus(name, &rep)
	w.WriteHeader(http.StatusNoContent)
}

// gatewayOf is the gateway that serves a cluster: the one the cluster
// names, else the only one there is; nil when there is none to say (the
// console's own settings then stand).
func (s *Server) gatewayOf(ctx context.Context, cluster string) *devpodv1alpha1.Gateway {
	if s.Clusters == nil || cluster == "" {
		return nil
	}
	var c devpodv1alpha1.Cluster
	if err := s.Client.Get(ctx, types.NamespacedName{Name: cluster}, &c); err == nil && c.Spec.Gateway != "" {
		var g devpodv1alpha1.Gateway
		if err := s.Client.Get(ctx, types.NamespacedName{Name: c.Spec.Gateway}, &g); err == nil {
			return &g
		}
		return nil
	}
	var list devpodv1alpha1.GatewayList
	if err := s.Client.List(ctx, &list); err == nil && len(list.Items) == 1 {
		return &list.Items[0]
	}
	return nil
}

// sshOf is where users connect by SSH for a cluster's DevPods, and the
// cluster to write in the login (a gateway of several clusters takes
// "<user>+<pod>+<cluster>"); nil when the console's own settings stand.
func (s *Server) sshOf(ctx context.Context, cluster string) map[string]any {
	g := s.gatewayOf(ctx, cluster)
	if g == nil || g.Spec.SSHAddress == "" {
		return nil
	}
	host, port := g.Spec.SSHAddress, 22
	if h, p, err := net.SplitHostPort(g.Spec.SSHAddress); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			host, port = h, n
		}
	}
	return map[string]any{"host": host, "port": port, "loginSuffix": cluster}
}

// proxyOfGateway is the port-link address scheme of a gateway (signed
// with the console's key, which every gateway shares); nil if it serves
// no port links.
func (s *Server) proxyOfGateway(g *devpodv1alpha1.Gateway) *HTTPProxy {
	if g == nil || g.Spec.HTTPBaseDomain == "" || s.HTTPProxy == nil {
		return nil
	}
	h, err := NewHTTPProxy("{id}-{port}"+g.Spec.HTTPSuffix+"."+g.Spec.HTTPBaseDomain, s.HTTPProxy.Scheme, s.HTTPProxy.Key)
	if err != nil {
		return nil
	}
	h.Port = int(g.Spec.HTTPPort)
	return h
}

// httpProxies are every port-link address scheme there is: the console's
// own and each gateway's.
func (s *Server) httpProxies(ctx context.Context) []*HTTPProxy {
	out := []*HTTPProxy{s.HTTPProxy}
	if s.Clusters != nil {
		var list devpodv1alpha1.GatewayList
		if err := s.Client.List(ctx, &list); err == nil {
			for i := range list.Items {
				if h := s.proxyOfGateway(&list.Items[i]); h != nil {
					out = append(out, h)
				}
			}
		}
	}
	return out
}

// loginCluster is the cluster to write in a login for the request's
// DevPod: its name where a gateway of several clusters serves it, nothing
// where the cluster's own gateway does (that one knows no cluster names).
func (s *Server) loginCluster(r *http.Request) string {
	if s.Clusters == nil {
		return ""
	}
	if name := s.k(r).Name; s.gatewayOf(r.Context(), name) != nil {
		return name
	}
	return ""
}

// authorizedKeyLine: k as one authorized_keys line (key type and key, no
// options), if it is a single SSH public key.
func authorizedKeyLine(k string) (string, bool) {
	if k == "" || len(k) > 4096 {
		return "", false
	}
	pub, _, opts, rest, err := ssh.ParseAuthorizedKey([]byte(k))
	if err != nil || len(opts) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
		return "", false
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), true
}
