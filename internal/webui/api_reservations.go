// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// Node reservations (access control design §12; shares: design
// 2026-11-26): a reservation is one user's share of one reservable
// machine for a while — some of its cards and cores, or all of it — and
// a machine's reservations never add up to more than it has
// (reservation_capacity.go). It is also the permit the access rules ask
// of some users on some machines. When one starts, whoever is in its way
// is cleared after a notice; when one ends, its holder's DevPods there
// that only it let run are hibernated (reservation_enforce.go).

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
	Purpose     string    `json:"purpose,omitempty"`
	// CanManage: the caller may change or end it — theirs, an admin, or
	// a 管理预约 grant on its machine
	CanManage bool `json:"canManage"`
	// GPU and CPU: the cards and cores it holds; absent, the whole machine
	GPU *int32   `json:"gpu,omitempty"`
	CPU *float64 `json:"cpu,omitempty"`
	// Overbooked: it does not fit beside the machine's other reservations
	// (made before reservations held shares)
	Overbooked bool `json:"overbooked,omitempty"`
}

// mayManageReservation: the caller may run others' reservations of node
// (ctx's cluster) — an admin, or one granted 管理预约 on it (design
// 2026-11-24). A node that cannot be read is judged by its name.
func (s *Server) mayManageReservation(ctx context.Context, sess Session, node string) bool {
	if sess.Admin {
		return true
	}
	gs, groups, err := s.grantsFor(ctx, sess.User)
	if err != nil {
		return false
	}
	var n corev1.Node
	if err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		n.Name = node
	}
	return gs.Has(devpodv1alpha1.PermManageReservations, sess.User, groups, s.resCluster(ctx), &access.Node{Name: n.Name, Labels: n.Labels})
}

// validPurpose: a reservation's purpose — trimmed, one line, at most
// 200 characters.
func validPurpose(p string) (string, error) {
	p = strings.TrimSpace(p)
	if utf8.RuneCountInString(p) > 200 {
		return "", fmt.Errorf("the purpose is longer than 200 characters")
	}
	if strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("the purpose is one line, without control characters")
	}
	return p, nil
}

// shareOf: the share a request asks of n — gpu cards and cpu cores; both
// absent, the whole machine (nil, nil). Cards alone take their share of
// the cores (whole cores, rounded down); cores alone take no cards.
func shareOf(n *corev1.Node, gpu *int, cpu *float64) (*int32, *resource.Quantity, *serverErr) {
	if gpu == nil && cpu == nil {
		return nil, nil, nil
	}
	bad := func(format string, a ...any) (*int32, *resource.Quantity, *serverErr) {
		return nil, nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf(format, a...), nil}
	}
	c := capacityOf(n)
	var share amount
	if gpu != nil {
		switch g := int64(*gpu); {
		case g < 0:
			return bad("gpu must be 0 or more")
		case g > 0 && c.GPU == 0:
			return bad("node %s has no GPUs", n.Name)
		case g > c.GPU:
			return bad("node %s has %d GPUs", n.Name, c.GPU)
		}
		share.GPU = int64(*gpu)
	}
	switch {
	case cpu != nil:
		share.CPU = int64(math.Round(*cpu * 1000))
		if share.CPU <= 0 || math.IsNaN(*cpu) {
			return bad("cpu must be more than 0 cores")
		}
		if share.CPU > c.CPU {
			return bad("node %s has %s cores", n.Name, coresOf(c.CPU))
		}
	case share.GPU > 0:
		share.CPU = c.CPU * share.GPU / c.GPU / 1000 * 1000
	}
	if share.CPU <= 0 {
		return bad("say how many cores (cpu): %d GPUs take none of node %s's", share.GPU, n.Name)
	}
	g := int32(share.GPU)
	return &g, resource.NewMilliQuantity(share.CPU, resource.DecimalSI), nil
}

// roomFor: nil when nr fits beside n's other reservations. A change (old:
// the reservation as it was) is checked only for the time it adds — over
// all it has left when its share grows: one made before shares may
// overlap others and still be shortened, renamed or moved within itself.
func (s *Server) roomFor(ctx context.Context, n *corev1.Node, nr, old *devpodv1alpha1.NodeReservation) *serverErr {
	all, err := s.nodeReservations(ctx)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	cluster := s.resCluster(ctx)
	var others []*devpodv1alpha1.NodeReservation
	for i := range all {
		if o := &all[i]; o.Spec.Cluster == cluster && o.Spec.Node == n.Name && o.Name != nr.Name {
			others = append(others, o)
		}
	}
	// running jobs that may not be stopped hold their share there too
	others = append(others, s.jobHolds(ctx, cluster, n.Name, s.Reservations.now())...)
	c := capacityOf(n)
	ask, whole := reservedOf(nr, c), isWhole(nr)
	start, end := nr.Spec.Start.Time, nr.Spec.End.Time
	type span struct{ from, to time.Time }
	// what is past does not matter (checkRoom looks from now on): a
	// reservation of the current slot fits beside one that ended, or was
	// cancelled, earlier in it
	now := s.Reservations.now()
	spans := []span{{start, end}}
	if old != nil {
		from := later(start, now)
		os, oe := old.Spec.Start.Time, old.Spec.End.Time
		if !ask.fits(reservedOf(old, c)) || (whole && !isWhole(old)) {
			spans = []span{{from, end}}
		} else {
			spans = nil
			if from.Before(os) {
				spans = append(spans, span{from, earlier(os, end)})
			}
			if end.After(oe) {
				spans = append(spans, span{later(oe, from), end})
			}
		}
	}
	ru := s.Reservations
	horizon := ru.MaxAdvance
	if horizon <= 0 {
		horizon = 14 * 24 * time.Hour
	}
	for _, sp := range spans {
		if !sp.from.Before(sp.to) {
			continue
		}
		r := checkRoom(others, c, ask, whole, sp.from, sp.to, now, ru.Granularity, horizon)
		if r == nil {
			continue
		}
		detail := map[string]any{"free": r.Free.view()}
		msg := fmt.Sprintf("node %s has at most %s free to reserve in that time", n.Name, r.Free)
		if old == nil && !r.Earliest.IsZero() {
			detail["earliest"] = r.Earliest.UTC()
			msg += fmt.Sprintf("; what you ask is free from %s", r.Earliest.In(reservationTZ).Format("2006-01-02 15:04"))
		}
		return &serverErr{http.StatusConflict, "NO_ROOM", msg, detail}
	}
	return nil
}

// keptAfterCreate: nil when nr, just made, keeps its place beside the
// node's reservations as they now are; else the refusal it gives way with.
func (s *Server) keptAfterCreate(ctx context.Context, n *corev1.Node, nr *devpodv1alpha1.NodeReservation) *serverErr {
	all, err := s.nodeReservations(ctx)
	if err != nil {
		return nil // unread: kept (the check before it was made passed)
	}
	cluster := s.resCluster(ctx)
	var rs []*devpodv1alpha1.NodeReservation
	var self *devpodv1alpha1.NodeReservation
	for i := range all {
		if o := &all[i]; o.Spec.Cluster == cluster && o.Spec.Node == n.Name {
			rs = append(rs, o)
			if o.Name == nr.Name {
				self = o
			}
		}
	}
	if self == nil || admitted(rs, capacityOf(n), self, s.Reservations.now()) {
		return nil
	}
	if e := s.roomFor(ctx, n, nr, nil); e != nil {
		return e
	}
	return &serverErr{http.StatusConflict, "NO_ROOM", fmt.Sprintf("node %s was reserved by someone else at the same moment", n.Name), map[string]any{"free": amount{}.view()}}
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func viewOfReservation(nr *devpodv1alpha1.NodeReservation, me string, now time.Time) reservationView {
	v := reservationView{ID: nr.Name, Node: nr.Spec.Node, User: nr.Spec.User, Start: nr.Spec.Start.Time.UTC(), End: overAt(nr).UTC(), Mine: nr.Spec.User == me, Purpose: nr.Spec.Purpose, CanManage: nr.Spec.User == me}
	if !isWhole(nr) {
		gpu, cpu := int32(0), 0.0
		if nr.Spec.GPU != nil {
			gpu = *nr.Spec.GPU
		}
		if nr.Spec.CPU != nil {
			cpu = float64(nr.Spec.CPU.MilliValue()) / 1000
		}
		v.GPU, v.CPU = &gpu, &cpu
	}
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
	holds := []jobHoldView{} // the timeline's: what running jobs hold
	shown := s.clusterGate(r.Context(), sess)
	var gs *access.GrantSet
	var groups map[string]bool
	if !sess.Admin {
		var gerr error
		if gs, groups, gerr = s.grantsFor(r.Context(), sess.User); gerr != nil {
			gs = nil // unread: no grant gives anything
		}
	}
	for _, cc := range s.ks(r) {
		ctx := s.in(r.Context(), cc)
		cluster := s.resCluster(ctx)
		open := shown(cc.Name)
		// the machines whose reservations the caller may run (a grant)
		managed := map[string]bool{}
		manageable := func(node string) bool {
			if sess.Admin {
				return true
			}
			if gs == nil {
				return false
			}
			if ok, seen := managed[node]; seen {
				return ok
			}
			var n corev1.Node
			if err := cc.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
				n.Name = node
			}
			managed[node] = gs.Has(devpodv1alpha1.PermManageReservations, sess.User, groups, cluster, &access.Node{Name: n.Name, Labels: n.Labels})
			return managed[node]
		}
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
		// a machine's reservations that do not fit beside each other (made
		// before shares), worked out once per machine shown
		onNode := map[string][]*devpodv1alpha1.NodeReservation{}
		for i := range all {
			if all[i].Spec.Cluster == cluster {
				onNode[all[i].Spec.Node] = append(onNode[all[i].Spec.Node], &all[i])
			}
		}
		overOn := map[string]map[string]bool{}
		overbookedOn := func(node string) map[string]bool {
			if o, seen := overOn[node]; seen {
				return o
			}
			var n corev1.Node
			if err := cc.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
				overOn[node] = nil // unknown capacity: not said
				return nil
			}
			overOn[node] = overbooked(onNode[node], capacityOf(&n), now)
			return overOn[node]
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
			v.CanManage = v.Mine || manageable(nr.Spec.Node)
			if !v.CanManage && (!open || !usable(nr.Spec.Node)) {
				continue // others' reservations in a cluster, or of a machine, not shown to them
			}
			if v.Start.Before(to) && from.Before(v.End) {
				v.Cluster = s.clusterLabel(cc)
				if now.Before(overAt(nr)) {
					v.Overbooked = overbookedOn(nr.Spec.Node)[nr.Name]
				}
				items = append(items, v)
			}
		}
		if !mine && limit == 0 && after == nil && s.Queue != nil {
			for _, h := range s.jobHoldViews(ctx, cluster, s.clusterLabel(cc), sess.User, func(node string) bool { return open && usable(node) }, now) {
				if (q.Get("node") == "" || q.Get("node") == h.Node) && h.Start.Before(to) && from.Before(h.End) {
					holds = append(holds, h)
				}
			}
		}
	}
	if limit == 0 && after == nil {
		sort.SliceStable(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
		out := map[string]any{"items": items}
		if !mine && s.Queue != nil {
			out["jobs"] = holds
		}
		s.writeJSON(w, http.StatusOK, out)
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
		Node    string    `json:"node"`
		Start   time.Time `json:"start"`
		End     time.Time `json:"end"`
		User    string    `json:"user"`
		Purpose string    `json:"purpose"`
		GPU     *int      `json:"gpu"`
		CPU     *float64  `json:"cpu"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	purpose, err := validPurpose(req.Purpose)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	user := sess.User
	if req.User != "" && req.User != sess.User {
		if !s.mayManageReservation(r.Context(), sess, req.Node) {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "only an admin, or one granted 管理预约 on this machine, reserves for someone else", nil)
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
	gpu, cpu, apiErr := shareOf(n, req.GPU, req.CPU)
	if apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	cluster := s.resCluster(r.Context())
	d, _, err := s.decide(r.Context(), user, cluster, access.Node{Name: n.Name, Labels: n.Labels})
	switch {
	case err != nil:
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	case !d.Allow:
		s.writeErr(w, http.StatusForbidden, "ACL_DENIED", fmt.Sprintf("%s may not use node %s", user, n.Name), nil)
		return
	}
	nr := &devpodv1alpha1.NodeReservation{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s-%s", n.Name, user, randSuffix(6)), Annotations: map[string]string{devpodv1alpha1.ClearsAnnotation: "true"}},
		Spec:       devpodv1alpha1.NodeReservationSpec{Cluster: cluster, Node: n.Name, User: user, Start: metav1.NewTime(req.Start.UTC()), End: metav1.NewTime(req.End.UTC()), Purpose: purpose},
	}
	nr.Spec.GPU, nr.Spec.CPU = gpu, cpu
	syncShare(nr)
	if apiErr := s.roomFor(r.Context(), n, nr, nil); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	if err := s.createReservation(r.Context(), nr); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", rejected("reserve", err), nil)
		return
	}
	// another made at the same time (on another replica) may have taken
	// the room: of the two, the later gives way
	if apiErr := s.keptAfterCreate(r.Context(), n, nr); apiErr != nil {
		if err := s.deleteReservation(r.Context(), nr.Name); err != nil {
			slog.Warn("reservation_give_way", "reservation", nr.Name, "err", err)
		}
		s.writeServerErr(w, apiErr)
		return
	}
	s.reservationHistory(r.Context(), nr, "created")
	created := viewOfReservation(nr, sess.User, ru.now())
	created.CanManage = true // made by them: theirs, or one they may run
	s.writeJSON(w, http.StatusCreated, created)
}

// ownReservation loads reservation id of the request's cluster for a change
// by its holder or an admin; not one that is over.
func (s *Server) ownReservation(w http.ResponseWriter, r *http.Request, sess Session, now time.Time) (*devpodv1alpha1.NodeReservation, bool) {
	var nr devpodv1alpha1.NodeReservation
	err := s.Reader.Get(r.Context(), types.NamespacedName{Name: r.PathValue("id")}, &nr)
	restoreShare(&nr)
	if err != nil || nr.Spec.Cluster != s.resCluster(r.Context()) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such reservation", nil)
		return nil, false
	}
	if nr.Spec.User != sess.User && !s.mayManageReservation(r.Context(), sess, nr.Spec.Node) {
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
// end?, purpose?}: one that has started can only have its end moved (and
// its purpose changed).
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
		Start   *time.Time `json:"start"`
		End     *time.Time `json:"end"`
		Purpose *string    `json:"purpose"`
		GPU     *int       `json:"gpu"`
		CPU     *float64   `json:"cpu"`
		Whole   *bool      `json:"whole"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Start == nil && req.End == nil && req.Purpose == nil && req.GPU == nil && req.CPU == nil && req.Whole == nil) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "send start, end, purpose and / or the amount (gpu, cpu, or whole)", nil)
		return
	}
	whole := req.Whole != nil && *req.Whole
	if whole && (req.GPU != nil || req.CPU != nil) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "the whole machine, or gpu and cpu: not both", nil)
		return
	}
	purpose := ""
	if req.Purpose != nil {
		var err error
		if purpose, err = validPurpose(*req.Purpose); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
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
	moves := req.Start != nil || req.End != nil // a purpose alone: the times are left as they are
	resizes := whole || req.GPU != nil || req.CPU != nil
	var n corev1.Node
	var gpu *int32
	var cpu *resource.Quantity
	if moves || resizes {
		if err := s.kc(r.Context()).Client.Get(r.Context(), types.NamespacedName{Name: nr.Spec.Node}, &n); err != nil {
			s.writeErr(w, http.StatusConflict, "NODE_UNAVAILABLE", fmt.Sprintf("node %s cannot be read: %v", nr.Spec.Node, err), nil)
			return
		}
		gpu, cpu = nr.Spec.GPU, nr.Spec.CPU
		if resizes && !whole {
			// cores alone keep the cards it holds (all of them, for a whole one)
			cards := req.GPU
			if cards == nil && req.CPU != nil {
				held := int(reservedOf(nr, capacityOf(&n)).GPU)
				cards = &held
			}
			var apiErr *serverErr
			if gpu, cpu, apiErr = shareOf(&n, cards, req.CPU); apiErr != nil {
				s.writeServerErr(w, apiErr)
				return
			}
		} else if whole {
			gpu, cpu = nil, nil
		}
	}
	var err error
	switch {
	case !moves:
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
	want := nr.DeepCopy()
	if moves || resizes {
		want.Spec.Start, want.Spec.End = metav1.NewTime(start), metav1.NewTime(end)
		want.Spec.GPU, want.Spec.CPU = gpu, cpu
		if apiErr := s.roomFor(r.Context(), &n, want, nr); apiErr != nil {
			s.writeServerErr(w, apiErr)
			return
		}
	}
	changed, err := s.changeReservation(r.Context(), nr.Name, func(spec *devpodv1alpha1.NodeReservationSpec) bool {
		if moves || resizes {
			spec.Start, spec.End, spec.GPU, spec.CPU = want.Spec.Start, want.Spec.End, want.Spec.GPU, want.Spec.CPU
		}
		if req.Purpose != nil {
			spec.Purpose = purpose
		}
		return true
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	// a change made at the same time (on another replica) may have taken
	// the room: checked again, and if it no longer fits, put back
	if moves || resizes {
		if apiErr := s.roomFor(r.Context(), &n, changed, nr); apiErr != nil {
			if _, err := s.changeReservation(r.Context(), nr.Name, func(spec *devpodv1alpha1.NodeReservationSpec) bool {
				spec.Start, spec.End, spec.GPU, spec.CPU, spec.Purpose = nr.Spec.Start, nr.Spec.End, nr.Spec.GPU, nr.Spec.CPU, nr.Spec.Purpose
				return true
			}); err != nil {
				slog.Warn("reservation_put_back", "reservation", nr.Name, "err", err)
			}
			s.writeServerErr(w, apiErr)
			return
		}
	}
	out := viewOfReservation(changed, sess.User, now)
	out.CanManage = true // they just changed it
	s.writeJSON(w, http.StatusOK, out)
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
	if err := s.cancelReservation(r.Context(), nr, sess.User, now); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cancelReservation: one not begun is deleted; one under way is cancelled
// now (its end is then handled as any end is).
func (s *Server) cancelReservation(ctx context.Context, nr *devpodv1alpha1.NodeReservation, by string, now time.Time) error {
	if now.Before(nr.Spec.Start.Time) {
		err := s.deleteReservation(ctx, nr.Name)
		if err == nil {
			s.reservationHistory(ctx, nr, "deleted")
		}
		return err
	}
	_, err := s.changeReservation(ctx, nr.Name, func(spec *devpodv1alpha1.NodeReservationSpec) bool {
		spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: by, At: metav1.NewTime(now.UTC().Truncate(time.Second))}
		return true
	})
	return err
}

// handleSetReservable serves PUT /api/admin/nodes/{name}/reservable
// {reservable}: the node label that opens a node for reservation.
// Turning it off leaves reservations already made to run their course.
func (s *Server) handleSetReservable(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	// an admin, or one granted it on this machine (design 2026-11-20 §6.1)
	if !sess.Admin {
		var n corev1.Node
		if err := s.k(r).Client.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name")}, &n); err != nil {
			if apierrors.IsNotFound(err) {
				s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "node not found", nil)
				return
			}
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		gs, groups, err := s.grantsFor(r.Context(), sess.User)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		if !gs.Has(devpodv1alpha1.PermSetReservable, sess.User, groups, s.resCluster(r.Context()), &access.Node{Name: n.Name, Labels: n.Labels}) {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "you may not switch this machine reservable", nil)
			return
		}
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
	cluster, now := s.resCluster(r.Context()), ru.now()
	action, detail := "reservable-on", ""
	if !*req.Reservable {
		action = "reservable-off"
		// closed: every reservation on it that is not over ends, as its owner would end it
		for i := range all {
			nr := &all[i]
			if nr.Spec.Cluster != cluster || nr.Spec.Node != name || !now.Before(overAt(nr)) {
				continue
			}
			if err := s.cancelReservation(r.Context(), nr, sess.User, now); err != nil {
				s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
				return
			}
			detail += nr.Name + " cancelled; "
		}
		all = nil
	}
	s.recordMachine(r.Context(), cluster, eventFrom(r, sess), name, action, strings.TrimSuffix(detail, "; "))
	s.writeJSON(w, http.StatusOK, map[string]any{"reservable": *req.Reservable, "pending": pendingOn(all, cluster, name, now)})
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
	// ManageReservations: the caller may run others' reservations here
	// (an admin, or a 管理预约 grant) — listed even where the rules keep
	// them off it.
	ManageReservations bool `json:"manageReservations,omitempty"`
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
	var gs *access.GrantSet
	var groups map[string]bool
	if !sess.Admin {
		var gerr error
		if gs, groups, gerr = s.grantsFor(r.Context(), sess.User); gerr != nil {
			gs = nil
		}
	}
	for _, cc := range s.ks(r) {
		// a cluster the rules keep them off is not shown — unless a 管理预约
		// grant of theirs may hold there
		if !shown(cc.Name) && (gs == nil || !gs.PossibleIn(devpodv1alpha1.PermManageReservations, sess.User, groups, s.resCluster(s.in(r.Context(), cc)))) {
			continue
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
	var gs *access.GrantSet
	var groups map[string]bool
	if !sess.Admin {
		var gerr error
		if gs, groups, gerr = s.grantsFor(ctx, sess.User); gerr != nil {
			gs = nil
		}
	}
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
		c.ManageReservations = sess.Admin || (gs != nil && gs.Has(devpodv1alpha1.PermManageReservations, sess.User, groups, cluster, &access.Node{Name: v.Name, Labels: v.Labels}))
		switch c.Access {
		case "denied":
			if !sess.Admin && !c.ManageReservations {
				continue // a machine the rules keep them off is not shown to them (unless its reservations are theirs to run)
			}
			c.Selectable, c.Reason = false, "no access"
		case "reservation":
			c.Selectable, c.Reason = false, "reservation required"
		}
		items = append(items, c)
	}
	return items, nil
}
