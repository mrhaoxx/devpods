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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Node reservations (reservation spec §3, §7) and the node choice rule
// built on them: a DevPod may be pinned to a node unless someone other
// than its owner holds the node right now.

// ReservationRules are the cluster-wide rule parameters (spec §3.1);
// nil on Server turns reservations off.
type ReservationRules struct {
	Granularity time.Duration // starts and ends align to it
	MaxDuration time.Duration // per reservation; 0 = unlimited
	MaxAdvance  time.Duration // how far ahead a start may be; 0 = unlimited
	ClearNotice time.Duration // how long before a start others are warned
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

// validate checks a new period against the rules (not against other
// reservations).
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

func (s *Server) reservationRules(w http.ResponseWriter) (*ReservationRules, bool) {
	if s.Reservations == nil {
		s.writeErr(w, http.StatusNotFound, "RESERVATIONS_DISABLED", "node reservations are not enabled", nil)
		return nil, false
	}
	return s.Reservations, true
}

// allReservations reads from the API server: conflict resolution and
// the node rule must not act on a stale cache.
func (s *Server) allReservations(ctx context.Context) ([]devpodv1alpha1.Reservation, error) {
	var list devpodv1alpha1.ReservationList
	if err := s.kc(ctx).Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	// one being deleted (it can linger on the controller's finalizer) is gone
	out := list.Items[:0]
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp == nil {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// effectiveReservations drops conflict losers: a live reservation that
// overlaps an earlier-created live one on the same node counts for
// nothing — the controller ignores it too, so both agree on who holds
// a node.
func effectiveReservations(all []devpodv1alpha1.Reservation) []devpodv1alpha1.Reservation {
	out := make([]devpodv1alpha1.Reservation, 0, len(all))
	for i := range all {
		r := &all[i]
		loser := false
		if r.Spec.Cancelled == nil {
			for j := range all {
				o := &all[j]
				if i != j && o.Spec.Node == r.Spec.Node && o.Overlaps(r.Spec.Start, r.Spec.End) && reservationBefore(o, r) {
					loser = true
					break
				}
			}
		}
		if !loser {
			out = append(out, *r)
		}
	}
	return out
}

// holderOf returns the reservation holding node at t, if any.
func holderOf(all []devpodv1alpha1.Reservation, node string, t time.Time) *devpodv1alpha1.Reservation {
	mt := metav1.NewTime(t)
	for i := range all {
		if all[i].Spec.Node == node && all[i].HeldAt(mt) {
			return &all[i]
		}
	}
	return nil
}

// heldUntil follows the holder's extensions: when the node is really free.
func heldUntil(all []devpodv1alpha1.Reservation, h *devpodv1alpha1.Reservation) time.Time {
	end := h.Spec.End.Time
	for moved := true; moved; {
		moved = false
		for i := range all {
			r := &all[i]
			if r.Spec.Cancelled == nil && r.Spec.Node == h.Spec.Node && r.Spec.User == h.Spec.User && r.Spec.Start.Time.Equal(end) {
				end, moved = r.Spec.End.Time, true
			}
		}
	}
	return end
}

// pinnedNode is the node a DevPod is pinned to ("" = scheduler's choice).
func pinnedNode(dp *devpodv1alpha1.DevPod) string {
	if dp.Spec.Pod == nil {
		return ""
	}
	return dp.Spec.Pod.Spec.NodeSelector[hostnameLabel]
}

// pinErr is the node choice rule: a DevPod owned by owner may sit on
// node unless someone else holds the node now. It is judged by the
// DevPod's owner, whoever performs the action (admins included).
func (s *Server) pinErr(ctx context.Context, owner, node string) *serverErr {
	if s.Reservations == nil || node == "" {
		return nil
	}
	all, err := s.allReservations(ctx)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	all = effectiveReservations(all)
	h := holderOf(all, node, s.Reservations.now())
	if h == nil || h.Spec.User == owner {
		return nil
	}
	until := heldUntil(all, h)
	return &serverErr{http.StatusConflict, "NODE_RESERVED",
		fmt.Sprintf("node %s is reserved by %s until %s", node, h.Spec.User, until.In(reservationTZ).Format("01-02 15:04")),
		map[string]any{"node": node, "user": h.Spec.User, "until": until.UTC()}}
}

/* ---- views ---- */

// reservationView is one block on the timeline: consecutive segments of
// one user (an original plus its extensions) merged.
type reservationView struct {
	ID          string    `json:"id"` // the first segment
	Segments    []string  `json:"segments"`
	Node        string    `json:"node"`
	User        string    `json:"user"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Phase       string    `json:"phase"` // Upcoming | Active | Ended | Cancelled
	Mine        bool      `json:"mine"`
	CancelledBy string    `json:"cancelledBy,omitempty"`
}

// mergeReservations builds the blocks: live segments of the same node
// and user that touch end to start become one; cancelled ones stay apart.
func mergeReservations(all []devpodv1alpha1.Reservation, me string, now time.Time) []reservationView {
	sorted := append([]devpodv1alpha1.Reservation(nil), all...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := &sorted[i], &sorted[j]
		if !a.Spec.Start.Time.Equal(b.Spec.Start.Time) {
			return a.Spec.Start.Time.Before(b.Spec.Start.Time)
		}
		return a.Name < b.Name
	})
	var out []reservationView
	open := map[string]int{} // node/user → index of the block still growing
	for i := range sorted {
		r := &sorted[i]
		if r.Spec.Cancelled != nil {
			out = append(out, reservationView{ID: r.Name, Segments: []string{r.Name}, Node: r.Spec.Node, User: r.Spec.User,
				Start: r.Spec.Start.Time.UTC(), End: r.Spec.End.Time.UTC(), Phase: "Cancelled", Mine: r.Spec.User == me, CancelledBy: r.Spec.Cancelled.By})
			continue
		}
		key := r.Spec.Node + "/" + r.Spec.User
		if k, ok := open[key]; ok && out[k].End.Equal(r.Spec.Start.Time) {
			out[k].Segments = append(out[k].Segments, r.Name)
			out[k].End = r.Spec.End.Time.UTC()
			continue
		}
		open[key] = len(out)
		out = append(out, reservationView{ID: r.Name, Segments: []string{r.Name}, Node: r.Spec.Node, User: r.Spec.User,
			Start: r.Spec.Start.Time.UTC(), End: r.Spec.End.Time.UTC(), Mine: r.Spec.User == me})
	}
	for i := range out {
		if out[i].Phase == "Cancelled" {
			continue
		}
		switch {
		case now.Before(out[i].Start):
			out[i].Phase = "Upcoming"
		case now.Before(out[i].End):
			out[i].Phase = "Active"
		default:
			out[i].Phase = "Ended"
		}
	}
	if out == nil {
		out = []reservationView{}
	}
	return out
}

// blockOf finds the block containing segment id.
func blockOf(views []reservationView, id string) *reservationView {
	for i := range views {
		for _, seg := range views[i].Segments {
			if seg == id {
				return &views[i]
			}
		}
	}
	return nil
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
	all, err := s.allReservations(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := []reservationView{}
	for _, v := range mergeReservations(effectiveReservations(all), sess.User, now) {
		if v.Start.Before(to) && from.Before(v.End) && (q.Get("node") == "" || q.Get("node") == v.Node) {
			items = append(items, v)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) reservableNode(ctx context.Context, name string) *serverErr {
	var n corev1.Node
	if err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("node %q not found", name), nil}
	}
	if n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" {
		return &serverErr{http.StatusBadRequest, "NOT_RESERVABLE", fmt.Sprintf("node %q is not open for reservation", name), nil}
	}
	return nil
}

// conflictWith returns the first live reservation on node overlapping
// [start, end), ignoring the names in skip.
func conflictWith(all []devpodv1alpha1.Reservation, node string, start, end time.Time, skip map[string]bool) *devpodv1alpha1.Reservation {
	var hits []*devpodv1alpha1.Reservation
	for i := range all {
		r := &all[i]
		if r.Spec.Node == node && !skip[r.Name] && r.Overlaps(metav1.NewTime(start), metav1.NewTime(end)) {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(i, j int) bool { return reservationBefore(hits[i], hits[j]) })
	return hits[0]
}

// reservationBefore orders by (creationTimestamp, name): the earlier
// reservation wins a conflict (spec §3.3).
func reservationBefore(a, b *devpodv1alpha1.Reservation) bool {
	if !a.CreationTimestamp.Time.Equal(b.CreationTimestamp.Time) {
		return a.CreationTimestamp.Time.Before(b.CreationTimestamp.Time)
	}
	return a.Name < b.Name
}

func conflictErr(c *devpodv1alpha1.Reservation) *serverErr {
	return &serverErr{http.StatusConflict, "RESERVATION_CONFLICT",
		fmt.Sprintf("%s already holds %s from %s to %s", c.Spec.User, c.Spec.Node,
			c.Spec.Start.Time.In(reservationTZ).Format("01-02 15:04"), c.Spec.End.Time.In(reservationTZ).Format("01-02 15:04")),
		map[string]any{"id": c.Name, "node": c.Spec.Node, "user": c.Spec.User, "start": c.Spec.Start.Time.UTC(), "end": c.Spec.End.Time.UTC()}}
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

// reserve creates a segment and settles conflicts by creation order:
// every writer reads everything back from the API server, and only the
// earliest overlapping reservation stays (spec §3.3). skip names the
// caller's own segments being replaced.
func (s *Server) reserve(ctx context.Context, user, node string, start, end time.Time, extends string, skip map[string]bool) (*devpodv1alpha1.Reservation, *serverErr) {
	internal := func(err error) *serverErr {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	all, err := s.allReservations(ctx)
	if err != nil {
		return nil, internal(err)
	}
	if c := conflictWith(all, node, start, end, skip); c != nil {
		return nil, conflictErr(c)
	}
	res := &devpodv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s-%s", node, user, randSuffix(6))},
		Spec:       devpodv1alpha1.ReservationSpec{Node: node, User: user, Start: metav1.NewTime(start.UTC()), End: metav1.NewTime(end.UTC()), Extends: extends},
	}
	if err := s.kc(ctx).Client.Create(ctx, res); err != nil {
		return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "reservation rejected", err.Error()}
	}
	if all, err = s.allReservations(ctx); err != nil {
		return nil, internal(err)
	}
	skipSelf := map[string]bool{}
	for k := range skip {
		skipSelf[k] = true
	}
	var self *devpodv1alpha1.Reservation
	for i := range all {
		if all[i].Name == res.Name {
			self = &all[i]
		}
	}
	if self == nil {
		return nil, internal(fmt.Errorf("reservation %s vanished", res.Name))
	}
	skipSelf[res.Name] = true
	if c := conflictWith(all, node, start, end, skipSelf); c != nil && reservationBefore(c, self) {
		if err := s.kc(ctx).Client.Delete(ctx, res); err != nil && !apierrors.IsNotFound(err) {
			return nil, internal(err)
		}
		return nil, conflictErr(c)
	}
	return self, nil
}

// willHibernate counts other users' running DevPods on node: they are
// hibernated when a reservation by user starts.
func (s *Server) willHibernate(ctx context.Context, node, user string) int {
	n := 0
	for _, ref := range (&LiveInputs{S: s}).Build(ctx).DevPods {
		if ref.Node == node && ref.Owner != user && ref.Phase == string(devpodv1alpha1.DevPodRunning) {
			n++
		}
	}
	return n
}

// handleCreateReservation serves POST /api/reservations {node, start,
// end}. With ?dryRun=true nothing is created: the answer is
// {willHibernate} (how many other users' DevPods the start will
// hibernate), or the same 400 / 409 the real call would give.
func (s *Server) handleCreateReservation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	var req struct {
		Node  string    `json:"node"`
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	fail := func(e *serverErr) { s.writeErr(w, e.status, e.Code, e.Message, e.Detail) }
	if e := s.reservableNode(r.Context(), req.Node); e != nil {
		fail(e)
		return
	}
	if err := ru.validate(req.Start, req.End); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	if r.URL.Query().Get("dryRun") == "true" {
		all, err := s.allReservations(r.Context())
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		if c := conflictWith(all, req.Node, req.Start, req.End, nil); c != nil {
			fail(conflictErr(c))
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"willHibernate": s.willHibernate(r.Context(), req.Node, sess.User)})
		return
	}
	res, e := s.reserve(r.Context(), sess.User, req.Node, req.Start, req.End, "", nil)
	if e != nil {
		fail(e)
		return
	}
	s.writeJSON(w, http.StatusCreated, mergeReservations([]devpodv1alpha1.Reservation{*res}, sess.User, ru.now())[0])
}

// cancelSegments writes spec.cancelled on the named segments.
func (s *Server) cancelSegments(ctx context.Context, names []string, by string, at time.Time) error {
	for _, name := range names {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var res devpodv1alpha1.Reservation
			if err := s.kc(ctx).Reader.Get(ctx, types.NamespacedName{Name: name}, &res); err != nil {
				return client.IgnoreNotFound(err)
			}
			if res.Spec.Cancelled != nil || !at.Before(res.Spec.End.Time) {
				return nil // already over
			}
			res.Spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: by, At: metav1.NewTime(at.UTC().Truncate(time.Second))}
			return s.kc(ctx).Client.Update(ctx, &res)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ownBlock loads the block holding segment id and checks the caller may
// change it.
func (s *Server) ownBlock(w http.ResponseWriter, r *http.Request, sess Session, ru *ReservationRules) (*reservationView, []devpodv1alpha1.Reservation, bool) {
	all, err := s.allReservations(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return nil, nil, false
	}
	b := blockOf(mergeReservations(all, sess.User, ru.now()), r.PathValue("id"))
	if b == nil {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such reservation", nil)
		return nil, nil, false
	}
	if b.User != sess.User && !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "this is someone else's reservation", nil)
		return nil, nil, false
	}
	if b.Phase == "Cancelled" || b.Phase == "Ended" {
		s.writeErr(w, http.StatusConflict, "RESERVATION_OVER", "this reservation is already over", nil)
		return nil, nil, false
	}
	return b, all, true
}

// handleDeleteReservation serves DELETE /api/reservations/{id}: cancel
// (not started) or release early (started). Owners and admins.
func (s *Server) handleDeleteReservation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ru, ok := s.reservationRules(w)
	if !ok {
		return
	}
	b, _, ok := s.ownBlock(w, r, sess, ru)
	if !ok {
		return
	}
	if err := s.cancelSegments(r.Context(), b.Segments, sess.User, ru.now()); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePatchReservation serves PATCH /api/reservations/{id} {start?,
// end?}. Not started: the block is cancelled and reserved anew for the
// new period (it queues as a new reservation). Started: only a later
// end is accepted, which adds an extension segment; the original is
// untouched if the extension loses a conflict.
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
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || (req.Start == nil && req.End == nil) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "send start and / or end", nil)
		return
	}
	b, all, ok := s.ownBlock(w, r, sess, ru)
	if !ok {
		return
	}
	fail := func(e *serverErr) { s.writeErr(w, e.status, e.Code, e.Message, e.Detail) }
	bad := func(msg string) { s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", msg, nil) }
	now := ru.now()
	start, end := b.Start, b.End
	if req.Start != nil {
		start = *req.Start
	}
	if req.End != nil {
		end = *req.End
	}
	if e := s.reservableNode(r.Context(), b.Node); e != nil {
		fail(e)
		return
	}
	if b.Phase == "Active" {
		if !start.Equal(b.Start) || !end.After(b.End) {
			bad("a reservation that has started can only be extended; release it early to end it sooner")
			return
		}
		if !ru.aligned(end) || (ru.MaxDuration > 0 && end.Sub(b.End) > ru.MaxDuration) {
			bad(fmt.Sprintf("the new end must fall on a %s boundary, at most %s later", ru.Granularity, ru.MaxDuration))
			return
		}
		if _, e := s.reserve(r.Context(), b.User, b.Node, b.End, end, b.Segments[len(b.Segments)-1], nil); e != nil {
			fail(e)
			return
		}
	} else {
		if err := ru.validate(start, end); err != nil {
			bad(err.Error())
			return
		}
		skip := map[string]bool{}
		for _, seg := range b.Segments {
			skip[seg] = true
		}
		// someone else in the new period: refuse before giving up the old one
		if c := conflictWith(all, b.Node, start, end, skip); c != nil {
			fail(conflictErr(c))
			return
		}
		if err := s.cancelSegments(r.Context(), b.Segments, sess.User, now); err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		if _, e := s.reserve(r.Context(), b.User, b.Node, start, end, "", nil); e != nil {
			fail(e)
			return
		}
	}
	fresh, err := s.allReservations(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	for _, v := range mergeReservations(fresh, sess.User, now) {
		if v.Node == b.Node && v.User == b.User && v.Phase != "Cancelled" && v.End.Equal(end) {
			s.writeJSON(w, http.StatusOK, v)
			return
		}
	}
	s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "the changed reservation was not found", nil)
}

// handleSetReservable serves PUT /api/admin/nodes/{name}/reservable
// {reservable}: the node label. Reservations already made run on;
// `pending` says how many are not over yet.
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
	all, err := s.allReservations(r.Context())
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"reservable": *req.Reservable, "pending": pendingOn(all, name, ru.now())})
}

// pendingOn counts the node's reservations that are not over.
func pendingOn(all []devpodv1alpha1.Reservation, node string, now time.Time) int {
	n := 0
	for i := range all {
		if all[i].Spec.Node == node && all[i].Spec.Cancelled == nil && now.Before(all[i].Spec.End.Time) {
			n++
		}
	}
	return n
}

/* ---- the node list a user picks from ---- */

type nodeHold struct {
	User  string    `json:"user"`
	Until time.Time `json:"until"`
}
type nodeNext struct {
	User  string    `json:"user"`
	Start time.Time `json:"start"`
}

// nodeChoice is one node in GET /api/nodes.
type nodeChoice struct {
	Name       string    `json:"name"`
	Ready      bool      `json:"ready"`
	CPU        resView   `json:"cpu"`
	Mem        resView   `json:"mem"`
	GPUModel   string    `json:"gpuModel,omitempty"`
	GPUTotal   int       `json:"gpuTotal"`
	GPUUsed    int       `json:"gpuUsed"`
	Reservable bool      `json:"reservable"`
	Pending    int       `json:"pending"` // reservations not over yet
	Held       *nodeHold `json:"held"`    // who holds it now
	Next       *nodeNext `json:"next"`    // the next reservation by someone else
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
	views, _, _, _ := s.nodeViews(r.Context())
	var all []devpodv1alpha1.Reservation
	now := time.Now()
	if s.Reservations != nil {
		var err error
		if all, err = s.allReservations(r.Context()); err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		all = effectiveReservations(all)
		now = s.Reservations.now()
	}
	items := make([]nodeChoice, 0, len(views))
	for _, v := range views {
		c := nodeChoice{Name: v.Name, Ready: v.Ready, GPUModel: v.GPU.Model, GPUTotal: v.GPU.Total, GPUUsed: v.GPU.Used,
			CPU: resView{Allocatable: v.CPU.Allocatable, Allocated: v.CPU.Allocated}, Mem: resView{Allocatable: v.Mem.Allocatable, Allocated: v.Mem.Allocated},
			Reservable: v.Labels[devpodv1alpha1.ReservableNodeLabel] == "true", Pending: pendingOn(all, v.Name, now), Selectable: true}
		if h := holderOf(all, v.Name, now); h != nil {
			c.Held = &nodeHold{User: h.Spec.User, Until: heldUntil(all, h).UTC()}
			if h.Spec.User != sess.User {
				c.Selectable, c.Reason = false, fmt.Sprintf("reserved by %s", h.Spec.User)
			}
		}
		for i := range all {
			x := &all[i]
			if x.Spec.Node != v.Name || x.Spec.Cancelled != nil || x.Spec.User == sess.User || !x.Spec.Start.Time.After(now) {
				continue
			}
			if c.Next == nil || x.Spec.Start.Time.Before(c.Next.Start) {
				c.Next = &nodeNext{User: x.Spec.User, Start: x.Spec.Start.Time.UTC()}
			}
		}
		items = append(items, c)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
