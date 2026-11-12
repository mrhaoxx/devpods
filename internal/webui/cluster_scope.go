// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// Which cluster a request is about.
//
// A console of several clusters keeps users, configs and the clusters
// themselves in its own cluster (s.Client, s.Reader) and everything else
// — DevPods, Pods, nodes, reservations, snapshots — in the cluster it
// belongs to, reached through that cluster's agent. A request says which:
//
//   - /api/clusters/{cluster}/<path> is <path> in that cluster;
//   - ?cluster= says the same on the plain path;
//   - a path naming a DevPod finds it among the caller's DevPods of every
//     cluster: one match is used, several are 409 AMBIGUOUS_CLUSTER;
//   - with one cluster registered, that is the one;
//   - lists without any of these cover every cluster, each item saying
//     which it is from.
//
// Anything else that needs a cluster and was not told one is answered
// 400 CLUSTER_REQUIRED. Without s.Clusters the console is that of the
// cluster it runs in, and none of this applies.

// StaleHeader marks an answer read from the cache of a cluster whose
// agent is away: the moment it went.
const StaleHeader = "X-DevPod-Stale-Since"

type scopeKey struct{}

// reqScope is a request's cluster, once known.
type reqScope struct {
	cc *clusters.ClusterClients
	// only, when set: the clusters the caller's token reaches.
	only []string
}

// errClusterRequired is what k panics with; withCluster answers it.
var errClusterRequired = errors.New("cluster required")

// local is the cluster the console runs in, as a cluster.
func (s *Server) local() *clusters.ClusterClients {
	return &clusters.ClusterClients{Client: s.Client, Reader: s.Reader, Cache: s.Cache}
}

// k is the request's cluster. In a console of several clusters a request
// that did not say which ends here, answered 400 by withCluster.
func (s *Server) k(r *http.Request) *clusters.ClusterClients { return s.kc(r.Context()) }

// kc is k for code that holds the request's context.
func (s *Server) kc(ctx context.Context) *clusters.ClusterClients {
	if s.Clusters == nil {
		return s.local()
	}
	if sc, _ := ctx.Value(scopeKey{}).(*reqScope); sc != nil && sc.cc != nil {
		return sc.cc
	}
	panic(errClusterRequired)
}

// in is ctx with cc as its cluster: for work done cluster by cluster.
func (s *Server) in(ctx context.Context, cc *clusters.ClusterClients) context.Context {
	if s.Clusters == nil {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, &reqScope{cc: cc})
}

// ks are the clusters a list covers: the request's, or all of them.
func (s *Server) ks(r *http.Request) []*clusters.ClusterClients { return s.ksc(r.Context()) }

// ksc is ks for code that holds the request's context.
func (s *Server) ksc(ctx context.Context) []*clusters.ClusterClients {
	if s.Clusters == nil {
		return []*clusters.ClusterClients{s.local()}
	}
	if sc, _ := ctx.Value(scopeKey{}).(*reqScope); sc != nil && sc.cc != nil {
		return []*clusters.ClusterClients{sc.cc}
	}
	sc, _ := ctx.Value(scopeKey{}).(*reqScope)
	var all []*clusters.ClusterClients
	s.Clusters.Each(func(cc *clusters.ClusterClients) {
		if sc == nil || len(sc.only) == 0 || slices.Contains(sc.only, cc.Name) {
			all = append(all, cc)
		}
	})
	return all
}

// limitClusters holds a request to the clusters its token names: one
// that is about another cluster is answered as if that cluster did not
// exist, and what covers every cluster covers these.
func (s *Server) limitClusters(w http.ResponseWriter, r *http.Request, only []string) bool {
	if s.Clusters == nil || len(only) == 0 {
		return true
	}
	sc, _ := r.Context().Value(scopeKey{}).(*reqScope)
	if sc == nil {
		return true
	}
	if sc.cc != nil && !slices.Contains(only, sc.cc.Name) {
		s.writeErr(w, http.StatusNotFound, "UNKNOWN_CLUSTER", "no cluster named "+sc.cc.Name, nil)
		return false
	}
	sc.only = only
	return true
}

// staleOf reports the clusters among ccs whose agent is away, and marks
// the answer with the earliest of those moments.
func (s *Server) staleOf(w http.ResponseWriter, ccs []*clusters.ClusterClients) map[string]time.Time {
	if s.Clusters == nil {
		return nil
	}
	var out map[string]time.Time
	var first time.Time
	for _, cc := range ccs {
		if since, stale := s.Clusters.Stale(cc.Name); stale {
			if out == nil {
				out = map[string]time.Time{}
			}
			out[cc.Name] = since
			if first.IsZero() || since.Before(first) {
				first = since
			}
		}
	}
	if out != nil {
		w.Header().Set(StaleHeader, first.UTC().Format(time.RFC3339))
	}
	return out
}

// settle makes cc the request's cluster. A change to a cluster whose
// agent is away is refused here; a read of it is marked stale.
func (s *Server) settle(w http.ResponseWriter, r *http.Request, sc *reqScope, cc *clusters.ClusterClients) bool {
	if since, stale := s.Clusters.Stale(cc.Name); stale {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.clusterOffline(w, cc.Name)
			return false
		}
		w.Header().Set(StaleHeader, since.UTC().Format(time.RFC3339))
	}
	sc.cc = cc
	return true
}

func (s *Server) clusterOffline(w http.ResponseWriter, name string) {
	s.writeErr(w, http.StatusServiceUnavailable, "CLUSTER_OFFLINE",
		"cluster "+name+" is not connected: what is shown of it is the last known state, and it cannot be changed until its agent is back", nil)
}

// named resolves a cluster the request named.
func (s *Server) named(w http.ResponseWriter, name string) (*clusters.ClusterClients, bool) {
	cc, err := s.Clusters.Clients(name)
	switch {
	case errors.Is(err, clusters.ErrUnknownCluster):
		s.writeErr(w, http.StatusNotFound, "UNKNOWN_CLUSTER", "no cluster named "+name, nil)
	case errors.Is(err, clusters.ErrClusterOffline):
		s.clusterOffline(w, name)
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	default:
		return cc, true
	}
	return nil, false
}

// clusterNames lists the registered clusters, from the console's cache.
func (s *Server) clusterNames(ctx context.Context) []string {
	var list devpodv1alpha1.ClusterList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names
}

// withCluster settles the cluster of every API request that names one,
// and answers those that needed one and named none.
func (s *Server) withCluster(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		// /api/clusters/{cluster}/<path> → /api/<path>, in that cluster
		name := ""
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/clusters/"); ok {
			if c, path, ok := strings.Cut(rest, "/"); ok && c != "" && path != "" {
				name = c
				r2 := r.Clone(r.Context())
				r2.URL.Path, r2.URL.RawPath = "/api/"+path, ""
				r = r2
			}
		}
		if s.Clusters == nil {
			if name != "" {
				s.writeErr(w, http.StatusNotFound, "UNKNOWN_CLUSTER", "this console manages only the cluster it runs in", nil)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if name == "" {
			name = r.URL.Query().Get("cluster")
		}
		if name == "" {
			if names := s.clusterNames(r.Context()); len(names) == 1 {
				// the only cluster there is; if it never connected, whoever
				// needs it is told so by k
				if cc, err := s.Clusters.Clients(names[0]); err == nil {
					name = cc.Name
				}
			}
		}
		sc := &reqScope{}
		if name != "" {
			cc, ok := s.named(w, name)
			if !ok || !s.settle(w, r, sc, cc) {
				return
			}
		}
		defer func() {
			if p := recover(); p != nil {
				if p != errClusterRequired {
					panic(p)
				}
				s.writeErr(w, http.StatusBadRequest, "CLUSTER_REQUIRED",
					"say which cluster: /api/clusters/{cluster}/… or ?cluster=", s.clusterNames(r.Context()))
			}
		}()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopeKey{}, sc)))
	})
}

// clusterPrefix labels a message with its cluster, when there is one.
func clusterPrefix(name string) string {
	if name == "" {
		return ""
	}
	return name + ": "
}

// Candidate is one of several DevPods a name could mean.
type Candidate struct {
	Cluster string `json:"cluster"`
	Name    string `json:"name"`
}

// findDevPod is getOwned's lookup: the DevPod of that name the caller may
// see. With the request's cluster settled it is looked up there;
// otherwise in every cluster, settling the request on the one that has
// it. The response is written when ok is false.
func (s *Server) findDevPod(w http.ResponseWriter, r *http.Request, sess Session, name string) (*devpodv1alpha1.DevPod, bool) {
	notFound := func() (*devpodv1alpha1.DevPod, bool) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "devpod not found", nil)
		return nil, false
	}
	if !sess.AllowsDevPod(name) {
		return notFound()
	}
	visible := func(cc *clusters.ClusterClients) (*devpodv1alpha1.DevPod, error) {
		var dp devpodv1alpha1.DevPod
		err := cc.Client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.NS}, &dp)
		if apierrors.IsNotFound(err) || (err == nil && dp.Spec.Owner != sess.User && !sess.Admin) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &dp, nil
	}
	sc, _ := r.Context().Value(scopeKey{}).(*reqScope)
	if s.Clusters == nil || sc == nil || sc.cc != nil {
		dp, err := visible(s.k(r))
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return nil, false
		}
		if dp == nil {
			return notFound()
		}
		return dp, true
	}
	var (
		found *devpodv1alpha1.DevPod
		in    *clusters.ClusterClients
		cands []Candidate
	)
	for _, cc := range s.ks(r) {
		dp, err := visible(cc)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return nil, false
		}
		if dp != nil {
			found, in = dp, cc
			cands = append(cands, Candidate{Cluster: cc.Name, Name: name})
		}
	}
	switch len(cands) {
	case 0:
		return notFound()
	case 1:
		if !s.settle(w, r, sc, in) {
			return nil, false
		}
		return found, true
	}
	s.writeErr(w, http.StatusConflict, "AMBIGUOUS_CLUSTER",
		"several clusters have a DevPod named "+name+": say which with /api/clusters/{cluster}/…", cands)
	return nil, false
}

// InCluster is a DevPod with the cluster it is in (empty in a console of
// one cluster).
type InCluster struct {
	devpodv1alpha1.DevPod `json:",inline"`
	Cluster               string `json:"cluster,omitempty"`
}

// clusterQuota is what a cluster allows a user who has no quota of their
// own for it: the cluster's default, else the console's.
func (s *Server) clusterQuota(ctx context.Context, cc *clusters.ClusterClients) devpodv1alpha1.UserQuota {
	if s.Clusters != nil && cc.Name != "" {
		var c devpodv1alpha1.Cluster
		if err := s.Client.Get(ctx, types.NamespacedName{Name: cc.Name}, &c); err == nil && c.Spec.DefaultQuota != nil {
			return *c.Spec.DefaultQuota
		}
	}
	return s.DefaultQuota
}
