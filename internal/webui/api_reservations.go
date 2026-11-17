// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// Node reservations (access control design §12): a reservation is one
// user's permit to use one reservable machine for a while — what the
// access rules ask of some users on some machines. Not exclusive, and
// reservations may overlap; when one ends, its holder's DevPods there that
// only it let run are hibernated (reservation_enforce.go).

// ReservationRules are the rule parameters (spec §3.1); nil on Server
// turns reservations off.
type ReservationRules struct {
	Granularity time.Duration // starts and ends align to it
	MaxDuration time.Duration // per reservation; 0 = unlimited
	MaxAdvance  time.Duration // how far ahead a start may be; 0 = unlimited
	ClearNotice time.Duration // how long before its end a holder is warned
	Now         func() time.Time
}

// reservationTZ is the zone times align to and are shown in (spec §3.1).
var reservationTZ = time.FixedZone("Asia/Shanghai", 8*3600)

func (ru *ReservationRules) now() time.Time {
	if ru.Now != nil {
		return ru.Now()
	}
	return time.Now()
}

// aligned: t falls on a granularity boundary of the local day.
func (ru *ReservationRules) aligned(t time.Time) bool {
	_, off := t.In(reservationTZ).Zone()
	return t.Nanosecond() == 0 && (t.Unix()+int64(off))%int64(ru.Granularity/time.Second) == 0
}

// slotStart is the start of the slot holding t.
func (ru *ReservationRules) slotStart(t time.Time) time.Time {
	_, off := t.In(reservationTZ).Zone()
	g := int64(ru.Granularity / time.Second)
	u := t.Unix() + int64(off)
	return time.Unix(u-u%g-int64(off), 0).UTC()
}

// validate checks a period that has not started against the rules.
func (ru *ReservationRules) validate(start, end time.Time) error {
	now := ru.now()
	switch {
	case !ru.aligned(start) || !ru.aligned(end):
		return fmt.Errorf("start and end must fall on %s boundaries", ru.Granularity)
	case !end.After(start):
		return fmt.Errorf("end must be after start")
	case ru.MaxDuration > 0 && end.Sub(start) > ru.MaxDuration:
		return fmt.Errorf("a reservation lasts at most %s", ru.MaxDuration)
	case start.Before(ru.slotStart(now)):
		return fmt.Errorf("start is in the past (the earliest is the current slot)")
	case ru.MaxAdvance > 0 && start.After(now.Add(ru.MaxAdvance)):
		return fmt.Errorf("a reservation starts at most %s ahead", ru.MaxAdvance)
	}
	return nil
}

// validateEnd checks a new end for a reservation that has started.
func (ru *ReservationRules) validateEnd(start, end time.Time) error {
	switch {
	case !ru.aligned(end):
		return fmt.Errorf("the end must fall on a %s boundary", ru.Granularity)
	case !end.After(ru.now()):
		return fmt.Errorf("the end must be in the future (release it to end it now)")
	case ru.MaxDuration > 0 && end.Sub(start) > ru.MaxDuration:
		return fmt.Errorf("a reservation lasts at most %s", ru.MaxDuration)
	}
	return nil
}

func (s *Server) reservationRules(w http.ResponseWriter) (*ReservationRules, bool) {
	if s.Reservations == nil {
		s.writeErr(w, http.StatusNotFound, "RESERVATIONS_DISABLED", "node reservations are not enabled", nil)
		return nil, false
	}
	return s.Reservations, true
}

// pinnedNode is the node a DevPod is pinned to ("" = scheduler's choice).
func pinnedNode(dp *devpodv1alpha1.DevPod) string {
	if dp.Spec.Pod == nil {
		return ""
	}
	return dp.Spec.Pod.Spec.NodeSelector[hostnameLabel]
}

// overAt: when a reservation stops counting — its end, or its cancel.
func overAt(nr *devpodv1alpha1.NodeReservation) time.Time {
	if c := nr.Spec.Cancelled; c != nil && c.At.Time.Before(nr.Spec.End.Time) {
		return c.At.Time
	}
	return nr.Spec.End.Time
}

/* ---- views ---- */

// reservationView is one reservation as the API shows it.
type reservationView struct {
	// Cluster: the cluster it is in, in a console of several.
	Cluster     string    `json:"cluster,omitempty"`
	ID          string    `json:"id"`
	Node        string    `json:"node"`
	User        string    `json:"user"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Phase       string    `json:"phase"` // Upcoming | Active | Ended | Cancelled
	Mine        bool      `json:"mine"`
	CancelledBy string    `json:"cancelledBy,omitempty"`
}

func viewOfReservation(nr *devpodv1alpha1.NodeReservation, me string, now time.Time) reservationView {
	v := reservationView{ID: nr.Name, Node: nr.Spec.Node, User: nr.Spec.User, Start: nr.Spec.Start.Time.UTC(), End: overAt(nr).UTC(), Mine: nr.Spec.User == me}
	switch {
	case nr.Spec.Cancelled != nil:
		v.Phase, v.CancelledBy = "Cancelled", nr.Spec.Cancelled.By
	case now.Before(nr.Spec.Start.Time):
		v.Phase = "Upcoming"
	case now.Before(nr.Spec.End.Time):
		v.Phase = "Active"
	default:
		v.Phase = "Ended"
	}
	return v
}

/* ---- handlers ---- */

// handleReservationRules serves GET /api/reservations/rules.
func (s *Server) handleReservationRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFrom(w, r); !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"granularitySeconds": int64(ru.Granularity / time.Second),
		"maxDurationSeconds": int64(ru.MaxDuration / time.Second),
		"maxAdvanceSeconds":  int64(ru.MaxAdvance / time.Second),
		"clearNoticeSeconds": int64(ru.ClearNotice / time.Second),
		"timezone":           reservationTZ.String(),
		"now":                ru.now().UTC(),
	})
}

// handleListReservations serves GET /api/reservations?from=&to=[&node=]:
// the timeline, visible to everyone (who reserved is public).
func (s *Server) handleListReservations(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	now := ru.now()
	from, to := now.Add(-24*time.Hour), now.Add(15*24*time.Hour)
	q := r.URL.Query()
	// mine=1: only the caller's, of any time (unless from/to say);
	// limit=N: a page of them, the latest start first, `next` naming the
	// page after (cursor=)
	mine := q.Get("mine") == "1" || q.Get("mine") == "true"
	if mine {
		from, to = time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "limit must be a positive number", nil)
			return
		}
		limit = n
	}
	var after *resCursor
	if v := q.Get("cursor"); v != "" {
		c, err := parseResCursor(v)
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "cursor is not one this list gave", nil)
			return
		}
		after = &c
	}
	for name, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", name+" must be RFC 3339", nil)
				return
			}
			*dst = t
		}
	}
	all, err := s.nodeReservations(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := []reservationView{}
	shown := s.clusterGate(r.Context(), sess)
	for _, cc := range s.ks(r) {
		ctx := s.in(r.Context(), cc)
		cluster := s.resCluster(ctx)
		open := shown(cc.Name)
		// others' reservations of a machine the rules keep the caller off
		// are not shown to them, as the machine is not
		var j *judge
		if !sess.Admin {
			if j, err = s.judgeFor(ctx, sess.User); err != nil {
				s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
				return
			}
		}
		allowed := map[string]bool{}
		usable := func(node string) bool {
			if j == nil {
				return true
			}
			if ok, seen := allowed[node]; seen {
				return ok
			}
			var n corev1.Node
			if err := cc.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
				n.Name = node // gone, or away: judged by its name alone
			}
			allowed[node] = j.on(&n).Allow
			return allowed[node]
		}
		for i := range all {
			nr := &all[i]
			if nr.Spec.Cluster != cluster || (q.Get("node") != "" && q.Get("node") != nr.Spec.Node) {
				continue
			}
			v := viewOfReservation(nr, sess.User, now)
			if mine && !v.Mine {
				continue
			}
			if !v.Mine && (!open || !usable(nr.Spec.Node)) {
				continue // others' reservations in a cluster, or of a machine, not shown to them
			}
			if v.Start.Before(to) && from.Before(v.End) {
				v.Cluster = s.clusterLabel(cc)
				items = append(items, v)
			}
		}
	}
	if limit == 0 && after == nil {
		sort.SliceStable(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
		s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	// a page: the latest start first; those starting together in a fixed
	// order, so a page's edge between them loses or repeats none
	sort.Slice(items, func(i, j int) bool { return cursorOf(items[i]).before(cursorOf(items[j])) })
	if after != nil {
		i := sort.Search(len(items), func(i int) bool { return after.before(cursorOf(items[i])) })
		items = items[i:]
	}
	out := map[string]any{"items": items}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
		out["items"] = items
		out["next"] = cursorOf(items[limit-1]).String()
	}
	s.writeJSON(w, http.StatusOK, out)
}

// resCursor: a place in a paged list of reservations — the latest start
// first, then by cluster and name.
type resCursor struct {
	start int64 // UnixNano
	key   string
}

func cursorOf(v reservationView) resCursor {
	return resCursor{start: v.Start.UnixNano(), key: v.Cluster + "/" + v.ID}
}

// before: c comes before d in the list.
func (c resCursor) before(d resCursor) bool {
	if c.start != d.start {
		return c.start > d.start
	}
	return c.key < d.key
}

func (c resCursor) String() string { return strconv.FormatInt(c.start, 10) + "~" + c.key }

func parseResCursor(s string) (resCursor, error) {
	at, key, ok := strings.Cut(s, "~")
	n, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || !strings.Contains(key, "/") {
		return resCursor{}, fmt.Errorf("bad cursor %q", s)
	}
	return resCursor{start: n, key: key}, nil
}

// reservableNode: the node, which must exist and be marked reservable.
func (s *Server) reservableNode(ctx context.Context, name string) (*corev1.Node, *serverErr) {
	var n corev1.Node
	if err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("node %q not found", name), nil}
	}
	if n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" {
		return nil, &serverErr{http.StatusBadRequest, "NOT_RESERVABLE", fmt.Sprintf("node %s is not open for reservation", name), nil}
	}
	return &n, nil
}

func randSuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// handleCreateReservation serves POST /api/reservations {node, start, end,
// user?} (user: an admin reserving for someone). Only where the rules ask
// that user for a reservation.
func (s *Server) handleCreateReservation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	// gateways before reservations became permits offer "--dry-run": a
	// preview of DevPods to clear, which no longer exists. Refused, so
	// that the ask does not reserve for real.
	if r.URL.Query().Has("dryRun") {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "there is no dry run: a reservation no longer hibernates anyone when it starts; leave out --dry-run", nil)
		return
	}
	var req struct {
		Node  string    `json:"node"`
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
		User  string    `json:"user"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	user := sess.User
	if req.User != "" && req.User != sess.User {
		if !sess.Admin {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "only an admin reserves for someone else", nil)
			return
		}
		user = req.User
	}
	n, apiErr := s.reservableNode(r.Context(), req.Node)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if err := ru.validate(req.Start, req.End); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	cluster := s.resCluster(r.Context())
	d, admin, err := s.decide(r.Context(), user, cluster, access.Node{Name: n.Name, Labels: n.Labels})
	switch {
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	case !d.Allow:
		s.writeErr(w, http.StatusForbidden, "ACL_DENIED", fmt.Sprintf("%s may not use node %s", user, n.Name), nil)
		return
	case admin || !d.NeedsReservation:
		s.writeErr(w, http.StatusBadRequest, "NO_NEED", fmt.Sprintf("%s may use node %s without a reservation", user, n.Name), nil)
		return
	}
	nr := &devpodv1alpha1.NodeReservation{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s-%s", n.Name, user, randSuffix(6))},
		Spec:       devpodv1alpha1.NodeReservationSpec{Cluster: cluster, Node: n.Name, User: user, Start: metav1.NewTime(req.Start.UTC()), End: metav1.NewTime(req.End.UTC())},
	}
	if err := s.createReservation(r.Context(), nr); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("reserve", err), nil)
		return
	}
	s.reservationHistory(r.Context(), nr, "created")
	s.writeJSON(w, http.StatusCreated, viewOfReservation(nr, sess.User, ru.now()))
}

// ownReservation loads reservation id of the request's cluster for a change
// by its holder or an admin; not one that is over.
func (s *Server) ownReservation(w http.ResponseWriter, r *http.Request, sess Session, now time.Time) (*devpodv1alpha1.NodeReservation, bool) {
	var nr devpodv1alpha1.NodeReservation
	err := s.Reader.Get(r.Context(), types.NamespacedName{Name: r.PathValue("id")}, &nr)
	if err != nil || nr.Spec.Cluster != s.resCluster(r.Context()) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such reservation", nil)
		return nil, false
	}
	if nr.Spec.User != sess.User && !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "this is someone else's reservation", nil)
		return nil, false
	}
	if !now.Before(overAt(&nr)) {
		s.writeErr(w, http.StatusConflict, "RESERVATION_OVER", "this reservation is already over", nil)
		return nil, false
	}
	return &nr, true
}

// handlePatchReservation serves PATCH /api/reservations/{id} {start?,
// end?}: one that has started can only have its end moved.
func (s *Server) handlePatchReservation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	var req struct {
		Start *time.Time `json:"start"`
		End   *time.Time `json:"end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Start == nil && req.End == nil) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "send start and / or end", nil)
		return
	}
	now := ru.now()
	nr, ok := s.ownReservation(w, r, sess, now)
	if !ok {
		return
	}
	start, end := nr.Spec.Start.Time, nr.Spec.End.Time
	if req.Start != nil {
		start = req.Start.UTC()
	}
	if req.End != nil {
		end = req.End.UTC()
	}
	started := !now.Before(nr.Spec.Start.Time)
	var err error
	switch {
	case started && !start.Equal(nr.Spec.Start.Time):
		err = fmt.Errorf("a reservation that has started can only have its end moved")
	case started:
		err = ru.validateEnd(start, end)
	default:
		err = ru.validate(start, end)
	}
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	changed, err := s.changeReservation(r.Context(), nr.Name, func(spec *devpodv1alpha1.NodeReservationSpec) bool {
		spec.Start, spec.End = metav1.NewTime(start), metav1.NewTime(end)
		return true
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, viewOfReservation(changed, sess.User, now))
}

// handleDeleteReservation serves DELETE /api/reservations/{id}: one that
// has not started goes; one that has ends now. Holders and admins.
func (s *Server) handleDeleteReservation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	now := ru.now()
	nr, ok := s.ownReservation(w, r, sess, now)
	if !ok {
		return
	}
	var err error
	if now.Before(nr.Spec.Start.Time) {
		if err = s.deleteReservation(r.Context(), nr.Name); err == nil {
			s.reservationHistory(r.Context(), nr, "deleted")
		}
	} else {
		_, err = s.changeReservation(r.Context(), nr.Name, func(spec *devpodv1alpha1.NodeReservationSpec) bool {
			spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: sess.User, At: metav1.NewTime(now.UTC().Truncate(time.Second))}
			return true
		})
	}
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetReservable serves PUT /api/admin/nodes/{name}/reservable
// {reservable}: the node label that opens a node for reservation.
// Turning it off leaves reservations already made to run their course.
func (s *Server) handleSetReservable(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	var req struct {
		Reservable *bool `json:"reservable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reservable == nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "send {reservable: true|false}", nil)
		return
	}
	name := r.PathValue("name")
	var value any // null removes the label
	if *req.Reservable {
		value = "true"
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{devpodv1alpha1.ReservableNodeLabel: value}}})
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := s.k(r).Client.Patch(r.Context(), node, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "node not found", nil)
			return
		}
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	all, err := s.nodeReservations(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"reservable": *req.Reservable, "pending": pendingOn(all, s.resCluster(r.Context()), name, ru.now())})
}

// pendingOn counts the node's reservations that are not over.
func pendingOn(all []devpodv1alpha1.NodeReservation, cluster, node string, now time.Time) int {
	n := 0
	for i := range all {
		if all[i].Spec.Cluster == cluster && all[i].Spec.Node == node && now.Before(overAt(&all[i])) {
			n++
		}
	}
	return n
}

/* ---- node choices ---- */

// nodeHold: a reservation of the caller's that covers now, until when.
type nodeHold struct {
	User  string    `json:"user"`
	Until time.Time `json:"until"`
}

// nodeChoice is one node in GET /api/nodes.
type nodeChoice struct {
	// Cluster: the cluster it is in, in a console of several.
	Cluster    string  `json:"cluster,omitempty"`
	Name       string  `json:"name"`
	Ready      bool    `json:"ready"`
	CPU        resView `json:"cpu"`
	Mem        resView `json:"mem"`
	GPUModel   string  `json:"gpuModel,omitempty"`
	GPUTotal   int     `json:"gpuTotal"`
	GPUUsed    int     `json:"gpuUsed"`
	Reservable bool    `json:"reservable"`
	// Access: what it is to the caller — "ok", "reservation" (the rules
	// ask for one they do not hold now) or "denied".
	Access string `json:"access"`
	// ReservationRequired: the caller may use it only under a reservation.
	ReservationRequired bool      `json:"reservationRequired"`
	Pending             int       `json:"pending"` // reservations not over yet
	Mine                *nodeHold `json:"mine"`    // the caller's reservation covering now
	// Selectable: the caller's own DevPods may be pinned here now.
	Selectable bool   `json:"selectable"`
	Reason     string `json:"reason,omitempty"`
}

// handleNodes serves GET /api/nodes: the nodes a user may pick for a
// DevPod, with capacity and reservation state but nothing about other
// users' DevPods.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	items := []nodeChoice{}
	shown := s.clusterGate(r.Context(), sess)
	for _, cc := range s.ks(r) {
		if !shown(cc.Name) {
			continue // a cluster the rules keep them off: not shown
		}
		part, err := s.nodeChoicesIn(s.in(r.Context(), cc), sess)
		if away(err) {
			continue
		}
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		for i := range part {
			part[i].Cluster = s.clusterLabel(cc)
		}
		items = append(items, part...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Cluster != items[j].Cluster {
			return items[i].Cluster < items[j].Cluster
		}
		return items[i].Name < items[j].Name
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// nodeChoicesIn: the nodes of ctx's cluster, as the caller may use them.
func (s *Server) nodeChoicesIn(ctx context.Context, sess Session) ([]nodeChoice, error) {
	views, _, _, _ := s.nodeViews(ctx)
	var all []devpodv1alpha1.NodeReservation
	now := time.Now()
	if s.Reservations != nil {
		var err error
		if all, err = s.nodeReservations(ctx); err != nil {
			return nil, err
		}
		now = s.Reservations.now()
	}
	cluster := s.resCluster(ctx)
	j, err := s.judgeFor(ctx, sess.User)
	if err != nil {
		return nil, err
	}
	items := make([]nodeChoice, 0, len(views))
	for _, v := range views {
		c := nodeChoice{Name: v.Name, Ready: v.Ready, GPUModel: v.GPU.Model, GPUTotal: v.GPU.Total, GPUUsed: v.GPU.Used,
			CPU: resView{Allocatable: v.CPU.Allocatable, Allocated: v.CPU.Allocated}, Mem: resView{Allocatable: v.Mem.Allocatable, Allocated: v.Mem.Allocated},
			Reservable: v.Labels[devpodv1alpha1.ReservableNodeLabel] == "true", Pending: pendingOn(all, cluster, v.Name, now), Selectable: true}
		for i := range all {
			nr := &all[i]
			if nr.Spec.Cluster == cluster && nr.Spec.Node == v.Name && nr.Spec.User == sess.User && !now.Before(nr.Spec.Start.Time) && now.Before(overAt(nr)) {
				c.Mine = &nodeHold{User: sess.User, Until: overAt(nr).UTC()}
			}
		}
		var d access.Decision
		c.Access, d = j.accessOf(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: v.Name, Labels: v.Labels}})
		c.ReservationRequired = d.NeedsReservation && !j.admin
		switch c.Access {
		case "denied":
			if !sess.Admin {
				continue // a machine the rules keep them off is not shown to them
			}
			c.Selectable, c.Reason = false, "no access"
		case "reservation":
			c.Selectable, c.Reason = false, "reservation required"
		}
		items = append(items, c)
	}
	return items, nil
}
