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
		if k8sClient.Get(ctx, client_key_cluster(node), &n) == nil || time.Now().After(deadline) {
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
	return devpodv1alpha1.AccessRule{Machines: []string{node}, Subject: devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: e}
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

// Reservations are permits: two people may hold the same machine at once.
func TestReservationsOverlap(t *testing.T) {
	s, sm, _, node := rsvRig(t, "ov")
	setRules(t, onNode(node, needsReservation))
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	for _, u := range []string{"ov-alice", "ov-bob"} {
		if code, v := reserveAs(t, s, forge(sm, u, false), node, slot, slot.Add(time.Hour)); code != http.StatusCreated {
			t.Fatalf("%s: %d %+v", u, code, v)
		}
	}
	if items := listReservations(t, s, forge(sm, "ov-alice", false)); len(items) != 2 {
		t.Fatalf("listed %d", len(items))
	}
}

// Only where the rules ask for a reservation, and only on reservable machines.
func TestReservationNeedsRule(t *testing.T) {
	s, sm, _, node := rsvRig(t, "nr")
	alice := forge(sm, "nr-alice", false)
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	setRules(t, onNode("nr-n2", needsReservation))
	if code, v := reserveAs(t, s, alice, "nr-n2", slot, slot.Add(time.Hour)); code != http.StatusBadRequest || v.Code != "NOT_RESERVABLE" {
		t.Fatalf("not reservable: %d %+v", code, v)
	}
	// no rule: everyone may use it without one
	if code, v := reserveAs(t, s, alice, node, slot, slot.Add(time.Hour)); code != http.StatusBadRequest || v.Code != "NO_NEED" {
		t.Fatalf("no need: %d %+v", code, v)
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
		devpodv1alpha1.AccessRule{Machines: []string{node}, Subject: devpodv1alpha1.AccessSubject{Kind: "user", Name: "re-carol"}, Effect: devpodv1alpha1.AccessEffect{Allow: true}},
		onNode(node, needsReservation))
	slot := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	now := slot.Add(10 * time.Minute)
	s.Reservations.Now = func() time.Time { return now }
	users := map[string]*http.Cookie{}
	for _, u := range []string{"re-alice", "re-bob", "re-carol"} {
		users[u] = forge(sm, u, false)
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, users[u], createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", u, rec.Code, rec.Body)
		}
		name := u + "-dev1"
		waitDevPod(t, name)
		if rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+name, map[string]string{"name": name}, users[u], fmt.Sprintf(`{"node":%q}`, node)); rec.Code != http.StatusOK {
			t.Fatalf("pin %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	reserveAs(t, s, users["re-alice"], node, slot, slot.Add(time.Hour))
	reserveAs(t, s, users["re-bob"], node, slot, slot.Add(time.Hour))
	reserveAs(t, s, users["re-bob"], node, slot.Add(30*time.Minute), slot.Add(2*time.Hour)) // still covers bob after 11:00
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
