// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

// Several clusters. A console may run DevPods in several clusters; a
// DevPod, a snapshot or a reservation is in one of them. A name or id
// found in more than one says which as <cluster>/<name> — as the console's
// own addresses do (/devpods/<cluster>/<name>) — and a command that makes
// something, or lists, takes --cluster. `clusters` lists them.

const clustersHelp = `With several clusters, a DevPod (snapshot, reservation) is in one of them.
A name found in more than one is given as <cluster>/<name>, e.g. st2/code;
--cluster C says the same for every name of the command, and picks where
create, config launch and reserve add make things, and what lists show.
Lists show a CLUSTER column. ` + "`clusters`" + ` lists the clusters.`

// splitRef reads "<cluster>/<name>"; a plain name has no cluster.
func splitRef(s string) (cluster, name string) {
	if c, n, ok := strings.Cut(s, "/"); ok && c != "" && n != "" {
		return c, n
	}
	return "", s
}

// joinRef is the other way: <cluster>/<name>, or the name alone.
func joinRef(cluster, name string) string {
	if cluster == "" {
		return name
	}
	return cluster + "/" + name
}

// inCluster is the API path p (/api/…) in cluster c, or p without one.
func inCluster(c, p string) string {
	if c == "" {
		return p
	}
	return "/api/clusters/" + url.PathEscape(c) + strings.TrimPrefix(p, "/api")
}

// scoped is p in the cluster a reference named, else in --cluster's.
func scoped(e *Env, cluster, p string) string {
	if cluster == "" {
		cluster = e.Cluster
	}
	return inCluster(cluster, p)
}

// dpPath is the API path of a DevPod, given as <name> or <cluster>/<name>.
func dpPath(e *Env, ref string) string {
	c, n := splitRef(ref)
	return scoped(e, c, "/api/devpods/"+url.PathEscape(n))
}

// explain says the console's refusals about clusters the CLI's way: its
// own message names the API's paths.
func explain(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	switch ae.Code {
	case "AMBIGUOUS_CLUSTER":
		b, _ := json.Marshal(ae.Detail)
		var cands []struct {
			Cluster string `json:"cluster"`
			Name    string `json:"name"`
		}
		_ = json.Unmarshal(b, &cands)
		if len(cands) == 0 {
			ae.Message = "the name is in several clusters: give it as <cluster>/<name>, or add --cluster"
			break
		}
		var cs []string
		for _, c := range cands {
			cs = append(cs, c.Cluster)
		}
		sort.Strings(cs)
		name := cands[0].Name
		ae.Message = fmt.Sprintf("%s is in several clusters (%s): say which as %s, or add --cluster %s",
			name, strings.Join(cs, ", "), joinRef(cs[0], name), cs[0])
		ae.Detail = nil
	case "CLUSTER_REQUIRED":
		b, _ := json.Marshal(ae.Detail)
		var cs []string
		_ = json.Unmarshal(b, &cs)
		ae.Message = "say which cluster with --cluster <name>"
		if len(cs) > 0 {
			sort.Strings(cs)
			ae.Message += " (" + strings.Join(cs, ", ") + ")"
		}
		ae.Detail = nil
	}
	return ae
}

// sshInfo is where to reach a cluster's DevPods over SSH.
type sshInfo struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	LoginSuffix string `json:"loginSuffix"`
}

func (s sshInfo) address() string {
	switch {
	case s.Host == "":
		return "-"
	case s.Port != 0 && s.Port != 22:
		return fmt.Sprintf("%s:%d", s.Host, s.Port)
	}
	return s.Host
}

type usageView struct {
	DevPods int               `json:"devpods"`
	Running int               `json:"running"`
	Compute map[string]string `json:"compute"`
	Storage string            `json:"storage"`
}

// meView is GET /api/me as the CLI reads it; Clusters is per cluster, in a
// console of several.
type meView struct {
	User     string         `json:"user"`
	Admin    bool           `json:"admin"`
	Usage    usageView      `json:"usage"`
	Quota    map[string]any `json:"quota"`
	SSH      sshInfo        `json:"ssh"`
	Clusters map[string]struct {
		SSH   *sshInfo       `json:"ssh"`
		Usage usageView      `json:"usage"`
		Quota map[string]any `json:"quota"`
	} `json:"clusters"`
}

// sshOf is the SSH address of a cluster's DevPods (the console's own
// without one).
func (m *meView) sshOf(cluster string) sshInfo {
	if c, ok := m.Clusters[cluster]; ok && c.SSH != nil {
		return *c.SSH
	}
	return m.SSH
}

func runClusters(e *Env, a *parsed) int {
	if len(a.pos) > 0 {
		return usage(e, find("clusters"), "clusters takes no arguments")
	}
	var list struct {
		Items []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Connected   bool   `json:"connected"`
			Paused      bool   `json:"paused"`
			Gateway     string `json:"gateway"`
		} `json:"items"`
	}
	raw, err := call(e, "GET", "/api/clusters", nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(e.Out, "one cluster: names need no cluster")
		return ExitOK
	}
	var me meView
	_, _ = call(e, "GET", "/api/me", nil, &me)
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	table(e, "NAME\tSTATUS\tGATEWAY\tSSH\tDEVPODS", func(w io.Writer) {
		for _, c := range list.Items {
			status := "connected"
			switch {
			case c.Paused:
				status = "paused"
			case !c.Connected:
				status = "away"
			}
			mine := "-"
			if u, ok := me.Clusters[c.Name]; ok {
				mine = fmt.Sprintf("%d (%d running)", u.Usage.DevPods, u.Usage.Running)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Name, status, dash(c.Gateway), me.sshOf(c.Name).address(), mine)
		}
	})
	return ExitOK
}
