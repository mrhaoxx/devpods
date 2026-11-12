// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

// Snapshots (snapshot spec v2 §2.3): save a running DevPod's container
// as an image in the user's own repository, watch it happen, cancel it.
// The webui only creates and deletes DevPodSnapshot objects and reads
// the snapshot Job's log; the controller does the work.
//
// Where the image goes (spec §2.5): by default into the platform's
// registry, at an address built from ImageTemplate and pushed with the
// platform's Secret. A user may name any other address, but then must
// bring the credentials for it — the platform Secret is only ever used
// for addresses of the template's shape under the DevPod owner's name.
type Snapshots struct {
	// ImageTemplate builds the default target: {user}, {name}, {tag}.
	// Empty = no platform registry: every request names its image and
	// brings credentials.
	ImageTemplate string
	// PushSecret names the dockerconfigjson Secret (devpods namespace)
	// used for template addresses.
	PushSecret string
	// MaxPerUser caps the succeeded snapshots a user keeps (0 = no cap).
	MaxPerUser int
	// Logs follows the snapshot Job's output; it fails while the Job's
	// Pod has not started (the caller retries).
	Logs func(ctx context.Context, namespace, snapshot string) (io.ReadCloser, error)

	sem chan struct{} // concurrent progress streams
}

const (
	snapshotStreams     = 32
	progressTailLines   = 20
	progressLineBytes   = 500
	progressHeartbeat   = 2 * time.Second
	progressPoll        = time.Second
	snapshotNameMaxLen  = 63
	snapshotTagMaxLen   = 128
	snapshotObjPrefixMx = 40 // "snapshot-" + prefix + "-" + 6 stays under 63
)

// NewSnapshots validates the configuration.
func NewSnapshots(imageTemplate, pushSecret string, maxPerUser int, logs func(context.Context, string, string) (io.ReadCloser, error)) (*Snapshots, error) {
	for _, ph := range []string{"{user}", "{name}", "{tag}"} {
		if imageTemplate != "" && strings.Count(imageTemplate, ph) != 1 {
			return nil, fmt.Errorf("snapshot image template %q must contain %s once", imageTemplate, ph)
		}
	}
	if maxPerUser < 0 {
		return nil, fmt.Errorf("snapshot max per user must not be negative")
	}
	return &Snapshots{ImageTemplate: imageTemplate, PushSecret: pushSecret, MaxPerUser: maxPerUser, Logs: logs, sem: make(chan struct{}, snapshotStreams)}, nil
}

var snapshotPart = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

var shanghai = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return l
	}
	return time.FixedZone("CST", 8*3600)
}()

func (sn *Snapshots) image(user, name, tag string) string {
	return strings.NewReplacer("{user}", user, "{name}", name, "{tag}", tag).Replace(sn.ImageTemplate)
}

// ownAddress reports whether image has the template's shape with user
// in the {user} place: only such addresses get the platform Secret.
func (sn *Snapshots) ownAddress(image, user string) bool {
	if sn.ImageTemplate == "" {
		return false
	}
	const part = `[a-z0-9][a-z0-9._-]*`
	pat := regexp.QuoteMeta(sn.ImageTemplate)
	pat = strings.Replace(pat, regexp.QuoteMeta("{user}"), regexp.QuoteMeta(user), 1)
	pat = strings.Replace(pat, regexp.QuoteMeta("{name}"), part, 1)
	pat = strings.Replace(pat, regexp.QuoteMeta("{tag}"), part, 1)
	ok, err := regexp.MatchString("^"+pat+"$", image)
	return err == nil && ok
}

// imageRef is the CRD's pattern for spec.image.
var imageRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$`)

// validCustomImage: a full reference with a tag, not a digest.
func validCustomImage(image string) bool {
	if len(image) > 255 || !imageRef.MatchString(image) || strings.Contains(image, "@") {
		return false
	}
	slash := strings.LastIndexByte(image, '/')
	last := image[slash+1:]
	colon := strings.LastIndexByte(last, ':')
	return slash > 0 && colon > 0 && colon < len(last)-1
}

// savedRegistry reports whether the user saved credentials for host.
func (s *Server) savedRegistry(r *http.Request, user, host string) bool {
	if host == "" {
		return false
	}
	var u devpodv1alpha1.User
	if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: user}, &u); err != nil {
		return false
	}
	return slices.ContainsFunc(u.Spec.Registries, func(reg devpodv1alpha1.UserRegistry) bool { return reg.Host == host })
}

// customRegistryAnnotation marks a snapshot pushed to an address the
// user chose with their own credentials.
const customRegistryAnnotation = "devpod.io/custom-registry"

// snapshotView is the API's shape of a DevPodSnapshot.
type snapshotView struct {
	ID        string `json:"id"`
	DevPod    string `json:"devpod"`
	Owner     string `json:"owner"`
	Image     string `json:"image"`
	Phase     string `json:"phase"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	Message   string `json:"message"`
	// CustomRegistry: pushed to an address the user named, with their
	// own credentials (which are never shown).
	CustomRegistry bool       `json:"customRegistry"`
	CreatedAt      time.Time  `json:"createdAt"`
	StartedAt      *time.Time `json:"startedAt"`
	CompletedAt    *time.Time `json:"completedAt"`
}

func snapPhase(sn *devpodv1alpha1.DevPodSnapshot) devpodv1alpha1.DevPodSnapshotPhase {
	if sn.Status.Phase == "" {
		return devpodv1alpha1.SnapshotPending
	}
	return sn.Status.Phase
}

func snapTerminal(sn *devpodv1alpha1.DevPodSnapshot) bool {
	return sn.Status.Phase == devpodv1alpha1.SnapshotSucceeded || sn.Status.Phase == devpodv1alpha1.SnapshotFailed
}

func viewSnapshot(sn *devpodv1alpha1.DevPodSnapshot) snapshotView {
	v := snapshotView{ID: sn.Name, DevPod: sn.Spec.DevPodName, Owner: sn.Labels[render.LabelOwner], Image: sn.Spec.Image, Phase: string(snapPhase(sn)),
		Digest: sn.Status.Digest, SizeBytes: sn.Status.SizeBytes, Message: sn.Status.Message, CreatedAt: sn.CreationTimestamp.Time,
		CustomRegistry: sn.Annotations[customRegistryAnnotation] == "true"}
	if t := sn.Status.StartedAt; t != nil {
		v.StartedAt = &t.Time
	}
	if t := sn.Status.CompletedAt; t != nil {
		v.CompletedAt = &t.Time
	}
	return v
}

func (s *Server) snapshots(w http.ResponseWriter) (*Snapshots, bool) {
	if s.Snapshots == nil {
		s.writeErr(w, http.StatusNotFound, "SNAPSHOTS_DISABLED", "saving a DevPod as an image is not enabled", nil)
		return nil, false
	}
	return s.Snapshots, true
}

// ownedSnapshots lists owner's snapshots ("" = everyone's).
func (s *Server) ownedSnapshots(ctx context.Context, owner string) ([]devpodv1alpha1.DevPodSnapshot, error) {
	var list devpodv1alpha1.DevPodSnapshotList
	opts := []client.ListOption{client.InNamespace(s.NS)}
	if owner != "" {
		opts = append(opts, client.MatchingLabels{render.LabelOwner: owner})
	}
	// read from the API server: a snapshot created a moment ago must
	// count for the duplicate and quota checks
	if err := s.kc(ctx).Reader.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// getSnapshot loads a snapshot the session may see: its owner's, or any
// for an admin; a token limited to DevPods only those DevPods'.
func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request, sess Session, id string) (*devpodv1alpha1.DevPodSnapshot, bool) {
	var sn devpodv1alpha1.DevPodSnapshot
	err := s.k(r).Reader.Get(r.Context(), types.NamespacedName{Name: id, Namespace: s.NS}, &sn)
	if apierrors.IsNotFound(err) || (err == nil && ((sn.Labels[render.LabelOwner] != sess.User && !sess.Admin) || !sess.AllowsDevPod(sn.Spec.DevPodName))) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "snapshot not found", nil)
		return nil, false
	}
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return nil, false
	}
	return &sn, true
}

// mayStartSave: dp may be saved to image now — it is running, no save
// to that address is in progress, and the owner has room in the quota.
// Saving to an address again overwrites that tag: the registry gets the
// new image, and the controller drops the older records of the address
// once this one succeeds. Only two saves to one address at the same time
// are refused. An overwrite takes no new place in the quota.
func (s *Server) mayStartSave(r *http.Request, cfg *Snapshots, dp *devpodv1alpha1.DevPod, image string) *serverErr {
	if dp.Status.Phase != devpodv1alpha1.DevPodRunning {
		return &serverErr{http.StatusConflict, "NOT_RUNNING", "only a running DevPod can be saved as an image", nil}
	}
	mine, err := s.ownedSnapshots(r.Context(), dp.Spec.Owner)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	kept := 0
	for i := range mine {
		sn := &mine[i]
		terminal := sn.Status.Phase == devpodv1alpha1.SnapshotSucceeded || sn.Status.Phase == devpodv1alpha1.SnapshotFailed
		if sn.Spec.Image == image {
			if !terminal {
				return &serverErr{http.StatusConflict, "IMAGE_BUSY", fmt.Sprintf("%s is being saved right now; wait for it or cancel it", image), map[string]string{"id": sn.Name}}
			}
			continue
		}
		if sn.Status.Phase == devpodv1alpha1.SnapshotSucceeded {
			kept++
		}
	}
	if cfg.MaxPerUser > 0 && kept >= cfg.MaxPerUser {
		return &serverErr{http.StatusConflict, "SNAPSHOT_QUOTA", fmt.Sprintf("you keep %d saved images, the most allowed; delete one first", kept), nil}
	}
	return nil
}

// newSnapshot is a save of dp to image, its credentials yet to be set.
func newSnapshot(ns string, dp *devpodv1alpha1.DevPod, image string, pause *bool) *devpodv1alpha1.DevPodSnapshot {
	prefix := dp.Name
	if len(prefix) > snapshotObjPrefixMx {
		prefix = strings.TrimRight(prefix[:snapshotObjPrefixMx], "-.")
	}
	return &devpodv1alpha1.DevPodSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefix + "-" + randString(6),
			Namespace: ns,
			Labels:    map[string]string{render.LabelOwner: dp.Spec.Owner, render.LabelDevPod: dp.Name},
		},
		Spec: devpodv1alpha1.DevPodSnapshotSpec{DevPodName: dp.Name, Image: image, Push: true, Pause: pause},
	}
}

// handleRetrySnapshot serves POST /api/snapshots/{id}/retry: a failed
// save is started again as it was — the same DevPod, address, pause and
// credentials (inline ones were turned into a Secret the failed record
// still holds). The failed record stays.
func (s *Server) handleRetrySnapshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	cfg, ok := s.snapshots(w)
	if !ok {
		return
	}
	old, ok := s.getSnapshot(w, r, sess, r.PathValue("id"))
	if !ok {
		return
	}
	if old.Status.Phase != devpodv1alpha1.SnapshotFailed {
		s.writeErr(w, http.StatusConflict, "NOT_FAILED", "only a failed save is tried again", nil)
		return
	}
	var dp devpodv1alpha1.DevPod
	if err := s.k(r).Reader.Get(r.Context(), types.NamespacedName{Name: old.Spec.DevPodName, Namespace: s.NS}, &dp); err != nil {
		s.writeErr(w, http.StatusConflict, "NOT_RUNNING", fmt.Sprintf("the DevPod %s is gone", old.Spec.DevPodName), nil)
		return
	}
	if apiErr := s.mayStartSave(r, cfg, &dp, old.Spec.Image); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	sn := newSnapshot(s.NS, &dp, old.Spec.Image, old.Spec.Pause)
	sn.Spec.Push = old.Spec.Push
	if old.Spec.PushSecretRef != nil {
		ref := *old.Spec.PushSecretRef
		sn.Spec.PushSecretRef = &ref
	}
	if v, ok := old.Annotations[customRegistryAnnotation]; ok {
		sn.Annotations = map[string]string{customRegistryAnnotation: v}
	}
	if err := s.k(r).Client.Create(r.Context(), sn); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "snapshot rejected", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, viewSnapshot(sn))
}

// handleCreateSnapshot serves POST /api/devpods/{name}/snapshots.
func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	cfg, ok := s.snapshots(w)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var req struct {
		Name     string                         `json:"name"`
		Tag      string                         `json:"tag"`
		Pause    *bool                          `json:"pause"`
		Image    string                         `json:"image"`
		PushAuth *devpodv1alpha1.PushAuthInline `json:"pushAuth"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// not err.Error(): it may quote the body, credentials included
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body", nil)
		return
	}
	bad := func(msg string) { s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", msg, nil) }
	owner := dp.Spec.Owner
	var image string
	var auth *devpodv1alpha1.PushAuthInline // non-nil: credentials typed for this address
	savedSecret := ""                       // non-empty: the owner's saved registry credentials
	custom := false                         // an address outside the owner's place in the platform registry
	if req.Image == "" {
		if cfg.ImageTemplate == "" {
			bad("this deployment has no default registry: give the image address and its credentials")
			return
		}
		if req.Tag == "" {
			req.Tag = time.Now().In(shanghai).Format("20060102-1504")
		}
		if !snapshotPart.MatchString(req.Name) || len(req.Name) > snapshotNameMaxLen {
			bad("name: lowercase letters, digits, '.', '_' and '-', starting with a letter or digit, at most 63 characters")
			return
		}
		if !snapshotPart.MatchString(req.Tag) || len(req.Tag) > snapshotTagMaxLen {
			bad("tag: lowercase letters, digits, '.', '_' and '-', starting with a letter or digit, at most 128 characters")
			return
		}
		image = cfg.image(owner, req.Name, req.Tag)
	} else {
		if !validCustomImage(req.Image) {
			bad("image: a full reference with a tag, like registry.example.com/team/name:tag")
			return
		}
		image = req.Image
		// the platform's Secret is only for the owner's own place in the
		// platform registry. Any other address is pushed to with (spec
		// §5.2): credentials in the request; else the one the DevPod's
		// owner saved for that registry; else none at all — a registry
		// that takes pushes without a login. Nobody's Secret is lent to
		// an address that is not theirs.
		if !cfg.ownAddress(image, owner) {
			custom = true
			switch {
			case req.PushAuth != nil && (req.PushAuth.Username != "" || req.PushAuth.Password != ""):
				if req.PushAuth.Username == "" || req.PushAuth.Password == "" {
					s.writeErr(w, http.StatusBadRequest, "PUSH_AUTH_REQUIRED", "give both a user name and a password for the registry, or neither (a registry that needs no login)", nil)
					return
				}
				auth = &devpodv1alpha1.PushAuthInline{Username: req.PushAuth.Username, Password: req.PushAuth.Password}
			case s.savedRegistry(r, owner, imageRegistryHost(image)):
				savedSecret = devpodv1alpha1.RegistryCredentialsSecretName(owner)
			}
		}
	}
	if apiErr := s.mayStartSave(r, cfg, dp, image); apiErr != nil {
		s.writeServerErr(w, apiErr)
		return
	}
	sn := newSnapshot(s.NS, dp, image, req.Pause)
	switch {
	case auth != nil:
		// the controller turns this into a Secret and clears it at once
		sn.Spec.PushAuth = auth
		sn.Annotations = map[string]string{customRegistryAnnotation: "true"}
	case savedSecret != "":
		sn.Spec.PushSecretRef = &devpodv1alpha1.LocalObjectRef{Name: savedSecret}
		sn.Annotations = map[string]string{customRegistryAnnotation: "true"}
	case custom:
		// no credentials: pushed as nobody. The platform's Secret is not
		// for an address outside the owner's place in its registry.
		sn.Annotations = map[string]string{customRegistryAnnotation: "true"}
	case cfg.PushSecret != "":
		sn.Spec.PushSecretRef = &devpodv1alpha1.LocalObjectRef{Name: cfg.PushSecret}
	}
	if err := s.k(r).Client.Create(r.Context(), sn); err != nil {
		detail := any(err.Error())
		if auth != nil {
			detail = nil // an apiserver message may echo the object
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "snapshot rejected", detail)
		return
	}
	s.writeJSON(w, http.StatusCreated, viewSnapshot(sn))
}

// handleListSnapshots serves GET /api/snapshots[?scope=all][&devpod=].
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	if _, ok := s.snapshots(w); !ok {
		return
	}
	owner := sess.User
	if r.URL.Query().Get("scope") == "all" {
		if !sess.Admin {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "admin only", nil)
			return
		}
		owner = ""
	}
	list, err := s.ownedSnapshots(r.Context(), owner)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	devpod := r.URL.Query().Get("devpod")
	items := make([]snapshotView, 0, len(list))
	for i := range list {
		if (devpod == "" || list[i].Spec.DevPodName == devpod) && sess.AllowsDevPod(list[i].Spec.DevPodName) {
			items = append(items, viewSnapshot(&list[i]))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleGetSnapshot serves GET /api/snapshots/{id}.
func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	if _, ok := s.snapshots(w); !ok {
		return
	}
	if sn, ok := s.getSnapshot(w, r, sess, r.PathValue("id")); ok {
		s.writeJSON(w, http.StatusOK, viewSnapshot(sn))
	}
}

// handleDeleteSnapshot serves DELETE /api/snapshots/{id}: a running
// snapshot is cancelled (its Job goes with the object), a finished one
// is forgotten — the image stays in the registry.
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	if _, ok := s.snapshots(w); !ok {
		return
	}
	sn, ok := s.getSnapshot(w, r, sess, r.PathValue("id"))
	if !ok {
		return
	}
	// background: the Job and its Pod are removed after the object
	if err := s.k(r).Client.Delete(r.Context(), sn, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---- progress ---- */

// SnapshotProgress is one event of the progress stream.
type SnapshotProgress struct {
	Phase          string `json:"phase"`
	Stage          string `json:"stage"` // pending | commit | push | done | error
	ElapsedSeconds int64  `json:"elapsedSeconds"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
	// PushedBytes / TotalBytes: upload progress over the layers the
	// registry does not have yet (the Job's "@push" lines).
	PushedBytes int64    `json:"pushedBytes,omitempty"`
	TotalBytes  int64    `json:"totalBytes,omitempty"`
	Digest      string   `json:"digest,omitempty"`
	Message     string   `json:"message,omitempty"`
	Lines       []string `json:"lines"`
}

// logFold folds the Job's output (spec §2.1) into what the stream shows:
// "@" lines move the stage, everything else is the log tail.
type logFold struct {
	stage, digest, errText string
	size, pushed, total    int64
	tail                   []string
}

func (f *logFold) add(line string) {
	line = strings.TrimRight(line, "\r\n")
	if rest, ok := strings.CutPrefix(line, "@"); ok {
		verb, arg, _ := strings.Cut(rest, " ")
		arg = strings.TrimSpace(arg)
		switch verb {
		case "stage":
			if arg == "commit" || arg == "push" {
				f.stage = arg
			}
		case "size":
			if n, err := strconv.ParseInt(arg, 10, 64); err == nil && n >= 0 {
				f.size = n
			}
		case "push": // "@push <pushed> <total>"
			a, b, _ := strings.Cut(arg, " ")
			pushed, err1 := strconv.ParseInt(a, 10, 64)
			total, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
			if err1 == nil && err2 == nil && pushed >= 0 && total >= pushed {
				f.pushed, f.total = pushed, total
			}
		case "digest":
			f.digest = arg
		case "done":
			f.stage = "done"
		case "error":
			f.stage, f.errText = "error", arg
		}
		return
	}
	// a tool redrawing one line with \r: keep what it shows last
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	if strings.TrimSpace(line) == "" {
		return
	}
	if len(line) > progressLineBytes {
		line = line[:progressLineBytes]
		for len(line) > 0 && line[len(line)-1]&0xC0 == 0x80 { // not through a character
			line = line[:len(line)-1]
		}
		if n := len(line); n > 0 && line[n-1] >= 0xC0 {
			line = line[:n-1]
		}
	}
	f.tail = append(f.tail, line)
	if len(f.tail) > progressTailLines {
		f.tail = append(f.tail[:0], f.tail[len(f.tail)-progressTailLines:]...)
	}
}

// progressOf combines the snapshot object with the folded log. The
// object's terminal phase wins over the log; the log supplies the stage
// and details while it runs.
func progressOf(sn *devpodv1alpha1.DevPodSnapshot, f *logFold, now time.Time) SnapshotProgress {
	p := SnapshotProgress{Phase: string(snapPhase(sn)), Stage: f.stage, SizeBytes: f.size, PushedBytes: f.pushed, TotalBytes: f.total, Digest: f.digest, Message: f.errText, Lines: slices.Clone(f.tail)}
	if p.Lines == nil {
		p.Lines = []string{}
	}
	if p.Stage == "" {
		p.Stage = "pending"
	}
	switch sn.Status.Phase {
	case devpodv1alpha1.SnapshotSucceeded:
		p.Stage, p.Message = "done", ""
	case devpodv1alpha1.SnapshotFailed:
		p.Stage = "error"
		if p.Message == "" {
			p.Message = sn.Status.Message
		}
	}
	if sn.Status.SizeBytes > 0 {
		p.SizeBytes = sn.Status.SizeBytes
	}
	if sn.Status.Digest != "" {
		p.Digest = sn.Status.Digest
	}
	start := sn.CreationTimestamp.Time
	if sn.Status.StartedAt != nil {
		start = sn.Status.StartedAt.Time
	}
	end := now
	if sn.Status.CompletedAt != nil {
		end = sn.Status.CompletedAt.Time
	}
	if d := end.Sub(start); d > 0 {
		p.ElapsedSeconds = int64(d / time.Second)
	}
	return p
}

// followLog feeds the Job's output into f until the stream ends or ctx
// is done; it retries while the Pod has not started.
func (sn *Snapshots) followLog(ctx context.Context, namespace, name string, mu *sync.Mutex, f *logFold) {
	if sn.Logs == nil {
		return
	}
	for ctx.Err() == nil {
		rc, err := sn.Logs(ctx, namespace, name)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(progressPoll):
				continue
			}
		}
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			mu.Lock()
			f.add(sc.Text())
			mu.Unlock()
		}
		_ = rc.Close()
		return // the container ended (or the stream broke: the object's phase carries the outcome)
	}
}

// handleSnapshotProgress serves GET /api/snapshots/{id}/progress (SSE).
func (s *Server) handleSnapshotProgress(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	cfg, ok := s.snapshots(w)
	if !ok {
		return
	}
	sn, ok := s.getSnapshot(w, r, sess, r.PathValue("id"))
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported", nil)
		return
	}
	select {
	case cfg.sem <- struct{}{}:
		defer func() { <-cfg.sem }()
	default:
		s.writeErr(w, http.StatusTooManyRequests, "TOO_MANY_STREAMS", "too many progress views right now; try again shortly", nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var mu sync.Mutex
	fold := &logFold{}
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		cfg.followLog(ctx, s.NS, sn.Name, &mu, fold)
	}()

	var last string
	var lastAt time.Time
	emit := func(p SnapshotProgress, force bool) bool {
		b, _ := json.Marshal(p)
		if !force && string(b) == last && time.Since(lastAt) < progressHeartbeat {
			return true
		}
		// elapsed alone changing is only worth a line every heartbeat
		if !force && time.Since(lastAt) < progressHeartbeat {
			q := p
			q.ElapsedSeconds = 0
			var prev SnapshotProgress
			_ = json.Unmarshal([]byte(last), &prev)
			prev.ElapsedSeconds = 0
			qb, _ := json.Marshal(q)
			pb, _ := json.Marshal(prev)
			if string(qb) == string(pb) {
				return true
			}
		}
		last, lastAt = string(b), time.Now()
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	snapshot := func() SnapshotProgress {
		mu.Lock()
		defer mu.Unlock()
		return progressOf(sn, fold, time.Now())
	}
	tick := time.NewTicker(progressPoll)
	defer tick.Stop()
	for {
		if snapTerminal(sn) {
			// let the log's last lines arrive, then say the final word
			select {
			case <-logDone:
			case <-time.After(progressPoll):
			case <-ctx.Done():
				return
			}
			emit(snapshot(), true)
			return
		}
		if !emit(snapshot(), false) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var cur devpodv1alpha1.DevPodSnapshot
		err := s.kc(ctx).Reader.Get(ctx, types.NamespacedName{Name: sn.Name, Namespace: s.NS}, &cur)
		switch {
		case apierrors.IsNotFound(err) || (err == nil && cur.UID != sn.UID) || (err == nil && cur.DeletionTimestamp != nil):
			p := snapshot()
			p.Stage, p.Message = "error", "已取消"
			emit(p, true)
			return
		case err == nil:
			sn = &cur
		case ctx.Err() != nil:
			return
		}
	}
}
