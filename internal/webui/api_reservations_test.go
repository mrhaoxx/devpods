// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
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
	for name, reservable := range map[string]bool{node: true, prefix + "-n2": false} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		if reservable {
			n.Labels[devpodv1alpha1.ReservableNodeLabel] = "true"
		}
		if err := k8sClient.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, n) })
	}
	t.Cleanup(func() {
		var list devpodv1alpha1.ReservationList
		_ = k8sClient.List(ctx, &list)
		for i := range list.Items {
			_ = k8sClient.Delete(ctx, &list.Items[i])
		}
		cleanupDevPods(t)
	})
	// the nodes reach the cached client a moment later
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n corev1.Node
		if k8sClient.Get(ctx, client_key_cluster(node), &n) == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s, sm, now, node
}

type rsvView struct {
	ID       string    `json:"id"`
	Segments []string  `json:"segments"`
	Node     string    `json:"node"`
	User     string    `json:"user"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Phase    string    `json:"phase"`
	Mine     bool      `json:"mine"`
	Code     string    `json:"code"`
	Message  string    `json:"message"`
	Hib      *int      `json:"willHibernate"`
}

func rsvBody(node string, start, end time.Time) string {
	b, _ := json.Marshal(map[string]any{"node": node, "start": start, "end": end})
	return string(b)
}

func TestReservationRulesAndConflicts(t *testing.T) {
	s, sm, now, node := rsvRig(t, "rv")
	alice, bob, root := forge(sm, "rv-alice", false), forge(sm, "rv-bob", false), forge(sm, "rv-root", true)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC) // the current slot, 10:00
	post := func(c *http.Cookie, target, body string, want int) rsvView {
		t.Helper()
		rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", target, nil, c, body)
		if rec.Code != want {
			t.Fatalf("POST %s %s: %d (want %d) %s", target, body, rec.Code, want, rec.Body)
		}
		var v rsvView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}
	create := func(c *http.Cookie, start, end time.Time, want int) rsvView {
		t.Helper()
		return post(c, "/api/reservations", rsvBody(node, start, end), want)
	}

	t.Run("rules", func(t *testing.T) {
		create(alice, slot.Add(5*time.Minute), slot.Add(time.Hour), http.StatusBadRequest)                   // not aligned
		create(alice, slot.Add(time.Hour), slot.Add(time.Hour), http.StatusBadRequest)                       // empty
		create(alice, slot.Add(time.Hour), slot.Add(26*time.Hour), http.StatusBadRequest)                    // too long
		create(alice, slot.Add(-30*time.Minute), slot.Add(time.Hour), http.StatusBadRequest)                 // before the current slot
		create(alice, slot.Add(15*24*time.Hour), slot.Add(15*24*time.Hour+time.Hour), http.StatusBadRequest) // too far ahead
		post(alice, "/api/reservations", rsvBody("rv-n2", slot, slot.Add(time.Hour)), http.StatusBadRequest) // not reservable
		post(alice, "/api/reservations", rsvBody("rv-nope", slot, slot.Add(time.Hour)), http.StatusBadRequest)
	})

	t.Run("the current slot can be reserved; overlaps are refused with the holder", func(t *testing.T) {
		if v := post(alice, "/api/reservations?dryRun=true", rsvBody(node, slot, slot.Add(time.Hour)), http.StatusOK); v.Hib == nil || *v.Hib != 0 {
			t.Fatalf("dry run: %+v", v)
		}
		v := create(alice, slot, slot.Add(time.Hour), http.StatusCreated)
		if v.Phase != "Active" || !v.Mine || v.User != "rv-alice" || len(v.Segments) != 1 {
			t.Fatalf("created %+v", v)
		}
		c := create(bob, slot.Add(30*time.Minute), slot.Add(2*time.Hour), http.StatusConflict)
		if c.Code != "RESERVATION_CONFLICT" {
			t.Fatalf("conflict %+v", c)
		}
		post(bob, "/api/reservations?dryRun=true", rsvBody(node, slot.Add(30*time.Minute), slot.Add(2*time.Hour)), http.StatusConflict)
		create(bob, slot.Add(time.Hour), slot.Add(2*time.Hour), http.StatusCreated) // touching is not overlapping
	})

	t.Run("two users racing for one slot: exactly one wins", func(t *testing.T) {
		start, end := slot.Add(4*time.Hour), slot.Add(5*time.Hour)
		codes := make([]int, 8)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := forge(sm, fmt.Sprintf("rv-racer%d", i), false)
				codes[i] = doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, c, rsvBody(node, start, end)).Code
			}()
		}
		wg.Wait()
		won := 0
		for _, c := range codes {
			if c == http.StatusCreated {
				won++
			} else if c != http.StatusConflict {
				t.Fatalf("codes %v", codes)
			}
		}
		if won != 1 {
			t.Fatalf("winners %d: %v", won, codes)
		}
		var list devpodv1alpha1.ReservationList
		if err := s.Reader.List(context.Background(), &list); err != nil {
			t.Fatal(err)
		}
		live := 0
		for _, r := range list.Items {
			if r.Spec.Node == node && r.Spec.Start.Time.Equal(start) && r.Spec.Cancelled == nil {
				live++
			}
		}
		if live != 1 {
			t.Fatalf("%d reservations left for the slot", live)
		}
	})

	patch := func(c *http.Cookie, id, body string, want int) rsvView {
		t.Helper()
		rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+id, map[string]string{"id": id}, c, body)
		if rec.Code != want {
			t.Fatalf("PATCH %s %s: %d (want %d) %s", id, body, rec.Code, want, rec.Body)
		}
		var v rsvView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}
	del := func(c *http.Cookie, id string, want int) {
		t.Helper()
		if rec := doJSON(t, s.HandleDeleteReservationForTest(), "DELETE", "/api/reservations/"+id, map[string]string{"id": id}, c, ""); rec.Code != want {
			t.Fatalf("DELETE %s: %d (want %d) %s", id, rec.Code, want, rec.Body)
		}
	}
	list := func(c *http.Cookie) []rsvView {
		t.Helper()
		rec := doJSON(t, s.HandleListReservationsForTest(), "GET", "/api/reservations", nil, c, "")
		var out struct{ Items []rsvView }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return out.Items
	}
	mine := func(c *http.Cookie, start time.Time) rsvView {
		t.Helper()
		for _, v := range list(c) {
			if v.Mine && v.Start.Equal(start) && v.Phase != "Cancelled" {
				return v
			}
		}
		t.Fatalf("no block starting %s", start)
		return rsvView{}
	}

	t.Run("an extension is a new segment merged into the block; it loses to someone already there", func(t *testing.T) {
		// alice holds 10:00–11:00 (active), bob 11:00–12:00: no room to extend
		a := mine(alice, slot)
		patch(alice, a.ID, `{"end":"2026-11-12T03:30:00Z"}`, http.StatusConflict)
		if again := mine(alice, slot); !again.End.Equal(a.End) || len(again.Segments) != 1 {
			t.Fatalf("the original changed after a failed extension: %+v", again)
		}
		// bob (upcoming 11:00–12:00) extends... not started: a move. Move it to 12:00–13:00.
		b := mine(bob, slot.Add(time.Hour))
		moved := patch(bob, b.ID, `{"start":"2026-11-12T04:00:00Z","end":"2026-11-12T05:00:00Z"}`, http.StatusOK)
		if !moved.Start.Equal(slot.Add(2*time.Hour)) || moved.ID == b.ID {
			t.Fatalf("moved %+v", moved)
		}
		// now alice can extend to 11:30: one block, two segments
		ext := patch(alice, a.ID, `{"end":"2026-11-12T03:30:00Z"}`, http.StatusOK)
		if len(ext.Segments) != 2 || ext.ID != a.ID || !ext.End.Equal(slot.Add(90*time.Minute)) || ext.Phase != "Active" {
			t.Fatalf("extended %+v", ext)
		}
		// a started reservation cannot shrink or move
		patch(alice, a.ID, `{"end":"2026-11-12T02:30:00Z"}`, http.StatusBadRequest)
		patch(alice, a.ID, `{"start":"2026-11-12T02:30:00Z"}`, http.StatusBadRequest)
		// moving onto someone else's time is refused and keeps the old one
		patch(bob, moved.ID, `{"start":"2026-11-12T03:00:00Z","end":"2026-11-12T04:00:00Z"}`, http.StatusConflict)
		if still := mine(bob, slot.Add(2*time.Hour)); still.ID != moved.ID {
			t.Fatalf("bob lost his reservation: %+v", still)
		}
	})

	t.Run("cancel is early release for the whole block; owners and admins only", func(t *testing.T) {
		a := mine(alice, slot)
		del(bob, a.ID, http.StatusForbidden)
		del(alice, a.Segments[1], http.StatusNoContent) // any segment names the block
		var all devpodv1alpha1.ReservationList
		if err := s.Reader.List(context.Background(), &all); err != nil {
			t.Fatal(err)
		}
		for _, r := range all.Items {
			if r.Spec.User == "rv-alice" && r.Spec.Node == node && (r.Spec.Cancelled == nil || r.Spec.Cancelled.By != "rv-alice") {
				t.Fatalf("segment %s not cancelled: %+v", r.Name, r.Spec.Cancelled)
			}
		}
		del(alice, a.ID, http.StatusConflict) // already over
		// the slot is free again; an admin cancels bob's
		create(forge(sm, "rv-carol", false), slot, slot.Add(time.Hour), http.StatusCreated)
		b := mine(bob, slot.Add(2*time.Hour))
		del(root, b.ID, http.StatusNoContent)
		del(root, "rv-nope", http.StatusNotFound)
	})
	_ = now
}

// The node rule: a DevPod may be pinned to a node unless someone other
// than its owner holds the node now — whoever performs the action.
func TestNodeChoiceRule(t *testing.T) {
	s, sm, _, node := rsvRig(t, "nc")
	alice, bob, root := forge(sm, "nc-alice", false), forge(sm, "nc-bob", false), forge(sm, "nc-root", true)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for _, c := range []*http.Cookie{alice, bob} {
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, c, createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
	}
	waitDevPod(t, "nc-alice-dev1")
	waitDevPod(t, "nc-bob-dev1")
	patch := func(c *http.Cookie, name, body string, want int) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+name, map[string]string{"name": name}, c, body)
			if rec.Code == want {
				var e struct{ Code string }
				_ = json.Unmarshal(rec.Body.Bytes(), &e)
				return e.Code
			}
			if time.Now().After(deadline) {
				t.Fatalf("PATCH %s %s: %d (want %d) %s", name, body, rec.Code, want, rec.Body)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	pin := fmt.Sprintf(`{"node":%q}`, node)

	// nobody holds the node: anyone may pin their own DevPod, non-admins too
	patch(bob, "nc-bob-dev1", pin, http.StatusOK)
	patch(bob, "nc-bob-dev1", `{"node":""}`, http.StatusOK)
	patch(bob, "nc-bob-dev1", `{"node":"nc-nope"}`, http.StatusBadRequest)

	if rec := doJSON(t, s.HandleCreateReservationForTest(), "POST", "/api/reservations", nil, alice, rsvBody(node, slot, slot.Add(time.Hour))); rec.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	// alice holds it: her DevPod goes there, bob's does not — and an admin
	// acting on bob's DevPod is refused too, while one acting on alice's is not
	patch(alice, "nc-alice-dev1", pin, http.StatusOK)
	if code := patch(bob, "nc-bob-dev1", pin, http.StatusConflict); code != "NODE_RESERVED" {
		t.Fatalf("code %q", code)
	}
	patch(root, "nc-bob-dev1", pin, http.StatusConflict)
	patch(root, "nc-alice-dev1", `{"node":""}`, http.StatusOK)
	patch(root, "nc-alice-dev1", pin, http.StatusOK)

	// creating onto the held node: form path and YAML path
	body := func(name string) string {
		return fmt.Sprintf(`{"name":%q,"image":"ubuntu:24.04","cpu":"1","memory":"1Gi","node":%q}`, name, node)
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, bob, body("held")); rec.Code != http.StatusConflict {
		t.Fatalf("create onto a held node: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body("mine")); rec.Code != http.StatusCreated {
		t.Fatalf("holder creates: %d %s", rec.Code, rec.Body)
	}
	yaml, _ := json.Marshal(map[string]string{"yaml": fmt.Sprintf("apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: nc-bob-y\nspec:\n  owner: nc-bob\n  running: true\n  pod:\n    spec:\n      nodeSelector:\n        kubernetes.io/hostname: %s\n      containers:\n      - name: dev\n        image: ubuntu:24.04\n        resources:\n          limits: {cpu: \"1\", memory: 1Gi}\n", node)})
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, bob, string(yaml)); rec.Code != http.StatusConflict {
		t.Fatalf("yaml create onto a held node: %d %s", rec.Code, rec.Body)
	}
	// spec.nodeName would bypass the scheduler and this rule: it is
	// turned into the same pin, so it is judged the same way
	byName := func(name, owner string) string {
		b, _ := json.Marshal(map[string]string{"yaml": fmt.Sprintf("apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: %s\nspec:\n  owner: %s\n  running: true\n  pod:\n    spec:\n      nodeName: %s\n      containers:\n      - name: dev\n        image: ubuntu:24.04\n        resources:\n          limits: {cpu: \"1\", memory: 1Gi}\n", name, owner, node)})
		return string(b)
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, bob, byName("nc-bob-n", "nc-bob")); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "NODE_RESERVED") {
		t.Fatalf("yaml nodeName onto a held node: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, byName("nc-alice-n", "nc-alice")); rec.Code != http.StatusCreated {
		t.Fatalf("holder's yaml nodeName: %d %s", rec.Code, rec.Body)
	}
	var named devpodv1alpha1.DevPod
	if err := s.Reader.Get(ctx, client_key("nc-alice-n"), &named); err != nil {
		t.Fatal(err)
	}
	if named.Spec.Pod.Spec.NodeName != "" || named.Spec.Pod.Spec.NodeSelector["kubernetes.io/hostname"] != node {
		t.Fatalf("nodeName not turned into a pin: nodeName=%q selector=%v", named.Spec.Pod.Spec.NodeName, named.Spec.Pod.Spec.NodeSelector)
	}

	// starting a DevPod that was pinned there before the reservation began
	var dp devpodv1alpha1.DevPod
	if err := s.Reader.Get(ctx, client_key("nc-bob-dev1"), &dp); err != nil {
		t.Fatal(err)
	}
	dp.Spec.Running = false
	dp.Spec.Pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	if err := k8sClient.Update(ctx, &dp); err != nil {
		t.Fatal(err)
	}
	if code := patch(bob, "nc-bob-dev1", `{"running":true}`, http.StatusConflict); code != "NODE_RESERVED" {
		t.Fatalf("start code %q", code)
	}
	patch(bob, "nc-bob-dev1", `{"node":""}`, http.StatusOK) // unpinning is always allowed
	patch(bob, "nc-bob-dev1", `{"running":true}`, http.StatusOK)

	// the picker: held by alice → not selectable for bob, selectable for alice
	pick := func(c *http.Cookie) map[string]any {
		rec := doJSON(t, s.HandleNodesForTest(), "GET", "/api/nodes", nil, c, "")
		var out struct{ Items []map[string]any }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("nodes: %d %s", rec.Code, rec.Body)
		}
		for _, n := range out.Items {
			if n["name"] == node {
				return n
			}
		}
		t.Fatalf("node %s not listed: %s", node, rec.Body)
		return nil
	}
	if n := pick(bob); n["selectable"] != false || n["reservable"] != true || n["held"].(map[string]any)["user"] != "nc-alice" || n["reason"] == "" {
		t.Fatalf("bob sees %v", n)
	}
	if n := pick(alice); n["selectable"] != true {
		t.Fatalf("alice sees %v", n)
	}

	// the reservable switch
	set := func(c *http.Cookie, body string, want int) map[string]any {
		rec := doJSON(t, s.HandleSetReservableForTest(), "PUT", "/api/admin/nodes/"+node+"/reservable", map[string]string{"name": node}, c, body)
		if rec.Code != want {
			t.Fatalf("reservable %s: %d (want %d) %s", body, rec.Code, want, rec.Body)
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	set(alice, `{"reservable":false}`, http.StatusForbidden)
	if out := set(root, `{"reservable":false}`, http.StatusOK); out["pending"].(float64) != 1 {
		t.Fatalf("pending %v", out)
	}
	var n corev1.Node
	if err := s.Reader.Get(ctx, client_key_cluster(node), &n); err != nil || n.Labels[devpodv1alpha1.ReservableNodeLabel] != "" {
		t.Fatalf("label still set: %v %v", n.Labels, err)
	}
	set(root, `{"reservable":true}`, http.StatusOK)

	// a conflict loser (created later, overlapping) holds nothing — the
	// controller ignores it too — and one being deleted is gone
	mk := func(name, user string, finalizer bool) *devpodv1alpha1.Reservation {
		r := &devpodv1alpha1.Reservation{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: devpodv1alpha1.ReservationSpec{Node: node, User: user, Start: metav1.NewTime(slot), End: metav1.NewTime(slot.Add(time.Hour))}}
		if finalizer {
			r.Finalizers = []string{"devpod.io/reservation"}
		}
		time.Sleep(1100 * time.Millisecond) // creationTimestamp has second resolution
		if err := k8sClient.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	mk("nc-loser", "nc-bob", false)
	if n := pick(bob); n["selectable"] != false || n["held"].(map[string]any)["user"] != "nc-alice" {
		t.Fatalf("a conflict loser changed who holds the node: %v", n)
	}
	patch(bob, "nc-bob-dev1", pin, http.StatusConflict)
	// alice releases; bob's loser still does not count... but once hers is
	// being deleted (lingering on a finalizer) it is gone and the node is free
	var all devpodv1alpha1.ReservationList
	if err := s.Reader.List(ctx, &all); err != nil {
		t.Fatal(err)
	}
	for i := range all.Items {
		r := &all.Items[i]
		if r.Spec.Node != node {
			continue
		}
		r.Finalizers = []string{"devpod.io/reservation"}
		if err := k8sClient.Update(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := k8sClient.Delete(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n := pick(bob); n["selectable"] != true || n["held"] != nil {
		t.Fatalf("reservations being deleted still hold the node: %v", n)
	}
	patch(bob, "nc-bob-dev1", pin, http.StatusOK)
	// let them go
	if err := s.Reader.List(ctx, &all); err != nil {
		t.Fatal(err)
	}
	for i := range all.Items {
		all.Items[i].Finalizers = nil
		_ = k8sClient.Update(ctx, &all.Items[i])
	}
}

func client_key(name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: "devpods"}
}

func client_key_cluster(name string) types.NamespacedName { return types.NamespacedName{Name: name} }
