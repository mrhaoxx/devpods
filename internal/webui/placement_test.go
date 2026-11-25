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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

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

// allowedOf: the console's "only on these machines" of dp (nil: none).
func allowedOf(dp devpodv1alpha1.DevPod) []string {
	if s := dp.Annotations[access.AnnAllowed]; s != "" {
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
	if in := allowedOf(p.get(dp)); slices.Contains(in, p.n1) || !slices.Contains(in, p.n2) {
		t.Fatalf("allowed %v", in)
	}
	p.patch("pu-carol", dp, `{"running":false}`, http.StatusOK)
	setRules(t, devpodv1alpha1.AccessRule{Subject: &devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: denied}) // every machine
	if code := p.patch("pu-carol", dp, `{"running":true}`, http.StatusForbidden); code != "ACL_DENIED" {
		t.Fatalf("no machine left: %q", code)
	}
	setRules(t)
	p.patch("pu-carol", dp, `{"running":true}`, http.StatusOK)
	got := p.get(dp)
	if allowedOf(got) != nil || got.Spec.Pod.Spec.Affinity != nil {
		t.Fatalf("allow-list left: %v %+v", allowedOf(got), got.Spec.Pod.Spec.Affinity)
	}
}

func TestPlacementAdminOwner(t *testing.T) {
	p := newPlaceRig(t, "pa")
	p.s.Admins = map[string]bool{"pa-root": true}
	setRules(t, devpodv1alpha1.AccessRule{Subject: &devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: denied})
	dp := p.create("pa-root", http.StatusCreated)
	if in := allowedOf(p.get(dp)); in != nil {
		t.Fatalf("an admin's DevPod got an allow-list: %v", in)
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
	if !equality.Semantic.DeepEqual(before.Spec.Pod, after.Spec.Pod) || before.Annotations[access.AnnAllowed] != after.Annotations[access.AnnAllowed] {
		t.Fatalf("the Pod's spec changed: %+v -> %+v", before.Spec.Pod.Spec.Affinity, after.Spec.Pod.Spec.Affinity)
	}
}

func TestNodeChoicesAccess(t *testing.T) {
	p := newPlaceRig(t, "nca")
	setRules(t, onNode(p.n1, denied))
	type choice struct {
		Name       string `json:"name"`
		Access     string `json:"access"`
		Selectable bool   `json:"selectable"`
	}
	list := func(c *http.Cookie) map[string]choice {
		t.Helper()
		rec := doJSON(t, p.s.HandleNodesForTest(), "GET", "/api/nodes", nil, c, "")
		var out struct{ Items []choice }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("nodes: %d %s", rec.Code, rec.Body)
		}
		got := map[string]choice{}
		for _, n := range out.Items {
			got[n.Name] = n
		}
		return got
	}
	// a machine the rules keep a user off is not shown to them at all
	bob := list(p.as("nca-bob"))
	if _, shown := bob[p.n1]; shown {
		t.Errorf("%s shown to bob: %+v", p.n1, bob[p.n1])
	}
	if n, ok := bob[p.n2]; !ok || n.Access != "ok" || !n.Selectable {
		t.Errorf("%s: %+v", p.n2, n)
	}
	// an admin sees every machine
	root := list(forge(p.sm, "nca-root", true))
	if _, ok := root[p.n1]; !ok {
		t.Errorf("%s not shown to an admin", p.n1)
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

var everyoneAllowed = devpodv1alpha1.AccessRule{Subject: &devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: devpodv1alpha1.AccessEffect{Allow: true}}

// A machine that needs a reservation is used by pinning to it: an
// unpinned DevPod never goes there, reservation held or not — nothing
// would end it there when the reservation does (it may still be pending
// then, or have its Pod remade later). Refused for lack of any machine,
// it is told why.
func TestPlacementReservedMachineByPinOnly(t *testing.T) {
	p := newPlaceRig(t, "pk")
	setRules(t, onNode(p.n1, needsReservation))
	reserveAs(t, p.s, p.as("pk-alice"), p.n1, p.slot, p.slot.Add(time.Hour))
	dp := p.create("pk-alice", http.StatusCreated)
	if in := allowedOf(p.get(dp)); slices.Contains(in, p.n1) || !slices.Contains(in, p.n2) {
		t.Fatalf("allowed %v", in)
	}
	p.patch("pk-alice", dp, fmt.Sprintf(`{"node":%q}`, p.n1), http.StatusOK) // pinned, it may
	p.patch("pk-alice", dp, `{"node":""}`, http.StatusOK)
	p.patch("pk-alice", dp, `{"running":false}`, http.StatusOK)
	setRules(t, onNode(p.n1, needsReservation), devpodv1alpha1.AccessRule{Subject: &devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: denied})
	if code := p.patch("pk-alice", dp, `{"running":true}`, http.StatusConflict); code != "RESERVATION_REQUIRED" {
		t.Fatalf("only a reserved machine left: %q", code)
	}
}

// putYAML sends a DevPod's spec through the YAML editor until the answer
// is want (the cache catches up), and returns the error code.
func (p *placeRig) putYAML(user, name string, spec devpodv1alpha1.DevPodSpec, want int) string {
	p.t.Helper()
	text, err := yaml.Marshal(map[string]any{"apiVersion": "devpod.io/v1alpha1", "kind": "DevPod", "metadata": map[string]any{"name": name}, "spec": spec})
	if err != nil {
		p.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"yaml": string(text)})
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := doJSON(p.t, p.s.Routes().ServeHTTP, "PUT", "/api/devpods/"+name+"/yaml", nil, p.as(user), string(body))
		if rec.Code == want {
			var e struct{ Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			return e.Code
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("PUT yaml %s: %d (want %d) %s", name, rec.Code, want, rec.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The YAML editor (and the rebuild from a config, which shares its
// replaceDevPodSpec) is a way in like any other: a running DevPod given
// privileges no machine allows is refused, saying so; one whose Pod's spec
// changed carries the exclusion the rules ask for now.
func TestPlacementYAML(t *testing.T) {
	p := newPlaceRig(t, "py")
	dp := p.create("py-carol", http.StatusCreated)
	setRules(t, onNode(p.n1, denied), everyoneAllowed) // n2: allowed, not privileged
	spec := p.get(dp).Spec
	priv := spec.DeepCopy()
	yes := true
	priv.Pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes}
	if code := p.putYAML("py-carol", dp, *priv, http.StatusForbidden); code != "PRIVILEGE_DENIED" {
		t.Fatalf("privileged through YAML: %q", code)
	}
	if got := p.get(dp); got.Spec.Pod.Spec.Containers[0].SecurityContext != nil {
		t.Fatalf("written: %+v", got.Spec.Pod.Spec.Containers[0].SecurityContext)
	}
	edited := spec.DeepCopy()
	edited.Pod.Spec.Containers[0].Env = append(edited.Pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "X", Value: "1"})
	p.putYAML("py-carol", dp, *edited, http.StatusOK)
	if in := allowedOf(p.get(dp)); slices.Contains(in, p.n1) || !slices.Contains(in, p.n2) {
		t.Fatalf("a running spec edit kept the old allow-list: %v", in)
	}
}

// A redeploy remakes the Pod: where it may go is decided again.
func TestPlacementRedeploy(t *testing.T) {
	p := newPlaceRig(t, "pd")
	ctx := context.Background()
	dp := p.create("pd-carol", http.StatusCreated)
	setRules(t, onNode(p.n1, denied))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: dp, Namespace: "devpods"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}}}}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	got := p.get(dp)
	got.Status.WorkloadRef = &devpodv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "Pod", Name: dp}
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := doJSON(t, p.s.HandleRedeployDevPodForTest(), "POST", "/api/devpods/"+dp+"/redeploy", map[string]string{"name": dp}, p.as("pd-carol"), "")
		if rec.Code == http.StatusNoContent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("redeploy: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if in := allowedOf(p.get(dp)); slices.Contains(in, p.n1) || !slices.Contains(in, p.n2) {
		t.Fatalf("redeployed with the old allow-list: %v", in)
	}
}

// The console writes "only on these machines", not "not on those": a
// machine that joins later is closed to a DevPod the rules restrict until
// it is started again — even when no machine the rules forbid is there now.
func TestPlacementAllowListClosesLaterMachines(t *testing.T) {
	p := newPlaceRig(t, "pj")
	later := "pj-n3"
	setRules(t, onNode(later, denied))
	dp := p.create("pj-carol", http.StatusCreated)
	if in := allowedOf(p.get(dp)); !slices.Contains(in, p.n1) || !slices.Contains(in, p.n2) || slices.Contains(in, later) {
		t.Fatalf("allowed %v", in)
	}
	terms := p.get(dp).Spec.Pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for _, term := range terms {
		if !slices.ContainsFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
			return r.Key == "kubernetes.io/hostname" && r.Operator == corev1.NodeSelectorOpIn
		}) {
			t.Fatalf("a term without the allow-list: %+v", term)
		}
	}
}

// Reservations on a machine the rules keep a user off are not shown to
// them (the 预约 page, the SSH CLI's reserve list) — but their own are.
func TestReservationsHideDeniedMachines(t *testing.T) {
	p := newPlaceRig(t, "rh")
	setRules(t, onNode(p.n1, denied))
	ctx := context.Background()
	for _, r := range []struct{ user, node string }{{"rh-dave", p.n1}, {"rh-dave", p.n2}, {"rh-bob", p.n1}} {
		nr := &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: r.user + "-" + r.node}, Spec: devpodv1alpha1.NodeReservationSpec{
			Node: r.node, User: r.user, Start: metav1.NewTime(p.slot), End: metav1.NewTime(p.slot.Add(time.Hour))}}
		if err := k8sClient.Create(ctx, nr); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(user string) []string {
		t.Helper()
		var got []string
		for _, v := range listReservations(t, p.s, p.as(user)) {
			got = append(got, v.ID)
		}
		slices.Sort(got)
		return got
	}
	if got := ids("rh-bob"); !slices.Equal(got, []string{"rh-bob-" + p.n1, "rh-dave-" + p.n2}) {
		t.Fatalf("bob sees %v", got)
	}
}

// createGPU makes user's DevPod name asking gpus cards, pinned to node
// ("": unpinned), and returns the error code ("" when made).
func (p *placeRig) createGPU(user, name string, gpus int, node string, want int) string {
	p.t.Helper()
	body := fmt.Sprintf(`{"name":%q,"image":"ubuntu:24.04","cpu":"2","memory":"4Gi","gpu":"%d","node":%q}`, name, gpus, node)
	rec := doJSON(p.t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, p.as(user), body)
	if rec.Code != want {
		p.t.Fatalf("create %s-%s (%d GPUs on %q): %d %s", user, name, gpus, node, rec.Code, rec.Body)
	}
	var e struct {
		Code   string
		Detail struct{ Free struct{ GPU, CPU float64 } }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if want == http.StatusCreated {
		waitDevPod(p.t, user+"-"+name)
	}
	return e.Code
}

// One who may use a machine only under a reservation uses no more of it
// than they reserved; a redeploy counts the DevPod once.
func TestStartWithinReservation(t *testing.T) {
	p := newPlaceRig(t, "sw")
	setRules(t, devpodv1alpha1.AccessRule{Machines: []string{p.n1}, Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"sw-alice"}}, Effect: needsReservation})
	if code, v := reserveWith(t, p.s, p.as("sw-alice"), p.n1, p.slot, p.slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	if code := p.createGPU("sw-alice", "g8", 8, p.n1, http.StatusConflict); code != "RESERVATION_TOO_SMALL" {
		t.Fatalf("8 cards in a 4-card reservation: %q", code)
	}
	p.createGPU("sw-alice", "g4", 4, p.n1, http.StatusCreated)
	if code := p.createGPU("sw-alice", "g1", 1, p.n1, http.StatusConflict); code != "RESERVATION_TOO_SMALL" {
		t.Fatalf("a fifth card: %q", code)
	}
	// redeployed: it is counted once
	ctx := context.Background()
	dp := "sw-alice-g4"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: dp, Namespace: "devpods"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}}}}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	got := p.get(dp)
	got.Status.WorkloadRef = &devpodv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "Pod", Name: dp}
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := doJSON(t, p.s.HandleRedeployDevPodForTest(), "POST", "/api/devpods/"+dp+"/redeploy", map[string]string{"name": dp}, p.as("sw-alice"), "")
		if rec.Code == http.StatusNoContent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("redeploy: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Everyone else — an admin too — uses what no reservation holds.
func TestStartOutsideReservations(t *testing.T) {
	p := newPlaceRig(t, "so")
	alice := p.as("so-alice")
	code, whole := reserveAs(t, p.s, alice, p.n1, p.slot, p.slot.Add(time.Hour))
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d", code)
	}
	if code := p.createGPU("so-bob", "g1", 1, p.n1, http.StatusConflict); code != "NO_ROOM" {
		t.Fatalf("bob on a machine reserved whole: %q", code)
	}
	root := forge(p.sm, "so-root", true)
	rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, root, fmt.Sprintf(`{"name":"g1","image":"ubuntu:24.04","cpu":"2","memory":"4Gi","gpu":"1","node":%q}`, p.n1))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "NO_ROOM") {
		t.Fatalf("an admin on a machine reserved whole: %d %s", rec.Code, rec.Body)
	}
	// alice gives back the whole machine and keeps half of it
	if rec := doJSON(t, p.s.HandleDeleteReservationForTest(), "DELETE", "/api/reservations/"+whole.ID, map[string]string{"id": whole.ID}, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("release: %d %s", rec.Code, rec.Body)
	}
	if code, v := reserveWith(t, p.s, alice, p.n1, p.slot, p.slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("reserve half: %d %+v", code, v)
	}
	p.createGPU("so-bob", "g4", 4, p.n1, http.StatusCreated)
	if code := p.createGPU("so-bob", "g1", 1, p.n1, http.StatusConflict); code != "NO_ROOM" {
		t.Fatalf("bob past the unreserved half: %q", code)
	}
}

// A DevPod not pinned is kept off reservable machines that have no room
// for it now — and only then gets an allow-list it would not have had.
func TestUnpinnedKeptOffFullMachines(t *testing.T) {
	p := newPlaceRig(t, "uk")
	root := forge(p.sm, "uk-root", true)
	create := func(c *http.Cookie, owner, name string, want int) devpodv1alpha1.DevPod {
		t.Helper()
		rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, c, fmt.Sprintf(`{"name":%q,"image":"ubuntu:24.04","cpu":"2","memory":"4Gi","gpu":"1"}`, name))
		if rec.Code != want {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
		if want != http.StatusCreated {
			return devpodv1alpha1.DevPod{}
		}
		waitDevPod(t, owner+"-"+name)
		return p.get(owner + "-" + name)
	}
	if in := allowedOf(create(root, "uk-root", "u1", http.StatusCreated)); in != nil {
		t.Fatalf("nothing reserved, yet an allow-list: %v", in)
	}
	if code, _ := reserveAs(t, p.s, p.as("uk-alice"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve n1: %d", code)
	}
	if in := allowedOf(create(root, "uk-root", "u2", http.StatusCreated)); slices.Contains(in, p.n1) || !slices.Contains(in, p.n2) {
		t.Fatalf("admin's, n1 reserved whole: %v", in)
	}
	// bob, whom some rule restricts
	setRules(t, devpodv1alpha1.AccessRule{Machines: []string{"uk-elsewhere"}, Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"uk-bob"}}, Effect: denied})
	if in := allowedOf(create(p.as("uk-bob"), "uk-bob", "u3", http.StatusCreated)); slices.Contains(in, p.n1) || !slices.Contains(in, p.n2) {
		t.Fatalf("bob's, n1 reserved whole: %v", in)
	}
	// n2 reservable and reserved too: nowhere left
	var n corev1.Node
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: p.n2}, &n); err != nil {
		t.Fatal(err)
	}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[devpodv1alpha1.ReservableNodeLabel] = "true"
	if err := k8sClient.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var got corev1.Node
		return k8sClient.Get(context.Background(), types.NamespacedName{Name: p.n2}, &got) == nil && got.Labels[devpodv1alpha1.ReservableNodeLabel] == "true"
	})
	if code, v := reserveAs(t, p.s, p.as("uk-alice"), p.n2, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve n2: %d %+v", code, v)
	}
	create(root, "uk-root", "u4", http.StatusConflict)
}
