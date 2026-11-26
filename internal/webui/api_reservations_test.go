// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// rsvRig: reservations on with 30 min slots and a fixed clock at 10:10
// Shanghai time; one reservable node and one that is not.
func rsvRig(t *testing.T, prefix string) (*webui.Server, *webui.SessionManager, time.Time, string) {
	t.Helper()
	setupSuite(t)
	s, sm := newServer(t)
	now := time.Date(2026, 11, 12, 2, 10, 0, 0, time.UTC) // 10:10 +08:00
	s.Reservations = &webui.ReservationRules{Granularity: 30 * time.Minute, MaxDuration: 24 * time.Hour, MaxAdvance: 14 * 24 * time.Hour,
		ClearNotice: 30 * time.Minute, Now: func() time.Time { return now }}
	ctx := context.Background()
	node := prefix + "-n1"
	// n1: reservable, 8 cards and 192 cores; n2: not, 64 cores, no cards
	for name, reservable := range map[string]bool{node: true, prefix + "-n2": false} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		if reservable {
			n.Labels[devpodv1alpha1.ReservableNodeLabel] = "true"
		}
		if err := k8sClient.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, n) })
		n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")}
		if reservable {
			n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("192"), gpures.NVIDIA: resource.MustParse("8")}
		}
		n.Status.Capacity = n.Status.Allocatable
		if err := k8sClient.Status().Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		var list devpodv1alpha1.NodeReservationList
		_ = k8sClient.List(ctx, &list)
		for i := range list.Items {
			_ = k8sClient.Delete(ctx, &list.Items[i])
		}
		_ = k8sClient.Delete(ctx, &devpodv1alpha1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.AccessPolicyName}})
		cleanupDevPods(t)
	})
	// the nodes reach the cached client a moment later
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n corev1.Node
		if (k8sClient.Get(ctx, client_key_cluster(node), &n) == nil && len(n.Status.Allocatable) > 0) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s, sm, now, node
}

type rsvView struct {
	ID      string    `json:"id"`
	Node    string    `json:"node"`
	User    string    `json:"user"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Phase   string    `json:"phase"`
	Mine    bool      `json:"mine"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Purpose string    `json:"purpose"`
	// CanManage: the caller may change or end it
	CanManage  bool     `json:"canManage"`
	GPU        *int     `json:"gpu"`
	CPU        *float64 `json:"cpu"`
	Overbooked bool     `json:"overbooked"`
	Detail     struct {
		Free     struct{ GPU, CPU float64 }
		Earliest time.Time
	} `json:"detail"`
}

// reserveWith: alice reserves with these extra fields (gpu, cpu, …).
func reserveWith(t *testing.T, s *webui.Server, c *http.Cookie, node string, start, end time.Time, extra map[string]any) (int, rsvView) {
	t.Helper()
	body := map[string]any{"node": node, "start": start, "end": end}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, c, string(b))
	var v rsvView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v
}

func rsvBody(node string, start, end time.Time) string {
	b, _ := json.Marshal(map[string]any{"node": node, "start": start, "end": end})
	return string(b)
}

// setRules makes the access rules these, and waits until the console's
// cache has them.
func setRules(t *testing.T, rules ...devpodv1alpha1.AccessRule) {
	t.Helper()
	ctx := context.Background()
	var p devpodv1alpha1.AccessPolicy
	err := k8sClient.Get(ctx, client_key_cluster(devpodv1alpha1.AccessPolicyName), &p)
	p.Name = devpodv1alpha1.AccessPolicyName
	p.Spec = devpodv1alpha1.AccessPolicySpec{Rules: rules, Default: devpodv1alpha1.DefaultAccessEffect()}
	if err != nil {
		p.ResourceVersion = ""
		err = k8sClient.Create(ctx, &p)
	} else {
		err = k8sClient.Update(ctx, &p)
	}
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got devpodv1alpha1.AccessPolicy
		if k8sClient.Get(ctx, client_key_cluster(devpodv1alpha1.AccessPolicyName), &got) == nil && got.ResourceVersion == p.ResourceVersion {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the rules did not reach the cache")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func onNode(node string, e devpodv1alpha1.AccessEffect) devpodv1alpha1.AccessRule {
	return devpodv1alpha1.AccessRule{Machines: []string{node}, Subject: &devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: e}
}

var needsReservation = devpodv1alpha1.AccessEffect{Allow: true, NeedsReservation: true}

func reserveAs(t *testing.T, s *webui.Server, c *http.Cookie, node string, start, end time.Time) (int, rsvView) {
	t.Helper()
	rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, c, rsvBody(node, start, end))
	var v rsvView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v
}

func listReservations(t *testing.T, s *webui.Server, c *http.Cookie) []rsvView {
	t.Helper()
	rec := doJSON(t, s.HandleListReservationsForTest(), "GET", "/api/reservations", nil, c, "")
	var out struct{ Items []rsvView }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	return out.Items
}

// A reservation holds its share: whole machines are exclusive, shares
// add up to no more than the machine, and a refusal says what is free and
// when what was asked would be.
func TestReservationRoom(t *testing.T) {
	s, sm, _, node := rsvRig(t, "ov")
	setRules(t, onNode(node, needsReservation))
	alice, bob, carol := forge(sm, "ov-alice", false), forge(sm, "ov-bob", false), forge(sm, "ov-carol", false)
	ten := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // 10:00 +08:00
	if code, v := reserveAs(t, s, alice, node, ten, ten.Add(time.Hour)); code != http.StatusCreated || v.GPU != nil || v.CPU != nil {
		t.Fatalf("alice whole: %d %+v", code, v)
	}
	code, v := reserveAs(t, s, bob, node, ten.Add(30*time.Minute), ten.Add(90*time.Minute))
	if code != http.StatusConflict || v.Code != "NO_ROOM" || v.Detail.Free.GPU != 0 || v.Detail.Free.CPU != 0 || !v.Detail.Earliest.Equal(ten.Add(time.Hour)) {
		t.Fatalf("bob whole over alice's: %d %+v", code, v)
	}
	if code, v := reserveWith(t, s, bob, node, ten.Add(30*time.Minute), ten.Add(90*time.Minute), map[string]any{"gpu": 4}); code != http.StatusConflict {
		t.Fatalf("bob 4 cards over alice's whole: %d %+v", code, v)
	}
	// asked from the current slot: free from a slot start, not from now on
	if code, v := reserveAs(t, s, bob, node, ten, ten.Add(time.Hour)); code != http.StatusConflict || !v.Detail.Earliest.Equal(ten.Add(time.Hour)) {
		t.Fatalf("bob from the current slot: %d %+v", code, v)
	}
	noon := ten.Add(2 * time.Hour)
	if code, v := reserveWith(t, s, alice, node, noon, noon.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated || v.GPU == nil || *v.GPU != 4 || v.CPU == nil || *v.CPU != 96 {
		t.Fatalf("alice 4 cards (cpu follows): %d %+v", code, v)
	}
	if code, v := reserveWith(t, s, bob, node, noon, noon.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("bob the other 4: %d %+v", code, v)
	}
	code, v = reserveWith(t, s, carol, node, noon, noon.Add(time.Hour), map[string]any{"gpu": 1})
	if code != http.StatusConflict || v.Code != "NO_ROOM" || v.Detail.Free.GPU != 0 || v.Detail.Free.CPU != 0 {
		t.Fatalf("carol on a full machine: %d %+v", code, v)
	}
	if items := listReservations(t, s, alice); len(items) != 3 {
		t.Fatalf("listed %d", len(items))
	}
	// released at 10:10, the rest of the slot is free: bob may have 10:00-11:00
	var mine string
	for _, v := range listReservations(t, s, alice) {
		if v.Mine && v.Start.Equal(ten) {
			mine = v.ID
		}
	}
	if rec := doJSON(t, s.HandleDeleteReservationForTest(), "DELETE", "/api/reservations/"+mine, map[string]string{"id": mine}, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("release: %d %s", rec.Code, rec.Body)
	}
	if code, v := reserveAs(t, s, bob, node, ten, ten.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("bob after alice released: %d %+v", code, v)
	}
	for _, v := range listReservations(t, s, bob) {
		if v.Overbooked {
			t.Errorf("overbooked only in the past: %+v", v)
		}
	}
}

// Amounts are checked against what the machine has.
func TestReservationAmountsInvalid(t *testing.T) {
	s, sm, _, node := rsvRig(t, "ai")
	n2 := "ai-n2"
	var n corev1.Node
	if err := k8sClient.Get(context.Background(), client_key_cluster(n2), &n); err != nil {
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
		return k8sClient.Get(context.Background(), client_key_cluster(n2), &got) == nil && got.Labels[devpodv1alpha1.ReservableNodeLabel] == "true"
	})
	alice := forge(sm, "ai-alice", false)
	slot := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		node  string
		extra map[string]any
		want  int
		msg   string
	}{
		{node, map[string]any{"gpu": 9}, http.StatusBadRequest, ""},
		{n2, map[string]any{"gpu": 1}, http.StatusBadRequest, "has no GPUs"},
		{node, map[string]any{"gpu": 0, "cpu": 0}, http.StatusBadRequest, ""},
		{n2, map[string]any{"cpu": 65}, http.StatusBadRequest, ""},
		{node, map[string]any{"gpu": -1}, http.StatusBadRequest, ""},
	} {
		code, v := reserveWith(t, s, alice, c.node, slot, slot.Add(time.Hour), c.extra)
		if code != c.want || !strings.Contains(v.Message, c.msg) {
			t.Errorf("%s %v: %d %q", c.node, c.extra, code, v.Message)
		}
	}
	code, v := reserveWith(t, s, alice, n2, slot, slot.Add(time.Hour), map[string]any{"cpu": 16})
	if code != http.StatusCreated || v.GPU == nil || *v.GPU != 0 || v.CPU == nil || *v.CPU != 16 {
		t.Fatalf("16 cores of a CPU machine: %d %+v", code, v)
	}
}

// Reservations made before shares (whole, overlapping) are shown as
// overbooked; a change is checked only for what it adds.
func TestReservationChangeRoom(t *testing.T) {
	s, sm, _, node := rsvRig(t, "cr")
	ctx := context.Background()
	ten := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // 10:00 +08:00; the clock says 10:10
	for _, r := range []struct {
		name, user string
		start      time.Time
	}{{"cr-a", "cr-alice", ten}, {"cr-b", "cr-bob", ten.Add(30 * time.Minute)}, {"cr-c", "cr-carol", ten.Add(3 * time.Hour)}} {
		nr := &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: r.name}, Spec: devpodv1alpha1.NodeReservationSpec{
			Node: node, User: r.user, Start: metav1.NewTime(r.start), End: metav1.NewTime(r.start.Add(time.Hour))}}
		if err := k8sClient.Create(ctx, nr); err != nil {
			t.Fatal(err)
		}
	}
	alice, carol, dave, erin := forge(sm, "cr-alice", false), forge(sm, "cr-carol", false), forge(sm, "cr-dave", false), forge(sm, "cr-erin", false)
	over := map[string]bool{}
	for _, v := range listReservations(t, s, alice) {
		over[v.ID] = v.Overbooked
	}
	if !over["cr-a"] || !over["cr-b"] || over["cr-c"] {
		t.Fatalf("overbooked: %v", over)
	}
	patch := func(c *http.Cookie, id, body string) (int, rsvView) {
		t.Helper()
		rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+id, map[string]string{"id": id}, c, body)
		var v rsvView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	at := func(d time.Duration) string { return `"` + ten.Add(d).Format(time.RFC3339) + `"` }
	for _, c := range []struct {
		who  *http.Cookie
		id   *string
		body string
		want int
	}{
		// alice's, under way and over bob's: a purpose, an earlier end — yes; a later end, over bob's — no
		{alice, strp("cr-a"), `{"purpose":"x"}`, http.StatusOK},
		{alice, strp("cr-a"), `{"end":` + at(30*time.Minute) + `}`, http.StatusOK},
		{alice, strp("cr-a"), `{"end":` + at(90*time.Minute) + `}`, http.StatusConflict},
		// carol's, 13:00-14:00: a later end into free time — yes; an earlier start over bob's — no
		{carol, strp("cr-c"), `{"end":` + at(5*time.Hour) + `}`, http.StatusOK},
		{carol, strp("cr-c"), `{"start":` + at(time.Hour) + `}`, http.StatusConflict},
	} {
		if code, v := patch(c.who, *c.id, c.body); code != c.want {
			t.Errorf("%s %s: %d %+v", *c.id, c.body, code, v)
		}
	}
	// shares, 16:00-17:00: dave 4, erin 2
	four := ten.Add(6 * time.Hour)
	code, d := reserveWith(t, s, dave, node, four, four.Add(time.Hour), map[string]any{"gpu": 4})
	code2, e := reserveWith(t, s, erin, node, four, four.Add(time.Hour), map[string]any{"gpu": 2})
	if code != http.StatusCreated || code2 != http.StatusCreated {
		t.Fatalf("shares: %d %d", code, code2)
	}
	for _, c := range []struct {
		who  *http.Cookie
		id   string
		body string
		want int
	}{
		{dave, d.ID, `{"gpu":2,"cpu":48}`, http.StatusOK},
		{dave, d.ID, `{"whole":true}`, http.StatusConflict},
		{erin, e.ID, `{"gpu":6,"cpu":144}`, http.StatusOK},
		{erin, e.ID, `{"gpu":7,"cpu":144}`, http.StatusConflict},
	} {
		if code, v := patch(c.who, c.id, c.body); code != c.want {
			t.Errorf("%s %s: %d %+v", c.id, c.body, code, v)
		}
	}
	var got devpodv1alpha1.NodeReservation
	if err := k8sClient.Get(ctx, client_key_cluster(e.ID), &got); err != nil || got.Spec.GPU == nil || *got.Spec.GPU != 6 {
		t.Fatalf("erin's after a refused change: %+v %v", got.Spec, err)
	}
	// cores alone: the cards stay
	if code, v := patch(erin, e.ID, `{"cpu":100}`); code != http.StatusOK || v.GPU == nil || *v.GPU != 6 || v.CPU == nil || *v.CPU != 100 {
		t.Fatalf("cores alone: %d %+v", code, v)
	}
}

func strp(s string) *string { return &s }

// Only on reservable machines, by someone the rules let use them — whether
// or not they need a reservation there.
func TestReservationNeedsRule(t *testing.T) {
	s, sm, _, node := rsvRig(t, "nr")
	alice := forge(sm, "nr-alice", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	setRules(t, onNode("nr-n2", needsReservation))
	if code, v := reserveAs(t, s, alice, "nr-n2", slot, slot.Add(time.Hour)); code != http.StatusBadRequest || v.Code != "NOT_RESERVABLE" {
		t.Fatalf("not reservable: %d %+v", code, v)
	}
	// no rule: everyone may use it without one, and may reserve it all the same
	if code, v := reserveAs(t, s, alice, node, slot, slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("no need, reserved anyway: %d %+v", code, v)
	}
	setRules(t, onNode(node, devpodv1alpha1.AccessEffect{}))
	if code, v := reserveAs(t, s, alice, node, slot, slot.Add(time.Hour)); code != http.StatusForbidden || v.Code != "ACL_DENIED" {
		t.Fatalf("denied: %d %+v", code, v)
	}
}

func TestReservationPatchCancel(t *testing.T) {
	s, sm, _, node := rsvRig(t, "pc")
	setRules(t, onNode(node, needsReservation))
	alice := forge(sm, "pc-alice", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // 10:00; now is 10:10
	_, later := reserveAs(t, s, alice, node, slot.Add(2*time.Hour), slot.Add(3*time.Hour))
	_, now := reserveAs(t, s, alice, node, slot, slot.Add(time.Hour))
	call := func(h http.HandlerFunc, method, id, body string) int {
		return doJSON(t, h, method, "/api/reservations/"+id, map[string]string{"id": id}, alice, body).Code
	}
	if c := call(s.HandlePatchReservationForTest(), "PATCH", now.ID, fmt.Sprintf(`{"start":%q}`, slot.Add(30*time.Minute).Format(time.RFC3339))); c != http.StatusBadRequest {
		t.Fatalf("moving the start of a started one: %d", c)
	}
	rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+now.ID, map[string]string{"id": now.ID}, alice, fmt.Sprintf(`{"end":%q}`, slot.Add(2*time.Hour).Format(time.RFC3339)))
	var moved rsvView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &moved) != nil || !moved.End.Equal(slot.Add(2*time.Hour)) {
		t.Fatalf("its end: %d %s", rec.Code, rec.Body)
	}
	if c := call(s.HandleDeleteReservationForTest(), "DELETE", later.ID, ""); c != http.StatusNoContent {
		t.Fatalf("cancel before it starts: %d", c)
	}
	if c := call(s.HandleDeleteReservationForTest(), "DELETE", now.ID, ""); c != http.StatusNoContent {
		t.Fatalf("release a started one: %d", c)
	}
	items := listReservations(t, s, alice)
	// released at 10:10: that is when it ended
	if len(items) != 1 || items[0].ID != now.ID || items[0].Phase != "Cancelled" || !items[0].End.Equal(slot.Add(10*time.Minute)) {
		t.Fatalf("after: %+v", items)
	}
}

// When a reservation ends, its holder's DevPods there that only it let run
// are hibernated — not those another of their reservations still covers,
// nor those of users the rules let in without one.
func TestReservationEnd(t *testing.T) {
	s, sm, _, node := rsvRig(t, "re")
	setRules(t,
		devpodv1alpha1.AccessRule{Machines: []string{node}, Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"re-carol"}}, Effect: devpodv1alpha1.AccessEffect{Allow: true}},
		onNode(node, needsReservation))
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	now := slot.Add(10 * time.Minute)
	s.Reservations.Now = func() time.Time { return now }
	users := map[string]*http.Cookie{}
	for _, u := range []string{"re-alice", "re-bob", "re-carol"} {
		users[u] = forge(sm, u, false)
	}
	// reserved first: without one, alice and bob may not run there at all
	// (shares of it: 4 + 2 + 2 cards at most at once)
	for _, r := range []struct {
		who        string
		gpu        int
		start, end time.Time
	}{
		{"re-alice", 4, slot, slot.Add(time.Hour)},
		{"re-bob", 2, slot, slot.Add(time.Hour)},
		{"re-bob", 2, slot.Add(30 * time.Minute), slot.Add(2 * time.Hour)}, // still covers bob after 11:00
	} {
		if code, v := reserveWith(t, s, users[r.who], node, r.start, r.end, map[string]any{"gpu": r.gpu}); code != http.StatusCreated {
			t.Fatalf("reserve %s: %d %+v", r.who, code, v)
		}
	}
	for _, u := range []string{"re-alice", "re-bob", "re-carol"} {
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, users[u], createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", u, rec.Code, rec.Body)
		}
		name := u + "-dev1"
		waitDevPod(t, name)
		if rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+name, map[string]string{"name": name}, users[u], fmt.Sprintf(`{"node":%q}`, node)); rec.Code != http.StatusOK {
			t.Fatalf("pin %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	now = slot.Add(time.Hour)
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.EnforceReservations(ctx)
		var dp devpodv1alpha1.DevPod
		if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: "devpods", Name: "re-alice-dev1"}, &dp); err != nil {
			t.Fatal(err)
		}
		if !dp.Spec.Running && strings.HasPrefix(dp.Annotations[devpodv1alpha1.HibernatedByAnnotation], "reservation-end/") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alice's DevPod: running %v, by %q", dp.Spec.Running, dp.Annotations[devpodv1alpha1.HibernatedByAnnotation])
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, name := range []string{"re-bob-dev1", "re-carol-dev1"} {
		if dp := waitDevPod(t, name); !dp.Spec.Running {
			t.Fatalf("%s was hibernated", name)
		}
	}
}

func client_key_cluster(name string) types.NamespacedName { return types.NamespacedName{Name: name} }

// failingPatches: a client whose next n Patches of a DevPod fail, as when
// a cluster's agent link drops for a moment.
type failingPatches struct {
	client.Client
	n *atomic.Int32
}

func (f failingPatches) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*devpodv1alpha1.DevPod); ok && f.n.Add(-1) >= 0 {
		return errors.New("agent link down")
	}
	return f.Client.Patch(ctx, obj, p, opts...)
}

// The end of a reservation that could not be carried out (the hibernation
// failed) is not marked done: the next round tries again.
func TestReservationEndRetries(t *testing.T) {
	s, sm, _, node := rsvRig(t, "rr")
	setRules(t, onNode(node, needsReservation))
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	now := slot.Add(10 * time.Minute)
	s.Reservations.Now = func() time.Time { return now }
	alice := forge(sm, "rr-alice", false)
	_, v := reserveAs(t, s, alice, node, slot, slot.Add(time.Hour))
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "rr-alice-dev1")
	if rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/rr-alice-dev1", map[string]string{"name": "rr-alice-dev1"}, alice, fmt.Sprintf(`{"node":%q}`, node)); rec.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
	ctx := context.Background()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) { // the pin reaches the cache
		var dp devpodv1alpha1.DevPod
		if s.Client.Get(ctx, types.NamespacedName{Namespace: "devpods", Name: "rr-alice-dev1"}, &dp) == nil && dp.Spec.Pod != nil && dp.Spec.Pod.Spec.NodeSelector["kubernetes.io/hostname"] == node {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pin never reached the cache")
		}
	}
	ended := func() bool {
		var nr devpodv1alpha1.NodeReservation
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: v.ID}, &nr); err != nil {
			t.Fatal(err)
		}
		return nr.Status.Ended
	}
	now = slot.Add(time.Hour)
	real := s.Client
	fail := &atomic.Int32{}
	fail.Store(1)
	s.Client = failingPatches{Client: real, n: fail}
	s.EnforceReservations(ctx)
	s.Client = real
	if fail.Load() == 1 {
		t.Fatalf("the hibernation was not tried")
	}
	if ended() {
		t.Fatalf("marked ended though alice's DevPod still runs")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.EnforceReservations(ctx)
		var dp devpodv1alpha1.DevPod
		if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: "devpods", Name: "rr-alice-dev1"}, &dp); err != nil {
			t.Fatal(err)
		}
		if !dp.Spec.Running && ended() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not carried out on the next round: running %v, ended %v", dp.Spec.Running, ended())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Gateways before this release offer "reserve add --dry-run", which asks
// with ?dryRun=true: there is nothing to preview any more, and the ask
// must not make a reservation for real.
func TestReservationDryRunRefused(t *testing.T) {
	s, sm, _, node := rsvRig(t, "dr")
	setRules(t, onNode(node, needsReservation))
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	alice := forge(sm, "dr-alice", false)
	rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations?dryRun=true", nil, alice, rsvBody(node, slot, slot.Add(time.Hour)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	if items := listReservations(t, s, alice); len(items) != 0 {
		t.Fatalf("a dry run reserved: %+v", items)
	}
}

// My reservations, paged: only mine, of any time, the latest start first;
// those starting together (reserved at once on several machines) are
// neither lost nor repeated across a page's edge.
func TestMyReservationsPaged(t *testing.T) {
	s, sm, now, node := rsvRig(t, "pg")
	ctx := context.Background()
	slot := now.Truncate(time.Hour)
	mk := func(name, user string, start time.Time) {
		t.Helper()
		nr := &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: devpodv1alpha1.NodeReservationSpec{
			Node: node, User: user, Start: metav1.NewTime(start), End: metav1.NewTime(start.Add(time.Hour))}}
		if err := k8sClient.Create(ctx, nr); err != nil {
			t.Fatal(err)
		}
	}
	mk("pg-old", "pg-alice", slot.Add(-90*24*time.Hour)) // long before any window
	mk("pg-a", "pg-alice", slot.Add(-24*time.Hour))
	mk("pg-b", "pg-alice", slot.Add(-24*time.Hour))
	mk("pg-c", "pg-alice", slot.Add(-24*time.Hour))
	mk("pg-next", "pg-alice", slot.Add(24*time.Hour))
	mk("pg-bob", "pg-bob", slot)
	alice := forge(sm, "pg-alice", false)
	page := func(cursor string) ([]string, string) {
		t.Helper()
		url := "/api/reservations?mine=1&limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := doJSON(t, s.HandleListReservationsForTest(), "GET", url, nil, alice, "")
		var out struct {
			Items []rsvView
			Next  string
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		var ids []string
		for _, v := range out.Items {
			ids = append(ids, v.ID)
		}
		return ids, out.Next
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ids, _ := page(""); len(ids) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var got []string
	cursor := ""
	for i := 0; i < 5; i++ {
		ids, next := page(cursor)
		got = append(got, ids...)
		if next == "" {
			break
		}
		cursor = next
	}
	if want := []string{"pg-next", "pg-a", "pg-b", "pg-c", "pg-old"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pages gave %v, want %v", got, want)
	}
	// a cursor that is not one
	rec := doJSON(t, s.HandleListReservationsForTest(), "GET", "/api/reservations?mine=1&limit=2&cursor=nonsense", nil, alice, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad cursor: %d", rec.Code)
	}
}

// Closing a machine for reservation ends every reservation on it that is
// not over — one not begun is gone, one under way is cancelled now, as
// their owners would cancel them; another machine's stay.
func TestSetReservableOffEndsReservations(t *testing.T) {
	s, sm, _, node := rsvRig(t, "ro")
	admin := forge(sm, "ro-root", true)
	put := func(n string, on bool) int {
		return doJSON(t, s.Routes().ServeHTTP, "PUT", "/api/admin/nodes/"+n+"/reservable", nil, admin, fmt.Sprintf(`{"reservable":%v}`, on)).Code
	}
	if code := put("ro-n2", true); code != http.StatusOK {
		t.Fatalf("opening ro-n2: %d", code)
	}
	alice, bob, carol := forge(sm, "ro-alice", false), forge(sm, "ro-bob", false), forge(sm, "ro-carol", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // 10:00; now is 10:10
	var other rsvView
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var code int
		if code, other = reserveAs(t, s, carol, "ro-n2", slot, slot.Add(time.Hour)); code == http.StatusCreated {
			break
		}
	}
	if other.ID == "" {
		t.Fatalf("carol on ro-n2: %+v", other)
	}
	if code, v := reserveAs(t, s, alice, node, slot, slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("alice: %d %+v", code, v)
	}
	if code, v := reserveAs(t, s, bob, node, slot.Add(2*time.Hour), slot.Add(3*time.Hour)); code != http.StatusCreated {
		t.Fatalf("bob: %d %+v", code, v)
	}

	if code := put(node, false); code != http.StatusOK {
		t.Fatalf("closing %s: %d", node, code)
	}
	// everyone sees every reservation
	var got []string
	for _, v := range listReservations(t, s, alice) {
		got = append(got, v.User+" "+v.Node+" "+v.Phase)
	}
	sort.Strings(got)
	if want := []string{"ro-alice ro-n1 Cancelled", "ro-carol ro-n2 Active"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after closing %s: %v; want %v (bob's, not begun, gone)", node, got, want)
	}
	_ = bob
}

// A reservation's purpose: optional, one line, at most 200 characters;
// changeable while the reservation is not over, under way too.
func TestReservationPurpose(t *testing.T) {
	s, sm, _, node := rsvRig(t, "pp")
	alice := forge(sm, "pp-alice", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // 10:00; now is 10:10
	post := func(start time.Time, purpose any) (int, rsvView) {
		b := map[string]any{"node": node, "start": start, "end": start.Add(time.Hour)}
		if purpose != nil {
			b["purpose"] = purpose
		}
		body, _ := json.Marshal(b)
		rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, alice, string(body))
		var v rsvView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	purposeOf := func(id string) string {
		for _, x := range listReservations(t, s, alice) {
			if x.ID == id {
				return x.Purpose
			}
		}
		return "<missing>"
	}
	patch := func(id, body string) int {
		return doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+id, map[string]string{"id": id}, alice, body).Code
	}
	code, v := post(slot, "跑 sglang 基准")
	if code != http.StatusCreated || v.Purpose != "跑 sglang 基准" {
		t.Fatalf("with a purpose: %d %+v", code, v)
	}
	if p := purposeOf(v.ID); p != "跑 sglang 基准" {
		t.Fatalf("listed: %q", p)
	}
	if c := patch(v.ID, `{"purpose":"改"}`); c != http.StatusOK {
		t.Fatalf("changing the purpose of one under way: %d", c)
	}
	if p := purposeOf(v.ID); p != "改" {
		t.Fatalf("changed: %q", p)
	}
	if c := patch(v.ID, `{"purpose":""}`); c != http.StatusOK {
		t.Fatalf("clearing: %d", c)
	}
	if p := purposeOf(v.ID); p != "" {
		t.Fatalf("cleared: %q", p)
	}
	if code, v := post(slot.Add(2*time.Hour), strings.Repeat("测", 200)); code != http.StatusCreated {
		t.Fatalf("200 characters: %d %+v", code, v)
	}
	if code, _ := post(slot.Add(4*time.Hour), strings.Repeat("测", 201)); code != http.StatusBadRequest {
		t.Fatalf("201 characters: %d", code)
	}
	if code, _ := post(slot.Add(6*time.Hour), "a\nb"); code != http.StatusBadRequest {
		t.Fatalf("two lines: %d", code)
	}
}

// A 管理预约 grant on a machine makes its reservations the grantee's to
// run, as an admin's — even where the rules keep the grantee off it; the
// one reserved for must still be let use it.
func TestReservationsManagedByGrant(t *testing.T) {
	s, sm, _, node := rsvRig(t, "mg")
	grantNow(t, s, "mg-keeper", "user = mg-carol and machine = mg-n1", devpodv1alpha1.PermManageReservations)
	setRules(t, devpodv1alpha1.AccessRule{Match: "user in (mg-carol, mg-dave) and machine = mg-n1"})
	alice, bob, carol := forge(sm, "mg-alice", false), forge(sm, "mg-bob", false), forge(sm, "mg-carol", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // 10:00; now is 10:10
	code, a := reserveAs(t, s, alice, node, slot, slot.Add(time.Hour))
	if code != http.StatusCreated {
		t.Fatalf("alice: %d %+v", code, a)
	}
	seen := func(c *http.Cookie) (bool, bool) {
		for _, v := range listReservations(t, s, c) {
			if v.ID == a.ID {
				return true, v.CanManage
			}
		}
		return false, false
	}
	if ok, can := seen(carol); !ok || !can {
		t.Errorf("carol (granted, kept off the machine): seen %v, can manage %v", ok, can)
	}
	if ok, can := seen(bob); !ok || can {
		t.Errorf("bob: seen %v, can manage %v", ok, can)
	}
	if ok, can := seen(alice); !ok || !can {
		t.Errorf("alice, hers: seen %v, can manage %v", ok, can)
	}
	call := func(h http.HandlerFunc, c *http.Cookie, method, body string) int {
		return doJSON(t, h, method, "/api/reservations/"+a.ID, map[string]string{"id": a.ID}, c, body).Code
	}
	end := fmt.Sprintf(`{"end":%q}`, slot.Add(2*time.Hour).Format(time.RFC3339))
	if c := call(s.HandlePatchReservationForTest(), bob, "PATCH", end); c != http.StatusForbidden {
		t.Errorf("bob moving alice's end: %d", c)
	}
	if c := call(s.HandlePatchReservationForTest(), carol, "PATCH", end); c != http.StatusOK {
		t.Errorf("carol moving alice's end: %d", c)
	}
	if c := call(s.HandleDeleteReservationForTest(), bob, "DELETE", ""); c != http.StatusForbidden {
		t.Errorf("bob ending alice's: %d", c)
	}
	if c := call(s.HandleDeleteReservationForTest(), carol, "DELETE", ""); c != http.StatusNoContent {
		t.Errorf("carol ending alice's: %d", c)
	}
	reserveFor := func(user string) (int, rsvView) {
		body, _ := json.Marshal(map[string]any{"node": node, "start": slot.Add(3 * time.Hour), "end": slot.Add(4 * time.Hour), "user": user})
		rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, carol, string(body))
		var v rsvView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	if code, v := reserveFor("mg-bob"); code != http.StatusCreated || v.User != "mg-bob" {
		t.Errorf("carol reserving for bob: %d %+v", code, v)
	}
	if code, v := reserveFor("mg-carol"); code != http.StatusForbidden || v.Code != "ACL_DENIED" {
		t.Errorf("carol reserving for herself, kept off: %d %+v", code, v)
	}
	if code, v := reserveFor("mg-dave"); code != http.StatusForbidden || v.Code != "ACL_DENIED" {
		t.Errorf("carol reserving for dave, kept off: %d %+v", code, v)
	}
	rec := doJSON(t, s.HandleNodesForTest(), "GET", "/api/nodes", nil, carol, "")
	var out struct {
		Items []struct {
			Name               string `json:"name"`
			ManageReservations bool   `json:"manageReservations"`
			Selectable         bool   `json:"selectable"`
		} `json:"items"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("nodes: %d %s", rec.Code, rec.Body)
	}
	found := false
	for _, n := range out.Items {
		if n.Name == node {
			found = true
			if !n.ManageReservations || n.Selectable {
				t.Errorf("carol's %s: %+v", node, n)
			}
		}
	}
	if !found {
		t.Errorf("carol's nodes lack %s", node)
	}
}

// A purpose-only change touches the purpose alone: the times are neither
// checked again (the rules may have changed since) nor written back.
func TestReservationPurposeOnlyPatchKeepsTimes(t *testing.T) {
	s, sm, _, node := rsvRig(t, "po")
	alice := forge(sm, "po-alice", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // 10:00; now is 10:10
	code, v := reserveAs(t, s, alice, node, slot, slot.Add(30*time.Minute))
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	s.Reservations.Granularity = time.Hour // 10:30 is no longer on a step
	rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+v.ID, map[string]string{"id": v.ID}, alice, `{"purpose":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("purpose only: %d %s", rec.Code, rec.Body)
	}
	for _, x := range listReservations(t, s, alice) {
		if x.ID == v.ID && (x.Purpose != "x" || !x.Start.Equal(slot) || !x.End.Equal(slot.Add(30*time.Minute))) {
			t.Fatalf("after: %+v", x)
		}
	}
}

// A 管理预约 grant reaches the machines it names and no others.
func TestReservationGrantLimitedToItsMachine(t *testing.T) {
	s, sm, _, _ := rsvRig(t, "gl")
	admin := forge(sm, "gl-root", true)
	if c := doJSON(t, s.Routes().ServeHTTP, "PUT", "/api/admin/nodes/gl-n2/reservable", nil, admin, `{"reservable":true}`).Code; c != http.StatusOK {
		t.Fatalf("opening gl-n2: %d", c)
	}
	grantNow(t, s, "gl-keeper", "user = gl-carol and machine = gl-n1", devpodv1alpha1.PermManageReservations)
	alice, carol := forge(sm, "gl-alice", false), forge(sm, "gl-carol", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	var a rsvView
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var code int
		if code, a = reserveAs(t, s, alice, "gl-n2", slot, slot.Add(time.Hour)); code == http.StatusCreated {
			break
		}
	}
	if a.ID == "" {
		t.Fatalf("alice on gl-n2: %+v", a)
	}
	for _, v := range listReservations(t, s, carol) {
		if v.ID == a.ID && v.CanManage {
			t.Errorf("carol may manage a reservation of gl-n2: %+v", v)
		}
	}
	call := func(h http.HandlerFunc, method, body string) int {
		return doJSON(t, h, method, "/api/reservations/"+a.ID, map[string]string{"id": a.ID}, carol, body).Code
	}
	if c := call(s.HandlePatchReservationForTest(), "PATCH", `{"purpose":"x"}`); c != http.StatusForbidden {
		t.Errorf("carol changing one on gl-n2: %d", c)
	}
	if c := call(s.HandleDeleteReservationForTest(), "DELETE", ""); c != http.StatusForbidden {
		t.Errorf("carol ending one on gl-n2: %d", c)
	}
	body, _ := json.Marshal(map[string]any{"node": "gl-n2", "start": slot.Add(2 * time.Hour), "end": slot.Add(3 * time.Hour), "user": "gl-bob"})
	if c := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, carol, string(body)).Code; c != http.StatusForbidden {
		t.Errorf("carol reserving gl-n2 for bob: %d", c)
	}
}

// A reservation's history keeps its purpose as it was at each change —
// the reservation object goes 90 days after it ends, the history stays.
func TestReservationHistoryKeepsPurpose(t *testing.T) {
	s, sm, _, node := rsvRig(t, "ph")
	st := &fakeAuditStore{}
	s.Audit = &webui.AuditDeps{Store: st}
	t.Cleanup(func() { s.Audit = nil })
	alice := forge(sm, "ph-alice", false)
	slot := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	body, _ := json.Marshal(map[string]any{"node": node, "start": slot, "end": slot.Add(time.Hour), "purpose": "跑基准"})
	rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, alice, string(body))
	var v rsvView
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	if c := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+v.ID, map[string]string{"id": v.ID}, alice, `{"purpose":"改了"}`).Code; c != http.StatusOK {
		t.Fatalf("patch: %d", c)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	got := map[string]string{}
	for _, r := range st.resv {
		if r.ID == v.ID {
			got[r.Event] = r.Purpose
		}
	}
	if got["created"] != "跑基准" || got["changed"] != "改了" {
		t.Fatalf("history purposes: %v", got)
	}
}

// A reservation's history keeps what it held: -1 for the whole machine.
func TestReservationHistoryAmounts(t *testing.T) {
	s, sm, _, node := rsvRig(t, "ha")
	st := &fakeAuditStore{}
	s.Audit = &webui.AuditDeps{Store: st}
	t.Cleanup(func() { s.Audit = nil })
	alice := forge(sm, "ha-alice", false)
	slot := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	_, part := reserveWith(t, s, alice, node, slot, slot.Add(time.Hour), map[string]any{"gpu": 4})
	_, whole := reserveAs(t, s, alice, node, slot.Add(time.Hour), slot.Add(2*time.Hour))
	st.mu.Lock()
	defer st.mu.Unlock()
	got := map[string][2]float64{}
	for _, r := range st.resv {
		if r.Event == "created" {
			got[r.ID] = [2]float64{float64(r.GPU), r.CPU}
		}
	}
	if got[part.ID] != [2]float64{4, 96} || got[whole.ID] != [2]float64{-1, -1} {
		t.Fatalf("history amounts: %v (part %s, whole %s)", got, part.ID, whole.ID)
	}
	st.mu.Unlock()
	// one written with cores alone (kubectl): no cards, not "whole"
	c16 := resource.MustParse("16")
	own := &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: "ha-cores"}, Spec: devpodv1alpha1.NodeReservationSpec{
		Node: node, User: "ha-alice", Start: metav1.NewTime(slot.Add(3 * time.Hour)), End: metav1.NewTime(slot.Add(4 * time.Hour)), CPU: &c16}}
	if err := k8sClient.Create(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/ha-cores", map[string]string{"id": "ha-cores"}, alice, `{"purpose":"y"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	st.mu.Lock()
	for _, r := range st.resv {
		if r.ID == "ha-cores" && (r.GPU != 0 || r.CPU != 16) {
			t.Fatalf("cores alone in history: %d %v", r.GPU, r.CPU)
		}
	}
}

// What a reservation holds is also kept in an annotation: a writer that
// does not know the amounts (an older console mid-rollout, or a CRD not
// yet updated) drops them from the spec, and they come back from it.
func TestReservationShareSurvivesOldWriter(t *testing.T) {
	s, sm, _, node := rsvRig(t, "sw2")
	ctx := context.Background()
	alice, bob := forge(sm, "sw2-alice", false), forge(sm, "sw2-bob", false)
	slot := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	code, v := reserveWith(t, s, alice, node, slot, slot.Add(time.Hour), map[string]any{"gpu": 4})
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	var nr devpodv1alpha1.NodeReservation
	if err := k8sClient.Get(ctx, client_key_cluster(v.ID), &nr); err != nil {
		t.Fatal(err)
	}
	nr.Spec.GPU, nr.Spec.CPU = nil, nil // as an older writer leaves it
	if err := k8sClient.Update(ctx, &nr); err != nil {
		t.Fatal(err)
	}
	for _, x := range listReservations(t, s, alice) {
		if x.ID == v.ID && (x.GPU == nil || *x.GPU != 4 || x.CPU == nil || *x.CPU != 96) {
			t.Fatalf("listed after an older writer: %+v", x)
		}
	}
	if code, w := reserveWith(t, s, bob, node, slot, slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("the other half beside it: %d %+v", code, w)
	}
	// a change through this console writes them back
	if rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+v.ID, map[string]string{"id": v.ID}, alice, `{"purpose":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { return k8sClient.Get(ctx, client_key_cluster(v.ID), &nr) == nil && nr.Spec.Purpose == "x" }) // the cache caught up
	if nr.Spec.GPU == nil || *nr.Spec.GPU != 4 {
		t.Fatalf("after a change: %+v", nr.Spec)
	}
	// any change keeps the annotation in step with the spec
	if rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+v.ID, map[string]string{"id": v.ID}, alice, `{"end":"`+slot.Add(30*time.Minute).Format(time.RFC3339)+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("shorten: %d %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool {
		return k8sClient.Get(ctx, client_key_cluster(v.ID), &nr) == nil && nr.Spec.End.Time.Equal(slot.Add(30*time.Minute))
	})
	if nr.Annotations[devpodv1alpha1.ShareAnnotation] != "gpu=4,cpu=96" {
		t.Fatalf("annotation after a change: %v", nr.Annotations)
	}
}
