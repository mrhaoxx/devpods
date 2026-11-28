// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/httpauth"
)

// The record of what was done to DevPods, jobs and machines (the audit
// store's events): written after the change itself, and never holding it
// up — a record the store does not take now waits in memory and is tried
// again, the oldest going first when too many wait. What waits goes in
// batches; a batch failed is sent again as it was, with its token, so one
// that went in though it timed out is kept once. A record the store
// refuses while it takes others lets them go first, and is given up after
// eventRefusals.

// eventBuffer: how many records may wait for the store.
const eventBuffer = 10000

// eventBatch: the most records one write carries.
const eventBatch = 500

// eventRefusals: how many times a record the store refuses alone, while
// it takes others, is tried before it is given up.
const eventRefusals = 3

// pendingEvent is a record waiting for the store, numbered in order.
type pendingEvent struct {
	seq uint64
	row store.EventRow
}

// eventWriter keeps the records not written yet, in order.
type eventWriter struct {
	mu      sync.Mutex
	pending []pendingEvent
	seq     uint64 // the last record's number
	id      string // this console's writer, in its batches' tokens
	running bool   // a writer goroutine is at work
	dropped int    // records given up, all told
}

// record writes ev to the audit store, now or later; without a store it
// does nothing. It never blocks on the store.
func (s *Server) record(ctx context.Context, ev store.EventRow) {
	if s.Audit == nil || s.Audit.Store == nil {
		return
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	w := &s.events
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) >= eventBuffer {
		w.pending = w.pending[1:]
		w.dropped++
		if w.dropped == 1 || w.dropped%100 == 0 {
			slog.Warn("events_dropped", "dropped", w.dropped, "why", "the audit store has not taken them")
		}
	}
	if w.id == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		w.id = hex.EncodeToString(b)
	}
	w.seq++
	w.pending = append(w.pending, pendingEvent{w.seq, ev})
	if !w.running {
		w.running = true
		// the store as it is now: the writer reads no field of s
		go s.writeEvents(context.WithoutCancel(ctx), eventSink{s.Audit.Store, w.id})
	}
}

// writeEvents writes what waits, in batches in order, until nothing
// does; a batch the store does not take is sent again as it was after
// eventRetry — unless the store takes another record alone (takesProbe),
// when a batch failed twice goes one record at a time, and one record
// refused so eventRefusals times is given up. Twice: the second time the
// batch went with its token, and would have gone (kept once) had the first
// gone in though it failed — what goes one at a time is never in already.
func (s *Server) writeEvents(ctx context.Context, sink eventSink) {
	w := &s.events
	retry := s.eventRetry
	if retry <= 0 {
		retry = 10 * time.Second
	}
	var batch []pendingEvent // being written: sent again as it is until it goes
	var alone uint64         // records up to this one go one at a time
	fails, refused := 0, 0   // batch's failed writes; of those, while the store took another
	for {
		if batch == nil {
			w.mu.Lock()
			if len(w.pending) == 0 {
				w.running = false
				w.mu.Unlock()
				return
			}
			n := min(len(w.pending), eventBatch)
			if w.pending[0].seq <= alone {
				n = 1
			}
			batch, fails, refused = slices.Clone(w.pending[:n]), 0, 0
			w.mu.Unlock()
		}
		err := sink.insert(ctx, batch)
		if err == nil {
			w.taken(batch)
			batch = nil
			continue
		}
		fails++
		first := batch[0].row
		slog.Warn("event_write", "records", len(batch), "kind", first.Kind, "object", first.Object, "action", first.Action, "err", err)
		if (len(batch) == 1 || fails >= 2) && s.takesProbe(ctx, sink, batch) {
			// the store is there, and refuses this batch: one of its records
			switch refused++; {
			case len(batch) > 1:
				alone, batch = batch[len(batch)-1].seq, nil
				continue
			case refused >= eventRefusals:
				slog.Warn("event_given_up", "kind", first.Kind, "object", first.Object, "action", first.Action, "err", err)
				w.mu.Lock()
				w.dropped++
				w.mu.Unlock()
				w.taken(batch)
				batch = nil
				continue
			}
		}
		time.Sleep(retry)
	}
}

// eventSink: where a writer writes — the store, and the writer's id in
// its batches' tokens.
type eventSink struct {
	store AuditStore
	id    string
}

// insert writes batch, its token naming the writer and the batch's first
// and last records (the same batch, the same token).
func (k eventSink) insert(ctx context.Context, batch []pendingEvent) error {
	rows := make([]store.EventRow, len(batch))
	for i := range batch {
		rows[i] = batch[i].row
	}
	token := fmt.Sprintf("%s-%d-%d", k.id, batch[0].seq, batch[len(batch)-1].seq)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return k.store.InsertEvents(wctx, rows, token)
}

// takesProbe: the store takes a record written alone (which is then taken
// off) — batch's first of several (the order kept when the store was only
// away), else the first waiting after batch, else batch's last of several;
// false when it takes none, or there is none to try.
func (s *Server) takesProbe(ctx context.Context, sink eventSink, batch []pendingEvent) bool {
	w := &s.events
	var probes []pendingEvent
	if len(batch) > 1 {
		probes = append(probes, batch[0])
	}
	last := batch[len(batch)-1].seq
	w.mu.Lock()
	if i := slices.IndexFunc(w.pending, func(p pendingEvent) bool { return p.seq > last }); i >= 0 {
		probes = append(probes, w.pending[i])
	}
	w.mu.Unlock()
	if len(batch) > 1 {
		probes = append(probes, batch[len(batch)-1])
	}
	for _, p := range probes {
		if sink.insert(ctx, []pendingEvent{p}) == nil {
			w.taken([]pendingEvent{p})
			return true
		}
	}
	return false
}

// taken takes batch's records off what waits (any dropped meanwhile, the
// buffer full, are gone already).
func (w *eventWriter) taken(batch []pendingEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	done := map[uint64]bool{}
	for _, p := range batch {
		done[p.seq] = true
	}
	w.pending = slices.DeleteFunc(w.pending, func(p pendingEvent) bool { return done[p.seq] })
}

// eventFrom: a record of what a request does — who, how, from where.
func eventFrom(r *http.Request, sess Session) store.EventRow {
	via := sess.Via
	if via == "" {
		via = "web"
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return store.EventRow{Actor: sess.User, Via: via, Origin: sess.Origin, ClientIP: ip, RequestID: r.Header.Get("X-Request-Id")}
}

// systemEvent: a record of what the console did by itself, as actor
// (system:queue, system:reservation, …).
func systemEvent(actor string) store.EventRow {
	return store.EventRow{Actor: actor, Via: "system"}
}

// recordDevPod records action on dp in cluster, as ev says who and how.
func (s *Server) recordDevPod(ctx context.Context, cluster string, ev store.EventRow, dp *devpodv1alpha1.DevPod, action, detail string) {
	ev.Cluster, ev.Kind, ev.Object, ev.ObjectUID, ev.Owner = cluster, "devpod", dp.Name, string(dp.UID), dp.Spec.Owner
	if ev.Node == "" {
		ev.Node = pinnedNode(dp)
	}
	ev.Action, ev.Detail = action, detail
	s.record(ctx, ev)
}

// devPodEvent records action on dp by the request's caller.
func (s *Server) devPodEvent(r *http.Request, sess Session, dp *devpodv1alpha1.DevPod, action, detail string) {
	s.recordDevPod(r.Context(), s.resCluster(r.Context()), eventFrom(r, sess), dp, action, detail)
}

// devPodChange: one thing an edit did, as recorded.
type devPodChange struct{ action, detail string }

// devPodActions: what changing before into after did — a pin moved, the
// rest of the spec edited, a start or a stop — in that order.
func devPodActions(before, after *devpodv1alpha1.DevPod) []devPodChange {
	var out []devPodChange
	autoNode, _, wasAuto := autoPin(before)
	_, _, isAuto := autoPin(after)
	startedOn := "" // placed at its start by the console itself
	switch b, a := pinnedNode(before), pinnedNode(after); {
	case b == a && wasAuto && !isAuto && a != "":
		out = append(out, devPodChange{"pin", a}) // the queue's pin made its user's
	case b == a:
	case wasAuto && a == "":
		out = append(out, devPodChange{"auto-unpin", autoNode})
	case isAuto && b == "":
		startedOn = a
	case a == "":
		out = append(out, devPodChange{"unpin", b})
	case b == "":
		out = append(out, devPodChange{"pin", a})
	default:
		out = append(out, devPodChange{"pin", b + "→" + a})
	}
	if d := devPodChanges(before, after, false); d != "" {
		out = append(out, devPodChange{"edit", d})
	}
	_, wasQ := queuedAt(before)
	_, isQ := queuedAt(after)
	switch {
	case wasQ && isQ && queuePriority(before) != queuePriority(after):
		out = append(out, devPodChange{"queue-priority", fmt.Sprintf("%d→%d", queuePriority(before), queuePriority(after))})
	case !wasQ && isQ:
		out = append(out, devPodChange{"queue", fmt.Sprintf("priority %d", queuePriority(after))})
	case wasQ && !isQ && !after.Spec.Running:
		out = append(out, devPodChange{"dequeue", ""})
	case !before.Spec.Running && after.Spec.Running && startedOn != "":
		out = append(out, devPodChange{"start", "on " + startedOn + " (auto)"})
	case !before.Spec.Running && after.Spec.Running:
		out = append(out, devPodChange{"start", ""})
	case before.Spec.Running && !after.Spec.Running:
		out = append(out, devPodChange{"stop", ""})
	}
	return out
}

// devPodChanges says in a line what changed from before to after: image,
// resources, shell, collaborators, environment variables (by name only:
// their values may be secrets), then the paths of whatever else changed
// in the spec — never a value of those. all adds running and the pin
// (which a PATCH records as actions of their own).
func devPodChanges(before, after *devpodv1alpha1.DevPod, all bool) string {
	var parts []string
	add := func(name, a, b string) {
		if a != b {
			parts = append(parts, name+" "+orNone(a)+"→"+orNone(b))
		}
	}
	if all {
		add("running", strconv.FormatBool(before.Spec.Running), strconv.FormatBool(after.Spec.Running))
		add("pin", pinnedNode(before), pinnedNode(after))
	}
	add("shell", before.Spec.Shell, after.Spec.Shell)
	for _, c := range after.Spec.Collaborators {
		if !slices.Contains(before.Spec.Collaborators, c) {
			parts = append(parts, "collaborator +"+c)
		}
	}
	for _, c := range before.Spec.Collaborators {
		if !slices.Contains(after.Spec.Collaborators, c) {
			parts = append(parts, "collaborator -"+c)
		}
	}
	bc, ac := mainContainer(before), mainContainer(after)
	add("image", bc.Image, ac.Image)
	for _, res := range resourceNames(bc, ac) {
		add(string(res), quantityOf(bc, res), quantityOf(ac, res))
	}
	for _, n := range envChanged(bc, ac) {
		parts = append(parts, "env."+n)
	}
	return strings.Join(append(parts, otherSpecPaths(before, after)...), "; ")
}

func orNone(s string) string {
	if s == "" {
		return "∅"
	}
	return s
}

// mainContainer: dp's first container (a zero one when it has none).
func mainContainer(dp *devpodv1alpha1.DevPod) corev1.Container {
	if dp.Spec.Pod == nil || len(dp.Spec.Pod.Spec.Containers) == 0 {
		return corev1.Container{}
	}
	return dp.Spec.Pod.Spec.Containers[0]
}

// resourceNames: the resources a or b sets, sorted.
func resourceNames(a, b corev1.Container) []corev1.ResourceName {
	seen := map[corev1.ResourceName]bool{}
	for _, c := range []corev1.Container{a, b} {
		for _, l := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
			for n := range l {
				seen[n] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// quantityOf: c's limit of res, else its request ("" none).
func quantityOf(c corev1.Container, res corev1.ResourceName) string {
	if q, ok := c.Resources.Limits[res]; ok {
		return q.String()
	}
	if q, ok := c.Resources.Requests[res]; ok {
		return q.String()
	}
	return ""
}

// envChanged: the names of the variables added, removed or changed.
func envChanged(a, b corev1.Container) []string {
	was := map[string]corev1.EnvVar{}
	for _, e := range a.Env {
		was[e.Name] = e
	}
	now := map[string]corev1.EnvVar{}
	for _, e := range b.Env {
		now[e.Name] = e
	}
	var out []string
	for n, e := range now {
		if old, ok := was[n]; !ok || !equality.Semantic.DeepEqual(old, e) {
			out = append(out, n)
		}
	}
	for n := range was {
		if _, ok := now[n]; !ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// otherSpecPaths: the paths in the spec of what else changed (at most 20),
// leaving out what devPodChanges names itself.
func otherSpecPaths(before, after *devpodv1alpha1.DevPod) []string {
	strip := func(dp *devpodv1alpha1.DevPod) map[string]any {
		c := dp.Spec.DeepCopy()
		c.Running, c.Shell, c.Collaborators = false, "", nil
		if c.Pod != nil {
			delete(c.Pod.Spec.NodeSelector, hostnameLabel)
			if len(c.Pod.Spec.Containers) > 0 {
				m := &c.Pod.Spec.Containers[0]
				m.Image, m.Resources, m.Env = "", corev1.ResourceRequirements{}, nil
			}
		}
		raw, _ := json.Marshal(c)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	var paths []string
	diffPaths("spec", strip(before), strip(after), &paths)
	if len(paths) > 20 {
		paths = append(paths[:20], "…")
	}
	return paths
}

// diffPaths adds to out the paths under prefix where a and b differ —
// maps by key, anything else whole.
func diffPaths(prefix string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, prefix)
		}
		return
	}
	keys := map[string]bool{}
	for k := range am {
		keys[k] = true
	}
	for k := range bm {
		keys[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		diffPaths(prefix+"."+k, am[k], bm[k], out)
	}
}

// createSummary: a new DevPod in a line — its image, resources, pin, and
// whether it runs.
func createSummary(dp *devpodv1alpha1.DevPod) string {
	var parts []string
	c := mainContainer(dp)
	if c.Image != "" {
		parts = append(parts, "image "+c.Image)
	}
	for _, res := range resourceNames(c, c) {
		parts = append(parts, string(res)+" "+quantityOf(c, res))
	}
	if n := pinnedNode(dp); n != "" {
		parts = append(parts, "pin "+n)
	}
	if dp.Spec.Running {
		parts = append(parts, "running")
	}
	if _, ok := queuedAt(dp); ok {
		parts = append(parts, "queued")
	}
	return strings.Join(parts, "; ")
}

// httpPortChanges: what a change of the HTTP ports did — each list's old
// and new value, and a new link (never the link itself).
func httpPortChanges(before, after *devpodv1alpha1.DevPod) string {
	var parts []string
	for _, k := range []struct{ name, key string }{
		{"public", httpauth.PublicPortsAnnotation}, {"users", httpauth.UserPortsAnnotation}, {"https", httpauth.HTTPSPortsAnnotation},
	} {
		if a, b := before.Annotations[k.key], after.Annotations[k.key]; a != b {
			parts = append(parts, k.name+" "+orNone(a)+"→"+orNone(b))
		}
	}
	if before.Annotations[httpauth.IDAnnotation] != after.Annotations[httpauth.IDAnnotation] {
		parts = append(parts, "new link")
	}
	return strings.Join(parts, "; ")
}

// recordJob records action on the queue's job j, as ev says who and how.
func (s *Server) recordJob(ctx context.Context, ev store.EventRow, j *devpodv1alpha1.Job, action, detail string) {
	ev.Cluster, ev.Kind, ev.Object, ev.Owner = j.Spec.Cluster, "job", j.Name, j.Spec.Owner
	ev.Action, ev.Detail = action, detail
	s.record(ctx, ev)
}

// jobSummary: what a job asks, in a line.
func jobSummary(j *devpodv1alpha1.Job) string {
	parts := []string{
		fmt.Sprintf("%d×%d GPU", j.Spec.Nodes, j.Spec.GPUsPerNode),
		j.Spec.CPUsPerNode.String() + " cores",
		"at most " + j.Spec.TimeLimit.Duration.String(),
		fmt.Sprintf("priority %d", j.Spec.Priority),
	}
	if j.Spec.Preemptible {
		parts = append(parts, "preemptible")
	}
	if j.Spec.Title != "" {
		parts = append(parts, "title "+j.Spec.Title)
	}
	return strings.Join(parts, "; ")
}

// jobEnd: how a job ended, in a line.
func jobEnd(phase devpodv1alpha1.JobPhase, reason string, detail map[string]string) string {
	parts := []string{string(phase)}
	if reason != "" {
		parts = append(parts, reason)
	}
	for _, k := range slices.Sorted(maps.Keys(detail)) {
		parts = append(parts, k+" "+detail[k])
	}
	return strings.Join(parts, "; ")
}

// recordMachine records action on machine node, as ev says who and how.
func (s *Server) recordMachine(ctx context.Context, cluster string, ev store.EventRow, node, action, detail string) {
	ev.Cluster, ev.Kind, ev.Object, ev.Node = cluster, "machine", node, node
	ev.Action, ev.Detail = action, detail
	s.record(ctx, ev)
}
