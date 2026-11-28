// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/kattach"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// v5Echo is a fake API server's attach: it says what it was asked, hands
// on every frame it is sent, echoes stdin as stdout (and "!"+it as
// stderr), and ends when stdin does.
type v5Echo struct {
	asked  chan string
	frames chan []byte
}

func newV5Echo() *v5Echo {
	return &v5Echo{asked: make(chan string, 8), frames: make(chan []byte, 32)}
}

func (a *v5Echo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.asked <- r.URL.Path + "?" + r.URL.RawQuery
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{kattach.Protocol}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	for {
		_, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		a.frames <- msg
		switch {
		case len(msg) > 0 && msg[0] == kattach.Stdin:
			_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{kattach.Stdout}, msg[1:]...))
			_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{kattach.Stderr, '!'}, msg[1:]...))
		case len(msg) == 2 && msg[0] == 255:
			_ = c.Write(r.Context(), websocket.MessageBinary, []byte("\x03{\"status\":\"Success\"}"))
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

// attachThrough: the console's attach goes to api.
func attachThrough(s *webui.Server, api *httptest.Server) {
	s.PodAttach = func(ctx context.Context, _, ns, pod, container string, tty bool) (*kattach.Conn, error) {
		return kattach.Dial(ctx, &rest.Config{Host: api.URL}, ns, pod, container, tty)
	}
}

// runRankPod makes rank dp's Pod, running its container.
func runRankPod(t *testing.T, dp devpodv1alpha1.DevPod, container string) string {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: render.PodName(&dp), Namespace: "devpods"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container, Image: "x"}}}}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: container, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var got corev1.Pod
		return k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: "devpods"}, &got) == nil && got.Status.Phase == corev1.PodRunning
	})
	return pod.Name
}

// readFrame reads the next frame from the console's side of an attach.
func readFrame(t *testing.T, c *websocket.Conn) (byte, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, msg, err := c.Read(ctx)
	if err != nil || len(msg) == 0 {
		t.Fatalf("read: %v %q", err, msg)
	}
	return msg[0], string(msg[1:])
}

// A running rank of one's job is attached to over a WebSocket — rank 0 of
// a terminal job on its terminal: what is sent is its stdin, what it
// writes comes back marked by stream, a resize and the end of stdin pass
// on, and its end closes the socket; the attach is recorded. A rank not
// running is answered before any upgrade; another's job is refused.
func TestJobAttach(t *testing.T) {
	p := jobRig(t, "ja", "ja-alice")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	api := newV5Echo()
	apiSrv := httptest.NewServer(api)
	defer apiSrv.Close()
	attachThrough(p.s, apiSrv)
	id := submit(t, p, "ja-alice", 1, 1, map[string]any{"attach": "tty"})
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	rank := ranksBecome(t, id, 1)[0]
	// it waits for its submitter as long as its start may take (the gang
	// timeout) and a little: never past this start of it
	wait := ""
	for _, e := range rank.Spec.Pod.Spec.Containers[0].Env {
		if e.Name == "SUPERVISOR_ATTACH_WAIT" {
			wait = e.Value
		}
	}
	if wait != (p.s.Queue.GangTimeout + time.Minute).String() {
		t.Fatalf("the rank waits %q", wait)
	}
	web := httptest.NewServer(p.s.Routes())
	defer web.Close()
	ctx := context.Background()
	dial := func(user string) (*websocket.Conn, int) {
		c, resp, err := websocket.Dial(ctx, web.URL+"/api/jobs/"+id+"/attach?rank=0", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {p.as(user).String()}}})
		if err != nil {
			if resp == nil {
				t.Fatalf("dial: %v", err)
			}
			return nil, resp.StatusCode
		}
		return c, http.StatusSwitchingProtocols
	}
	if _, code := dial("ja-alice"); code != http.StatusConflict {
		t.Fatalf("not running yet: %d", code)
	}
	pod := runRankPod(t, rank, "dev")
	if _, code := dial("ja-bob"); code != http.StatusForbidden {
		t.Fatalf("another's job: %d", code)
	}
	c, code := dial("ja-alice")
	if c == nil {
		t.Fatalf("attach: %d", code)
	}
	defer c.CloseNow()
	if q := <-api.asked; q != "/api/v1/namespaces/devpods/pods/"+pod+"/attach?container=dev&stderr=false&stdin=true&stdout=true&tty=true" {
		t.Fatalf("asked %s", q)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":80,"rows":24}`)); err != nil {
		t.Fatal(err)
	}
	if fr := <-api.frames; fr[0] != kattach.Resize || !strings.Contains(string(fr[1:]), `"Width":80`) {
		t.Fatalf("resize %q", fr)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if fr := <-api.frames; string(fr) != "\x00hi" {
		t.Fatalf("stdin %q", fr)
	}
	if ch, s := readFrame(t, c); ch != 1 || s != "hi" {
		t.Fatalf("stdout %d %q", ch, s)
	}
	if ch, s := readFrame(t, c); ch != 2 || s != "!hi" {
		t.Fatalf("stderr %d %q", ch, s)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"eof"}`)); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, _, err := c.Read(rctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("the end: %v", err)
	}
	if rows := eventsOf(t, f, id, "submit", "start", "attach"); rows[2].Detail != "rank 0; a terminal" || rows[2].Actor != "ja-alice" {
		t.Fatalf("records %+v", rows)
	}
}

// dp attaches as the DevPod's owner, to their own jobs only; it asks first
// whether it can.
func TestDPAttach(t *testing.T) {
	p := jobRig(t, "jd", "jd-alice")
	p.s.IngestKey = []byte("ingest-key-for-tests-0123456789ab")
	api := newV5Echo()
	apiSrv := httptest.NewServer(api)
	defer apiSrv.Close()
	attachThrough(p.s, apiSrv)
	id := submit(t, p, "jd-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	runRankPod(t, ranksBecome(t, id, 1)[0], "dev")
	src := p.get("jd-alice-dev1")
	dpSrv := httptest.NewServer(http.HandlerFunc(p.s.HandleDPAttach))
	defer dpSrv.Close()
	hdr := http.Header{"Authorization": {"Bearer " + wire.DPToken(p.s.IngestKey, string(src.UID))}, "X-Devpod-Uid": {string(src.UID)}}
	waitFor(t, func() bool { // the DevPod is found by its UID through the cache
		req, _ := http.NewRequest(http.MethodGet, dpSrv.URL+"/dp/v1/attach?probe=1", nil)
		req.Header = hdr.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusNoContent
	})
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, dpSrv.URL+"/dp/v1/attach?job="+id+"&rank=0", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer c.CloseNow()
	if q := <-api.asked; !strings.Contains(q, "tty=false") || !strings.Contains(q, "stderr=true") {
		t.Fatalf("a job without a terminal asked %s", q)
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("x"))
	if ch, s := readFrame(t, c); ch != 1 || s != "x" {
		t.Fatalf("stdout %d %q", ch, s)
	}
	// another's job: as if there were none
	p.create("jd-bob", http.StatusCreated)
	other := submit(t, p, "jd-bob", 1, 1, nil)
	_, resp, err := websocket.Dial(ctx, dpSrv.URL+"/dp/v1/attach?job="+other+"&rank=0", &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another's job: %v %v", err, resp)
	}
}
