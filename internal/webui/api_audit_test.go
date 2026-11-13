// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
)

type fakeAuditStore struct {
	mu        sync.Mutex
	sessF     store.SessionFilter
	cmdF      store.CommandFilter
	httpF     store.HTTPFilter
	sessions  map[string]*store.SessionDetail
	files     map[string]*store.FileEventRow
	presence  map[string][]string
	access    []store.AccessLogRow
	accessErr error
	alertsHit bool
}

func (f *fakeAuditStore) ListSessions(_ context.Context, sf store.SessionFilter) ([]store.SessionSummary, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessF = sf
	return []store.SessionSummary{}, "", nil
}

func (f *fakeAuditStore) GetSession(_ context.Context, id string) (*store.SessionDetail, error) {
	if d, ok := f.sessions[id]; ok {
		return d, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAuditStore) SearchCommands(_ context.Context, cf store.CommandFilter) ([]store.CommandRow, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmdF = cf
	return nil, "", nil
}

func (f *fakeAuditStore) SearchFiles(context.Context, store.FileFilter) ([]store.FileEventRow, string, error) {
	return nil, "", nil
}

func (f *fakeAuditStore) SearchHTTP(_ context.Context, hf store.HTTPFilter) ([]store.HTTPRequestRow, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.httpF = hf
	return []store.HTTPRequestRow{{RequestID: "r1", User: hf.User}}, "", nil
}

func (f *fakeAuditStore) GetFileEvent(_ context.Context, id string) (*store.FileEventRow, error) {
	if fe, ok := f.files[id]; ok {
		return fe, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAuditStore) ListAlerts(context.Context, store.TimeFilter) ([]store.AlertRow, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alertsHit = true
	return nil, "", nil
}

func (f *fakeAuditStore) ListAccessLog(context.Context, store.TimeFilter) ([]store.AccessLogRow, string, error) {
	return nil, "", nil
}

func (f *fakeAuditStore) InsertAccessLog(_ context.Context, r store.AccessLogRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accessErr != nil {
		return f.accessErr
	}
	f.access = append(f.access, r)
	return nil
}

func (f *fakeAuditStore) Presence(_ context.Context, uids []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, u := range uids {
		if v, ok := f.presence[u]; ok {
			out[u] = v
		}
	}
	return out, nil
}

func (f *fakeAuditStore) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.access {
		out = append(out, a.Action)
	}
	return out
}

type fakeObjects map[string][]byte

func (o fakeObjects) Put(context.Context, string, io.Reader, int64) error {
	return errors.New("read-only")
}
func (o fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := o[key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// segment builds one zstd raw segment for channel 1.
func segment(t *testing.T, recs ...rawlog.Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := rawlog.NewSegmentWriter(rawlog.SegmentConfig{
		Create: func(uint32) (io.WriteCloser, error) { return nopWC{&buf}, nil },
	})
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }

func auditRig(t *testing.T) (*webui.Server, *webui.SessionManager, *fakeAuditStore, fakeObjects) {
	t.Helper()
	s, sm := newServer(t)
	t0 := time.Unix(1794210460, 0).UTC()
	seg := segment(t,
		rawlog.Record{TS: t0, Channel: 1, Kind: rawlog.KindChannelOpen, Payload: rawlog.Encode(rawlog.ChannelOpen{Type: "session"})},
		rawlog.Record{TS: t0, Channel: 1, Kind: rawlog.KindRequest, Payload: rawlog.Encode(rawlog.Request{Type: "shell", OK: true})},
		rawlog.Record{TS: t0.Add(time.Second), Channel: 1, Kind: rawlog.KindDataOut, Payload: []byte("hello\r\n")},
		rawlog.Record{TS: t0.Add(2 * time.Second), Channel: 1, Kind: rawlog.KindDataIn, Payload: []byte("ls\r")},
	)
	objs := fakeObjects{"raw/s1/1": seg, "files/s1/1-1": []byte("TOP SECRET")}
	st := &fakeAuditStore{
		sessions: map[string]*store.SessionDetail{
			"S1": {Session: store.SessionRow{SessionID: "S1", User: "gl-bob", OpenedAt: t0}, Segments: []store.RawSegmentRow{{SessionID: "S1", Seq: 1, S3Key: "raw/s1/1"}}},
		},
		files: map[string]*store.FileEventRow{
			"F1": {EventID: "F1", SessionID: "S1", User: "gl-bob", Path: "/home/bob/secret plan.txt", S3Key: "files/s1/1-1"},
		},
		presence: map[string][]string{},
	}
	s.Audit = &webui.AuditDeps{Store: st, Objects: objs}
	return s, sm, st, objs
}

func route(t *testing.T, s *webui.Server, method, path string, c *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestAuditAPI(t *testing.T) {
	setupSuite(t)
	ctx := context.Background()
	_ = k8sClient.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-root"}, Spec: devpodv1alpha1.UserSpec{Admin: true}})
	_ = k8sClient.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-fallen"}})
	_ = k8sClient.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-carol"}})
	waitUser(t, "gl-root")
	waitUser(t, "gl-fallen")
	waitUser(t, "gl-carol")

	t.Run("NonAdminForcedToSelf", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		if rec := route(t, s, "GET", "/api/audit/sessions?user=gl-bob", forge(sm, "gl-alice", false), ""); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if st.sessF.User != "gl-alice" {
			t.Fatalf("store saw user %q", st.sessF.User)
		}
		route(t, s, "GET", "/api/audit/sessions?user=gl-bob", forge(sm, "gl-root", false), "")
		if st.sessF.User != "gl-bob" {
			t.Fatalf("admin filter: store saw %q", st.sessF.User)
		}
		route(t, s, "GET", "/api/audit/commands?q=rm&user=gl-bob", forge(sm, "gl-alice", false), "")
		if st.cmdF.User != "gl-alice" || st.cmdF.Q != "rm" {
			t.Fatalf("commands filter %+v", st.cmdF)
		}
	})

	// HTTP requests are searched as commands are: an admin by any user,
	// anyone else only their own; port and status are numbers.
	t.Run("HTTPSearch", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		rec := route(t, s, "GET", "/api/audit/http?user=gl-bob&port=8080&status=404&method=GET", forge(sm, "gl-root", false), "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"request_id":"r1"`) {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if f := st.httpF; f.User != "gl-bob" || f.Port != 8080 || f.Status != 404 || f.Method != "GET" || f.From.IsZero() || f.To.IsZero() {
			t.Fatalf("admin filter %+v", f)
		}
		route(t, s, "GET", "/api/audit/http?user=gl-bob", forge(sm, "gl-alice", false), "")
		if st.httpF.User != "gl-alice" {
			t.Fatalf("non-admin saw user %q", st.httpF.User)
		}
		if rec := route(t, s, "GET", "/api/audit/http?port=x", forge(sm, "gl-root", false), ""); rec.Code != 400 {
			t.Fatalf("bad port: %d", rec.Code)
		}
	})

	t.Run("DefaultRange7Days", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		route(t, s, "GET", "/api/audit/sessions", forge(sm, "gl-alice", false), "")
		if d := st.sessF.To.Sub(st.sessF.From); d != 7*24*time.Hour || time.Since(st.sessF.To) > time.Minute {
			t.Fatalf("range %v .. %v", st.sessF.From, st.sessF.To)
		}
	})

	t.Run("SessionDetailVisibility", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		if rec := route(t, s, "GET", "/api/audit/sessions/S1", forge(sm, "gl-alice", false), ""); rec.Code != 404 {
			t.Fatalf("other user: %d", rec.Code)
		}
		if rec := route(t, s, "GET", "/api/audit/sessions/S1", forge(sm, "gl-bob", false), ""); rec.Code != 200 {
			t.Fatalf("owner of the session: %d", rec.Code)
		}
		if len(st.actions()) != 0 {
			t.Fatalf("own view must not be logged: %v", st.actions())
		}
		if rec := route(t, s, "GET", "/api/audit/sessions/S1", forge(sm, "gl-root", false), ""); rec.Code != 200 {
			t.Fatalf("admin: %d", rec.Code)
		}
		if a := st.actions(); len(a) != 1 || a[0] != "view_session" {
			t.Fatalf("admin view must be logged: %v", a)
		}
	})

	t.Run("CastStreamsAndLogs", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		rec := route(t, s, "GET", "/api/audit/sessions/S1/channels/1/cast", forge(sm, "gl-bob", false), "")
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-asciicast" {
			t.Fatalf("%d %s", rec.Code, rec.Header())
		}
		first, rest, _ := strings.Cut(rec.Body.String(), "\n")
		if !strings.Contains(first, `"version":2`) || !strings.Contains(rest, `hello`) {
			t.Fatalf("cast %q", rec.Body)
		}
		if a := st.actions(); len(a) != 1 || a[0] != "replay" {
			t.Fatalf("access log %v", a)
		}
	})

	t.Run("RawAdminOnly", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		if rec := route(t, s, "GET", "/api/audit/sessions/S1/channels/1/raw?stream=out", forge(sm, "gl-bob", false), ""); rec.Code != 403 {
			t.Fatalf("non-admin: %d", rec.Code)
		}
		rec := route(t, s, "GET", "/api/audit/sessions/S1/channels/1/raw?stream=out", forge(sm, "gl-root", false), "")
		if rec.Code != 200 || rec.Body.String() != "hello\r\n" {
			t.Fatalf("%d %q", rec.Code, rec.Body)
		}
		if a := st.actions(); len(a) != 1 || a[0] != "download_raw" {
			t.Fatalf("access log %v", a)
		}
	})

	t.Run("FileContentDisposition", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		if rec := route(t, s, "GET", "/api/audit/files/F1/content", forge(sm, "gl-alice", false), ""); rec.Code != 404 {
			t.Fatalf("other user: %d", rec.Code)
		}
		rec := route(t, s, "GET", "/api/audit/files/F1/content", forge(sm, "gl-bob", false), "")
		if rec.Code != 200 || rec.Body.String() != "TOP SECRET" {
			t.Fatalf("%d %q", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="secret plan.txt"` {
			t.Fatalf("disposition %q", got)
		}
		if a := st.actions(); len(a) != 1 || a[0] != "download_file" {
			t.Fatalf("access log %v", a)
		}
	})

	// §9.1: access_log must not miss a content access, so content is
	// only served once its access record is written.
	t.Run("NoContentWithoutAccessLog", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		st.accessErr = errors.New("clickhouse down")
		for _, c := range []struct{ path, user string }{
			{"/api/audit/sessions/S1/channels/1/cast", "gl-bob"},
			{"/api/audit/sessions/S1/channels/1/raw?stream=out", "gl-root"},
			{"/api/audit/files/F1/content", "gl-bob"},
			{"/api/audit/sessions/S1", "gl-root"}, // admin viewing another user's session
		} {
			rec := route(t, s, "GET", c.path, forge(sm, c.user, false), "")
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "hello") || strings.Contains(rec.Body.String(), "TOP SECRET") {
				t.Errorf("%s: %d %q", c.path, rec.Code, rec.Body)
			}
		}
	})

	t.Run("PresenceOwnerSeesUsers", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		dp := &devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: "gl-alice-pres", Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{Owner: "gl-alice", Running: true,
				Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "busybox"}}}}},
		}
		if err := k8sClient.Create(ctx, dp); err != nil {
			t.Fatal(err)
		}
		got := waitDevPod(t, "gl-alice-pres")
		st.presence[string(got.UID)] = []string{"gl-alice", "gl-bob"}
		rec := route(t, s, "GET", "/api/audit/presence", forge(sm, "gl-alice", false), "")
		var body struct {
			Items map[string]struct {
				Count int      `json:"count"`
				Users []string `json:"users"`
			} `json:"items"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if p := body.Items["gl-alice-pres"]; p.Count != 2 || len(p.Users) != 2 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		rec = route(t, s, "GET", "/api/audit/presence", forge(sm, "gl-bob", false), "")
		if strings.Contains(rec.Body.String(), "gl-alice-pres") {
			t.Fatal("presence of other users' DevPods leaked")
		}
	})

	t.Run("PolicyPutUpdatesUserAndLogs", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		rec := route(t, s, "PUT", "/api/admin/users/gl-carol/audit-policy", forge(sm, "gl-root", false), `{"download":"Deny"}`)
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		waitFor(t, func() bool {
			var u devpodv1alpha1.User
			_ = k8sClient.Get(ctx, clientKey("gl-carol"), &u)
			return u.Spec.AuditPolicy != nil && u.Spec.AuditPolicy.Download != nil && *u.Spec.AuditPolicy.Download == devpodv1alpha1.PolicyDeny
		})
		if a := st.actions(); len(a) != 1 || a[0] != "edit_policy" {
			t.Fatalf("access log %v", a)
		}
		rec = route(t, s, "GET", "/api/admin/users/gl-carol/audit-policy", forge(sm, "gl-root", false), "")
		var body struct {
			Effective policy.Effective `json:"effective"`
			Warnings  []string         `json:"warnings"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if !body.Effective.DenyDownload || len(body.Warnings) != 1 || body.Warnings[0] != policy.WarnVSCodeBypass {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if rec := route(t, s, "PUT", "/api/admin/users/gl-carol/audit-policy", forge(sm, "gl-carol", false), `null`); rec.Code != 403 {
			t.Fatalf("non-admin edit: %d", rec.Code)
		}
	})

	t.Run("PolicyNoUserCR404", func(t *testing.T) {
		s, sm, _, _ := auditRig(t)
		if rec := route(t, s, "PUT", "/api/admin/users/gl-nobody/audit-policy", forge(sm, "gl-root", false), `{}`); rec.Code != 404 {
			t.Fatalf("%d", rec.Code)
		}
	})

	t.Run("AlertsRevokedAdminForbidden", func(t *testing.T) {
		s, sm, st, _ := auditRig(t)
		if rec := route(t, s, "GET", "/api/audit/alerts", forge(sm, "gl-fallen", true), ""); rec.Code != 403 || st.alertsHit {
			t.Fatalf("stale admin cookie: %d", rec.Code)
		}
		if rec := route(t, s, "GET", "/api/audit/alerts", forge(sm, "gl-root", false), ""); rec.Code != 200 {
			t.Fatalf("real admin: %d", rec.Code)
		}
	})

	t.Run("AuditDisabled404", func(t *testing.T) {
		s, sm := newServer(t)
		rec := route(t, s, "GET", "/api/audit/sessions", forge(sm, "gl-alice", false), "")
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "AUDIT_DISABLED") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
}
