// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestFoldSnapshotLog(t *testing.T) {
	t0 := time.Unix(1794210460, 0)
	sn := &devpodv1alpha1.DevPodSnapshot{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(t0)}}
	sn.Status.Phase = devpodv1alpha1.SnapshotRunning
	started := metav1.NewTime(t0.Add(2 * time.Second))
	sn.Status.StartedAt = &started
	now := t0.Add(12 * time.Second)

	p := webui.FoldSnapshotLogForTest(sn, nil, now)
	if p.Stage != "pending" || p.Phase != "Running" || p.ElapsedSeconds != 10 || p.Lines == nil || len(p.Lines) != 0 {
		t.Fatalf("no log yet: %+v", p)
	}
	log := []string{
		"@stage commit",
		"committing 7f3a… (paused)",
		"@size 1234567890",
		"@stage push",
		"layer a: waiting\rlayer a: 10%\rlayer a: 55%",
		"",
		strings.Repeat("x", 600),
	}
	p = webui.FoldSnapshotLogForTest(sn, log, now)
	// "@push" carries the upload progress; a malformed one is ignored
	p = webui.FoldSnapshotLogForTest(sn, append(slices.Clone(log), "@push 300 1200", "@push 9 nope", "@push 50 10"), now)
	if p.PushedBytes != 300 || p.TotalBytes != 1200 || len(p.Lines) != 3 {
		t.Fatalf("push progress: %+v", p)
	}
	p = webui.FoldSnapshotLogForTest(sn, log, now)
	if p.Stage != "push" || p.SizeBytes != 1234567890 || p.TotalBytes != 0 || len(p.Lines) != 3 || p.Lines[0] != "committing 7f3a… (paused)" || p.Lines[1] != "layer a: 55%" || len(p.Lines[2]) != 500 {
		t.Fatalf("pushing: %+v", p)
	}
	// only the last lines are kept
	var many []string
	for i := 0; i < 50; i++ {
		many = append(many, "line "+string(rune('a'+i%26)))
	}
	if p = webui.FoldSnapshotLogForTest(sn, many, now); len(p.Lines) != 20 || p.Lines[19] != many[49] {
		t.Fatalf("tail: %d %v", len(p.Lines), p.Lines)
	}
	p = webui.FoldSnapshotLogForTest(sn, append(log, "@digest sha256:abc", "@done"), now)
	if p.Stage != "done" || p.Digest != "sha256:abc" {
		t.Fatalf("done by log: %+v", p)
	}
	p = webui.FoldSnapshotLogForTest(sn, append(log, "@error push denied"), now)
	if p.Stage != "error" || p.Message != "push denied" {
		t.Fatalf("error by log: %+v", p)
	}
	// the object's terminal phase wins, and its times stop the clock
	done := metav1.NewTime(t0.Add(8 * time.Second))
	sn.Status.Phase, sn.Status.CompletedAt, sn.Status.Digest, sn.Status.SizeBytes = devpodv1alpha1.SnapshotSucceeded, &done, "sha256:def", 99
	if p = webui.FoldSnapshotLogForTest(sn, log, now); p.Stage != "done" || p.Digest != "sha256:def" || p.SizeBytes != 99 || p.ElapsedSeconds != 6 {
		t.Fatalf("succeeded: %+v", p)
	}
	sn.Status.Phase, sn.Status.Message = devpodv1alpha1.SnapshotFailed, "snapshot Job failed: boom"
	if p = webui.FoldSnapshotLogForTest(sn, nil, now); p.Stage != "error" || p.Message != "snapshot Job failed: boom" {
		t.Fatalf("failed: %+v", p)
	}
}

func TestSnapshotAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.Admins = map[string]bool{"sn-root": true}
	ctx := context.Background()
	for _, name := range []string{"sn-alice", "sn-bob", "sn-root"} {
		u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8sClient.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, u) })
	}
	t.Cleanup(func() {
		cleanupDevPods(t)
		_ = k8sClient.DeleteAllOf(ctx, &devpodv1alpha1.DevPodSnapshot{}, client.InNamespace("devpods"))
	})
	alice, bob, root := forge(sm, "sn-alice", false), forge(sm, "sn-bob", false), forge(sm, "sn-root", true)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	do := func(method, path string, auth any, body string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		switch a := auth.(type) {
		case *http.Cookie:
			req.AddCookie(a)
		case string:
			req.Header.Set("Authorization", "Bearer "+a)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, b
	}
	// call retries until `want`: writes reach the cached client a moment later
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			resp, b := do(method, path, auth, body)
			if resp.StatusCode == want {
				var out map[string]any
				_ = json.Unmarshal(b, &out)
				return out
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s: %d (want %d) %s", method, path, resp.StatusCode, want, b)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	setPhase := func(name string, phase devpodv1alpha1.DevPodPhase) {
		t.Helper()
		var dp devpodv1alpha1.DevPod
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: name, Namespace: "devpods"}, &dp); err != nil {
			t.Fatal(err)
		}
		dp.Status.Phase = phase
		if err := k8sClient.Status().Update(ctx, &dp); err != nil {
			t.Fatal(err)
		}
	}
	getSnap := func(id string) devpodv1alpha1.DevPodSnapshot {
		t.Helper()
		var sn devpodv1alpha1.DevPodSnapshot
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: id, Namespace: "devpods"}, &sn); err != nil {
			t.Fatal(err)
		}
		return sn
	}
	finish := func(id string, phase devpodv1alpha1.DevPodSnapshotPhase) {
		t.Helper()
		sn := getSnap(id)
		now := metav1.Now()
		sn.Status.Phase, sn.Status.CompletedAt, sn.Status.Digest, sn.Status.SizeBytes = phase, &now, "sha256:feed", 4096
		if phase == devpodv1alpha1.SnapshotFailed {
			sn.Status.Digest, sn.Status.Message = "", "snapshot Job failed: boom"
		}
		if err := k8sClient.Status().Update(ctx, &sn); err != nil {
			t.Fatal(err)
		}
	}

	for _, n := range []string{"dev1", "dev2"} {
		call("POST", "/api/devpods", alice, strings.Replace(createBody, `"dev1"`, `"`+n+`"`, 1), http.StatusCreated)
	}
	waitDevPod(t, "sn-alice-dev1")
	waitDevPod(t, "sn-alice-dev2")

	call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"env"}`, http.StatusNotFound) // feature off
	logs := make(chan string, 16)
	cfg, err := webui.NewSnapshots("reg.test/users/{user}/{name}:{tag}", "platform-push", 2, func(ctx context.Context, _, _ string) (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			for {
				select {
				case l, ok := <-logs:
					if !ok {
						_ = pw.Close()
						return
					}
					_, _ = io.WriteString(pw, l+"\n")
				case <-ctx.Done():
					_ = pw.CloseWithError(ctx.Err())
					return
				}
			}
		}()
		return pr, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Snapshots = cfg
	if _, err := webui.NewSnapshots("reg.test/{name}:{tag}", "", 1, nil); err == nil {
		t.Fatal("a template without {user} was accepted")
	}

	t.Run("default registry: the server builds the address and attaches the platform secret", func(t *testing.T) {
		call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"env","tag":"v1"}`, http.StatusConflict) // not running
		setPhase("sn-alice-dev1", devpodv1alpha1.DevPodRunning)
		setPhase("sn-alice-dev2", devpodv1alpha1.DevPodRunning)
		v := call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"env","tag":"v1","pause":false}`, http.StatusCreated)
		if v["image"] != "reg.test/users/sn-alice/env:v1" || v["devpod"] != "sn-alice-dev1" || v["owner"] != "sn-alice" || v["phase"] != "Pending" || v["customRegistry"] != false {
			t.Fatalf("view %v", v)
		}
		sn := getSnap(v["id"].(string))
		if sn.Spec.DevPodName != "sn-alice-dev1" || !sn.Spec.Push || sn.Spec.PushAuth != nil || sn.Spec.PushSecretRef == nil || sn.Spec.PushSecretRef.Name != "platform-push" ||
			sn.Spec.Pause == nil || *sn.Spec.Pause || sn.Labels["devpod.io/owner"] != "sn-alice" || sn.Labels["devpod.io/devpod"] != "sn-alice-dev1" {
			t.Fatalf("object %+v labels %v", sn.Spec, sn.Labels)
		}
		// the default tag is a timestamp
		v2 := call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"env"}`, http.StatusCreated)
		if img := v2["image"].(string); !strings.HasPrefix(img, "reg.test/users/sn-alice/env:20") || len(img) != len("reg.test/users/sn-alice/env:20261111-1530") {
			t.Fatalf("default tag: %s", img)
		}
		// the same address while it is still being saved: refused…
		if busy := call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"env","tag":"v1"}`, http.StatusConflict); busy["code"] != "IMAGE_BUSY" {
			t.Fatalf("same address in progress: %v", busy)
		}
		// …once it has finished, saving there again overwrites the tag
		var first devpodv1alpha1.DevPodSnapshot
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: v["id"].(string), Namespace: "devpods"}, &first); err != nil {
			t.Fatal(err)
		}
		first.Status.Phase = devpodv1alpha1.SnapshotSucceeded
		if err := k8sClient.Status().Update(ctx, &first); err != nil {
			t.Fatal(err)
		}
		again := call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"env","tag":"v1"}`, http.StatusCreated)
		if again["image"] != v["image"] || again["id"] == v["id"] {
			t.Fatalf("overwrite: %v", again)
		}
		// leave things as the later subtests expect: one pending snapshot of this address
		call("DELETE", "/api/snapshots/"+again["id"].(string), alice, "", http.StatusNoContent)
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: v["id"].(string), Namespace: "devpods"}, &first); err != nil {
			t.Fatal(err)
		}
		first.Status.Phase = ""
		if err := k8sClient.Status().Update(ctx, &first); err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{`{"name":"Env"}`, `{"name":"../x"}`, `{"name":"env","tag":"a b"}`, `{"name":""}`, `{"name":"env","unknown":1}`} {
			call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, body, http.StatusBadRequest)
		}
		// not the owner: neither a stranger nor (without being owner) anyone else
		call("POST", "/api/devpods/sn-alice-dev1/snapshots", bob, `{"name":"env","tag":"x"}`, http.StatusNotFound)
		// an admin saves someone's DevPod into the owner's repository
		v3 := call("POST", "/api/devpods/sn-alice-dev2/snapshots", root, `{"name":"byroot","tag":"v1"}`, http.StatusCreated)
		if v3["image"] != "reg.test/users/sn-alice/byroot:v1" || v3["owner"] != "sn-alice" {
			t.Fatalf("admin snapshot %v", v3)
		}
	})

	t.Run("a custom address needs its own credentials and never gets the platform secret", func(t *testing.T) {
		post := func(body string, want int) map[string]any {
			t.Helper()
			return call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, body, want)
		}
		if out := post(`{"image":"other.test/team/env:v1"}`, http.StatusBadRequest); out["code"] != "PUSH_AUTH_REQUIRED" {
			t.Fatalf("no credentials: %v", out)
		}
		// another user's place in the platform registry is "other" too
		if out := post(`{"image":"reg.test/users/sn-bob/env:v1"}`, http.StatusBadRequest); out["code"] != "PUSH_AUTH_REQUIRED" {
			t.Fatalf("someone else's path: %v", out)
		}
		if out := post(`{"image":"other.test/team/env:v1","pushAuth":{"username":"u","password":""}}`, http.StatusBadRequest); out["code"] != "PUSH_AUTH_REQUIRED" {
			t.Fatalf("empty password: %v", out)
		}
		for _, img := range []string{"other.test/team/env", "other.test/team/env@sha256:abcd", "noslash:v1", "other.test/team/env:"} {
			post(`{"image":"`+img+`","pushAuth":{"username":"u","password":"p"}}`, http.StatusBadRequest)
		}
		resp, raw := do("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"image":"other.test/team/env:v1","pushAuth":{"username":"robot","password":"s3cr3t-pw"}}`)
		if resp.StatusCode != http.StatusCreated || strings.Contains(string(raw), "s3cr3t-pw") || strings.Contains(string(raw), "robot") {
			t.Fatalf("custom create: %d %s", resp.StatusCode, raw)
		}
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		sn := getSnap(v["id"].(string))
		if v["image"] != "other.test/team/env:v1" || v["customRegistry"] != true || sn.Spec.PushSecretRef != nil ||
			sn.Spec.PushAuth == nil || sn.Spec.PushAuth.Username != "robot" || sn.Spec.PushAuth.Password != "s3cr3t-pw" {
			t.Fatalf("custom object: view %v spec %+v", v, sn.Spec)
		}
		// naming one's own template address is the default registry again
		own := post(`{"image":"reg.test/users/sn-alice/typed:v9","pushAuth":{"username":"ignored","password":"ignored"}}`, http.StatusCreated)
		sn = getSnap(own["id"].(string))
		if own["customRegistry"] != false || sn.Spec.PushAuth != nil || sn.Spec.PushSecretRef == nil || sn.Spec.PushSecretRef.Name != "platform-push" {
			t.Fatalf("own address: view %v spec %+v", own, sn.Spec)
		}
		// a malformed body is not echoed back (it may hold a password)
		if resp, raw := do("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"image":"x","pushAuth":{"password":"leak-me"`); resp.StatusCode != http.StatusBadRequest || strings.Contains(string(raw), "leak-me") {
			t.Fatalf("malformed: %d %s", resp.StatusCode, raw)
		}
	})

	var ids []string
	t.Run("lists by owner; admins see all; tokens limited to DevPods see only those", func(t *testing.T) {
		list := call("GET", "/api/snapshots", alice, "", http.StatusOK)["items"].([]any)
		if len(list) != 5 {
			t.Fatalf("alice sees %d", len(list))
		}
		for _, it := range list {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		if l := call("GET", "/api/snapshots?devpod=sn-alice-dev2", alice, "", http.StatusOK)["items"].([]any); len(l) != 1 {
			t.Fatalf("by devpod: %d", len(l))
		}
		if l := call("GET", "/api/snapshots", bob, "", http.StatusOK)["items"].([]any); len(l) != 0 {
			t.Fatalf("bob sees %d", len(l))
		}
		call("GET", "/api/snapshots?scope=all", alice, "", http.StatusForbidden)
		if l := call("GET", "/api/snapshots?scope=all", root, "", http.StatusOK)["items"].([]any); len(l) != 5 {
			t.Fatalf("admin sees %d", len(l))
		}
		call("GET", "/api/snapshots/"+ids[0], bob, "", http.StatusNotFound)
		call("GET", "/api/snapshots/"+ids[0], root, "", http.StatusOK)

		tok := call("POST", "/api/me/tokens", alice, `{"name":"one","scopes":["snapshots:read","snapshots:write"],"devpods":["sn-alice-dev2"]}`, http.StatusCreated)["token"].(string)
		l := call("GET", "/api/snapshots", tok, "", http.StatusOK)["items"].([]any)
		if len(l) != 1 || l[0].(map[string]any)["devpod"] != "sn-alice-dev2" {
			t.Fatalf("limited token list: %v", l)
		}
		on2 := l[0].(map[string]any)["id"].(string)
		call("GET", "/api/snapshots/"+on2, tok, "", http.StatusOK)
		for _, id := range ids {
			if id != on2 {
				call("GET", "/api/snapshots/"+id, tok, "", http.StatusNotFound)
				call("DELETE", "/api/snapshots/"+id, tok, "", http.StatusNotFound)
				break
			}
		}
		call("POST", "/api/devpods/sn-alice-dev1/snapshots", tok, `{"name":"x","tag":"y"}`, http.StatusForbidden)
		ro := call("POST", "/api/me/tokens", alice, `{"name":"ro","scopes":["snapshots:read"]}`, http.StatusCreated)["token"].(string)
		call("GET", "/api/snapshots", ro, "", http.StatusOK)
		call("DELETE", "/api/snapshots/"+on2, ro, "", http.StatusForbidden)
		if me := call("GET", "/api/me", alice, "", http.StatusOK)["features"].(map[string]any); me["snapshots"] != true || me["snapshotDefaultRegistry"] != true {
			t.Fatalf("features %v", me)
		}
	})

	t.Run("the quota counts succeeded snapshots of both kinds", func(t *testing.T) {
		finish(ids[0], devpodv1alpha1.SnapshotSucceeded)
		finish(ids[1], devpodv1alpha1.SnapshotSucceeded)
		deadline := time.Now().Add(10 * time.Second)
		for {
			resp, raw := do("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"more","tag":"v1"}`)
			if resp.StatusCode == http.StatusConflict && strings.Contains(string(raw), "SNAPSHOT_QUOTA") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("quota: %d %s", resp.StatusCode, raw)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if out := call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"image":"other.test/team/more:v1","pushAuth":{"username":"u","password":"p"}}`, http.StatusConflict); out["code"] != "SNAPSHOT_QUOTA" {
			t.Fatalf("custom address past the quota: %v", out)
		}
	})

	t.Run("progress follows the log, ends with the object, and a cancel is reported", func(t *testing.T) {
		open := func(id string) (*bufio.Scanner, func()) {
			req, _ := http.NewRequest("GET", srv.URL+"/api/snapshots/"+id+"/progress", nil)
			req.AddCookie(alice)
			resp, err := srv.Client().Do(req)
			if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("open progress: %v %v", err, resp)
			}
			return bufio.NewScanner(resp.Body), func() { _ = resp.Body.Close() }
		}
		next := func(sc *bufio.Scanner, until func(p map[string]any) bool) map[string]any {
			t.Helper()
			done := make(chan map[string]any, 1)
			go func() {
				for sc.Scan() {
					if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
						var p map[string]any
						_ = json.Unmarshal([]byte(d), &p)
						if until(p) {
							done <- p
							return
						}
					}
				}
				done <- nil
			}()
			select {
			case p := <-done:
				if p == nil {
					t.Fatal("the stream ended first")
				}
				return p
			case <-time.After(15 * time.Second):
				t.Fatal("timed out waiting for a progress event")
				return nil
			}
		}
		running := ids[2]
		sc, closeBody := open(running)
		defer closeBody()
		next(sc, func(p map[string]any) bool { return p["stage"] == "pending" })
		logs <- "@stage commit"
		logs <- "committing…"
		next(sc, func(p map[string]any) bool { return p["stage"] == "commit" })
		logs <- "@size 2048"
		logs <- "@stage push"
		logs <- "pushing layer 1/3"
		p := next(sc, func(p map[string]any) bool { return p["stage"] == "push" && len(p["lines"].([]any)) == 2 })
		if p["sizeBytes"] != 2048.0 || p["lines"].([]any)[1] != "pushing layer 1/3" {
			t.Fatalf("push event %v", p)
		}
		finish(running, devpodv1alpha1.SnapshotSucceeded)
		p = next(sc, func(p map[string]any) bool { return p["stage"] == "done" })
		if p["phase"] != "Succeeded" || p["digest"] != "sha256:feed" {
			t.Fatalf("final event %v", p)
		}
		if sc.Scan() && strings.HasPrefix(sc.Text(), "data: ") {
			t.Fatalf("the stream kept going: %s", sc.Text())
		}
		// a finished snapshot answers with one final event
		sc2, close2 := open(running)
		next(sc2, func(p map[string]any) bool { return p["stage"] == "done" })
		close2()

		// cancel = delete: the object goes, the stream says so
		cancelled := ids[3]
		sc3, close3 := open(cancelled)
		defer close3()
		next(sc3, func(p map[string]any) bool { return p["stage"] == "pending" })
		call("DELETE", "/api/snapshots/"+cancelled, alice, "", http.StatusNoContent)
		p = next(sc3, func(p map[string]any) bool { return p["stage"] == "error" })
		if p["message"] != "已取消" {
			t.Fatalf("cancel event %v", p)
		}
		var gone devpodv1alpha1.DevPodSnapshot
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: cancelled, Namespace: "devpods"}, &gone); !apierrors.IsNotFound(err) && gone.DeletionTimestamp == nil {
			t.Fatalf("still there: %v", err)
		}
		call("DELETE", "/api/snapshots/"+cancelled, alice, "", http.StatusNotFound)
		call("GET", "/api/snapshots/"+cancelled+"/progress", alice, "", http.StatusNotFound)
	})

	t.Run("without a default registry every snapshot names its address and credentials", func(t *testing.T) {
		bare, err := webui.NewSnapshots("", "platform-push", 0, func(context.Context, string, string) (io.ReadCloser, error) { return nil, errors.New("no pod") })
		if err != nil {
			t.Fatal(err)
		}
		s.Snapshots = bare
		defer func() { s.Snapshots = cfg }()
		call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"name":"env","tag":"v2"}`, http.StatusBadRequest)
		if out := call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"image":"reg.test/users/sn-alice/env:v2"}`, http.StatusBadRequest); out["code"] != "PUSH_AUTH_REQUIRED" {
			t.Fatalf("no template: %v", out)
		}
		v := call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, `{"image":"reg.test/users/sn-alice/env:v2","pushAuth":{"username":"u","password":"p"}}`, http.StatusCreated)
		if sn := getSnap(v["id"].(string)); sn.Spec.PushSecretRef != nil || sn.Spec.PushAuth == nil {
			t.Fatalf("no-template object %+v", sn.Spec)
		}
		if me := call("GET", "/api/me", alice, "", http.StatusOK)["features"].(map[string]any); me["snapshots"] != true || me["snapshotDefaultRegistry"] != false {
			t.Fatalf("features %v", me)
		}
	})

	t.Run("a registry the owner saved credentials for needs none in the request", func(t *testing.T) {
		s.Snapshots.MaxPerUser = 0 // the quota has its own test above
		post := func(body string, want int) map[string]any {
			t.Helper()
			return call("POST", "/api/devpods/sn-alice-dev1/snapshots", alice, body, want)
		}
		// someone else's saved registry does not help
		call("PUT", "/api/me/registries/saved.test", bob, `{"username":"bob","password":"bobpw"}`, http.StatusOK)
		if out := post(`{"image":"saved.test/team/env:s1"}`, http.StatusBadRequest); out["code"] != "PUSH_AUTH_REQUIRED" {
			t.Fatalf("bob's registry used for alice: %v", out)
		}
		call("PUT", "/api/me/registries/saved.test", alice, `{"username":"alice","password":"alicepw"}`, http.StatusOK)
		v := post(`{"image":"saved.test/team/env:s1"}`, http.StatusCreated)
		sn := getSnap(v["id"].(string))
		if v["customRegistry"] != true || sn.Spec.PushAuth != nil || sn.Spec.PushSecretRef == nil || sn.Spec.PushSecretRef.Name != "devpod-registry-sn-alice" {
			t.Fatalf("saved registry: view %v spec %+v", v, sn.Spec)
		}
		// credentials typed in the request win over the saved ones
		v = post(`{"image":"saved.test/team/env:s2","pushAuth":{"username":"typed","password":"typedpw"}}`, http.StatusCreated)
		sn = getSnap(v["id"].(string))
		if sn.Spec.PushSecretRef != nil || sn.Spec.PushAuth == nil || sn.Spec.PushAuth.Username != "typed" {
			t.Fatalf("typed credentials: %+v", sn.Spec)
		}
		// an admin saving alice's DevPod uses alice's registries, not their own
		call("PUT", "/api/me/registries/rootonly.test", root, `{"username":"root","password":"rootpw"}`, http.StatusOK)
		if out := call("POST", "/api/devpods/sn-alice-dev1/snapshots", root, `{"image":"rootonly.test/x/env:s3"}`, http.StatusBadRequest); out["code"] != "PUSH_AUTH_REQUIRED" {
			t.Fatalf("the caller's registry used for the owner's DevPod: %v", out)
		}
	})
}
