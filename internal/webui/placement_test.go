// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/webui"
)

var denied = devpodv1alpha1.AccessEffect{}

// placeRig: rsvRig's two nodes (<p>-n1 reservable, <p>-n2 not) and a way
// to create, patch and read DevPods.
type placeRig struct {
	t    *testing.T
	s    *webui.Server
	sm   *webui.SessionManager
	n1   string
	n2   string
	slot time.Time
}

func newPlaceRig(t *testing.T, prefix string) *placeRig {
	s, sm, _, n1 := rsvRig(t, prefix)
	return &placeRig{t: t, s: s, sm: sm, n1: n1, n2: prefix + "-n2", slot: time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)}
}

func (p *placeRig) as(user string) *http.Cookie { return forge(p.sm, user, false) }

// create makes user's dev1 (running, unpinned) and waits for it.
func (p *placeRig) create(user string, want int) string {
	p.t.Helper()
	rec := doJSON(p.t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, p.as(user), createBody)
	if rec.Code != want {
		p.t.Fatalf("create %s: %d %s", user, rec.Code, rec.Body)
	}
	name := user + "-dev1"
	if want == http.StatusCreated {
		waitDevPod(p.t, name)
	}
	return name
}

// patch sends body until the answer is want (the cache catches up), and
// returns the error code.
func (p *placeRig) patch(user, name, body string, want int) string {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := doJSON(p.t, p.s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+name, map[string]string{"name": name}, p.as(user), body)
		if rec.Code == want {
			var e struct{ Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			return e.Code
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("PATCH %s %s: %d (want %d) %s", name, body, rec.Code, want, rec.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (p *placeRig) get(name string) devpodv1alpha1.DevPod {
	p.t.Helper()
	var dp devpodv1alpha1.DevPod
	if err := p.s.Reader.Get(context.Background(), types.NamespacedName{Namespace: "devpods", Name: name}, &dp); err != nil {
		p.t.Fatal(err)
	}
	return dp
}

func excludedOf(dp devpodv1alpha1.DevPod) []string {
	if s := dp.Annotations[access.AnnExcluded]; s != "" {
		return strings.Split(s, ",")
	}
	return nil
}

func TestPlacementPinned(t *testing.T) {
	p := newPlaceRig(t, "pp")
	dp := p.create("pp-bob", http.StatusCreated)
	p.patch("pp-bob", dp, `{"running":false}`, http.StatusOK)
	p.patch("pp-bob", dp, fmt.Sprintf(`{"node":%q}`, p.n1), http.StatusOK)

	setRules(t, onNode(p.n1, denied))
	if code := p.patch("pp-bob", dp, `{"running":true}`, http.StatusForbidden); code != "ACL_DENIED" {
		t.Fatalf("denied: %q", code)
	}
	setRules(t, onNode(p.n1, needsReservation))
	if code := p.patch("pp-bob", dp, `{"running":true}`, http.StatusConflict); code != "RESERVATION_REQUIRED" {
		t.Fatalf("needs a reservation: %q", code)
	}
	if code, v := reserveAs(t, p.s, p.as("pp-bob"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	p.patch("pp-bob", dp, `{"running":true}`, http.StatusOK)

	// a privileged DevPod where the rule allows only plain ones
	setRules(t, onNode(p.n1, devpodv1alpha1.AccessEffect{Allow: true}))
	yaml := fmt.Sprintf("apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: pp-bob-priv\nspec:\n  owner: pp-bob\n  running: true\n  pod:\n    spec:\n      nodeSelector:\n        kubernetes.io/hostname: %s\n      containers:\n        - name: dev\n          image: ubuntu:24.04\n          securityContext:\n            privileged: true\n", p.n1)
	body, _ := json.Marshal(map[string]string{"yaml": yaml})
	rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, p.as("pp-bob"), string(body))
	var e struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusForbidden || e.Code != "PRIVILEGE_DENIED" {
		t.Fatalf("privileged: %d %s", rec.Code, rec.Body)
	}
}

func TestPlacementUnpinned(t *testing.T) {
	p := newPlaceRig(t, "pu")
	setRules(t, onNode(p.n1, denied))
	dp := p.create("pu-carol", http.StatusCreated)
	if ex := excludedOf(p.get(dp)); !slices.Contains(ex, p.n1) || slices.Contains(ex, p.n2) {
		t.Fatalf("excluded %v", ex)
	}
	p.patch("pu-carol", dp, `{"running":false}`, http.StatusOK)
	setRules(t, devpodv1alpha1.AccessRule{Subject: devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: denied}) // every machine
	if code := p.patch("pu-carol", dp, `{"running":true}`, http.StatusForbidden); code != "ACL_DENIED" {
		t.Fatalf("no machine left: %q", code)
	}
	setRules(t)
	p.patch("pu-carol", dp, `{"running":true}`, http.StatusOK)
	got := p.get(dp)
	if excludedOf(got) != nil || got.Spec.Pod.Spec.Affinity != nil {
		t.Fatalf("exclusion left: %v %+v", excludedOf(got), got.Spec.Pod.Spec.Affinity)
	}
}

func TestPlacementAdminOwner(t *testing.T) {
	p := newPlaceRig(t, "pa")
	p.s.Admins = map[string]bool{"pa-root": true}
	setRules(t, devpodv1alpha1.AccessRule{Subject: devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: denied})
	dp := p.create("pa-root", http.StatusCreated)
	if ex := excludedOf(p.get(dp)); ex != nil {
		t.Fatalf("an admin's DevPod got an exclusion: %v", ex)
	}
}

// A running DevPod changed in what does not recreate its Pod keeps its
// spec as it is: the exclusion is not rewritten under it.
func TestPlacementLeavesRunningAlone(t *testing.T) {
	p := newPlaceRig(t, "pr")
	setRules(t, onNode(p.n1, denied))
	dp := p.create("pr-dave", http.StatusCreated)
	before := p.get(dp)
	setRules(t)
	p.patch("pr-dave", dp, `{"collaborators":["pr-erin"]}`, http.StatusOK)
	after := p.get(dp)
	if !equality.Semantic.DeepEqual(before.Spec.Pod, after.Spec.Pod) || before.Annotations[access.AnnExcluded] != after.Annotations[access.AnnExcluded] {
		t.Fatalf("the Pod's spec changed: %+v -> %+v", before.Spec.Pod.Spec.Affinity, after.Spec.Pod.Spec.Affinity)
	}
}

func TestNodeChoicesAccess(t *testing.T) {
	p := newPlaceRig(t, "nca")
	setRules(t, onNode(p.n1, denied))
	rec := doJSON(t, p.s.HandleNodesForTest(), "GET", "/api/nodes", nil, p.as("nca-bob"), "")
	var out struct {
		Items []struct {
			Name       string `json:"name"`
			Access     string `json:"access"`
			Selectable bool   `json:"selectable"`
		} `json:"items"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("nodes: %d %s", rec.Code, rec.Body)
	}
	seen := 0
	for _, n := range out.Items {
		switch n.Name {
		case p.n1:
			seen++
			if n.Access != "denied" || n.Selectable {
				t.Errorf("%s: %+v", n.Name, n)
			}
		case p.n2:
			seen++
			if n.Access != "ok" || !n.Selectable {
				t.Errorf("%s: %+v", n.Name, n)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("listed: %s", rec.Body)
	}
}

// Launching from a config onto a machine the rules deny makes nothing.
func TestLaunchConfigChecks(t *testing.T) {
	p := newPlaceRig(t, "lc")
	t.Cleanup(func() { cleanupConfigs(t) })
	setRules(t, onNode(p.n1, denied))
	srv := httptest.NewServer(p.s.Routes())
	t.Cleanup(srv.Close)
	bob := p.as("lc-bob")
	apiCall(t, srv, "POST", "/api/configs", bob, jsonBody(map[string]any{"id": "pin", "title": "pin", "content": launchYAML("${NAME}", "1", pinExtra)}), http.StatusCreated)
	items := launchItems(t, apiCall(t, srv, "POST", "/api/configs/lc-bob-pin/launch", bob, jsonBody(map[string]any{"vars": map[string]string{"NAME": "x"}, "nodes": []string{p.n1}}), http.StatusUnprocessableEntity))
	if len(items) != 1 || items[0].OK || !strings.Contains(items[0].Code+items[0].Message, "ACL_DENIED") && !strings.Contains(items[0].Message, "may not use") {
		t.Fatalf("launch: %+v", items)
	}
	if devpodExists(t, "lc-bob-x") {
		t.Fatal("a DevPod was made")
	}
}
