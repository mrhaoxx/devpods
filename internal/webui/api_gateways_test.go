// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/gwlink"
	"github.com/mrhaoxx/devpod/internal/httpauth"
	"github.com/mrhaoxx/devpod/internal/tunnel"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// A gateway of several clusters: registered by an admin, joined with a
// token, and from then on asking the console — signed — what a gateway
// in a cluster reads from that cluster.
func TestGateways(t *testing.T) {
	w := setupWorld(t)
	ctx := context.Background()
	web := httptest.NewServer(w.h)
	defer web.Close()
	t.Cleanup(func() { _ = w.a.Delete(ctx, &devpodv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "b300"}}) })

	// only admins register gateways
	const reg = `{"name":"b300","displayName":"B300","sshAddress":"ssh.example.com:2222","httpBaseDomain":"apps.example.com","agentAddress":"https://gw.example.com"}`
	if code, _, _ := w.call("gl-alice", "POST", "/api/admin/gateways", reg); code != http.StatusForbidden {
		t.Fatalf("a user registering a gateway: %d", code)
	}
	code, out, _ := w.call("root", "POST", "/api/admin/gateways", reg)
	token, _ := out["joinToken"].(string)
	if code != http.StatusCreated || !strings.HasPrefix(token, "dpj_") {
		t.Fatalf("register: %d %v", code, out)
	}
	if code, _, _ := w.call("root", "POST", "/api/admin/gateways", reg); code != http.StatusConflict {
		t.Fatalf("the same name twice: %d", code)
	}

	// join: once
	id, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "gw")))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "other")))
	if err := gwlink.Join(ctx, nil, web.URL, "b300", "dpj_wrong", id); err == nil {
		t.Fatal("joined with a wrong token")
	}
	if err := gwlink.Join(ctx, nil, web.URL, "b300", token, id); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := gwlink.Join(ctx, nil, web.URL, "b300", token, other); err == nil {
		t.Fatal("the token worked a second time")
	}
	w.until("the console knows the gateway's key", func() bool {
		var g devpodv1alpha1.Gateway
		return k8sClient.Get(ctx, types.NamespacedName{Name: "b300"}, &g) == nil && g.Spec.PublicKey != ""
	})

	// what everyone sees of it: where to connect, not its key
	_, out, _ = w.call("gl-alice", "GET", "/api/gateways", "")
	list, _ := out["items"].([]any)
	if len(list) != 1 {
		t.Fatalf("the list: %v", out)
	}
	if g, _ := list[0].(map[string]any); g["sshAddress"] != "ssh.example.com:2222" || g["httpBaseDomain"] != "apps.example.com" || g["publicKey"] != nil || g["joined"] != nil {
		t.Fatalf("a user's view: %v", g)
	}

	// who the gateway asks about
	if err := w.a.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-alice"}, Spec: devpodv1alpha1.UserSpec{
		Pubkeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFzmR0Dk5Lk5gGk4Sx0dH5ZyqVQk1o3o8o4S0r9o6m0f alice"}, PasswordHash: "x"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.a.Delete(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-alice"}}) })
	const mk = `{"name":"x","image":"ubuntu:24.04"}`
	for _, c := range []string{"a", "b"} {
		if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/"+c+"/devpods", mk); code != http.StatusCreated {
			t.Fatalf("create in %s: %d %v", c, code, out)
		}
	}
	if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/b/devpods", `{"name":"only","image":"ubuntu:24.04"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	// a port-link id on b's DevPod
	var dp devpodv1alpha1.DevPod
	if err := w.b.Get(ctx, types.NamespacedName{Name: "gl-alice-only", Namespace: "devpods"}, &dp); err != nil {
		t.Fatal(err)
	}
	const linkID = "abcdefghijkl"
	if !httpauth.ValidID(linkID) {
		t.Fatalf("test id %q is not a valid id", linkID)
	}
	if dp.Annotations == nil {
		dp.Annotations = map[string]string{}
	}
	dp.Annotations[httpauth.IDAnnotation] = linkID
	if err := w.b.Update(ctx, &dp); err != nil {
		t.Fatal(err)
	}

	gw := gwlink.NewClient(gwlink.Options{Console: web.URL, Gateway: "b300", Identity: id, TTL: time.Millisecond})
	w.until("the console sees the DevPods and the user", func() bool {
		found, _, err := gw.Lookup(ctx, "gl-alice", "x", "")
		keys, _, kerr := gw.Keys(ctx, "gl-alice")
		link, _, lerr := gw.ByHTTPID(ctx, linkID)
		return err == nil && len(found) == 2 && kerr == nil && len(keys) == 1 && lerr == nil && link != nil
	})
	found, _, _ := gw.Lookup(ctx, "gl-alice", "x", "")
	if found[0].Cluster != "a" || found[1].Cluster != "b" || found[0].DevPod.Name != "gl-alice-x" || found[0].DevPod.Spec.Owner != "gl-alice" {
		t.Fatalf("a name both clusters have: %+v", found)
	}
	if found, _, err := gw.Lookup(ctx, "gl-alice", "x", "b"); err != nil || len(found) != 1 || found[0].Cluster != "b" {
		t.Fatalf("with the cluster: %+v %v", found, err)
	}
	if found, _, err := gw.Lookup(ctx, "gl-alice", "nothing", ""); err != nil || len(found) != 0 {
		t.Fatalf("a name nobody has: %+v %v", found, err)
	}
	if found, _, err := gw.Lookup(ctx, "gl-alice", "x", "nope"); err != nil || len(found) != 0 {
		t.Fatalf("a cluster that does not exist: %+v %v", found, err)
	}
	link, _, err := gw.ByHTTPID(ctx, linkID)
	if err != nil || link.Cluster != "b" || link.DevPod.Name != "gl-alice-only" {
		t.Fatalf("the port-link id: %+v %v", link, err)
	}
	if _, _, err := gw.ByHTTPID(ctx, "zzzzzzzzzzzz"); !errors.Is(err, gwlink.ErrNotFound) {
		t.Fatalf("an id nobody has: %v", err)
	}
	// keys, and nothing else of the user
	keys, _, _ := gw.Keys(ctx, "gl-alice")
	if !strings.HasPrefix(keys[0], "ssh-ed25519 ") {
		t.Fatalf("keys: %v", keys)
	}
	if _, _, err := gw.Keys(ctx, "gl-nobody"); !errors.Is(err, gwlink.ErrNotFound) {
		t.Fatalf("a user the console does not know: %v", err)
	}
	// whose agents it may accept
	clusters, _, err := gw.Clusters(ctx)
	if err != nil || len(clusters) != 2 || clusters[0].Name != "a" || !strings.HasPrefix(clusters[0].AgentPublicKey, "ssh-ed25519 ") {
		t.Fatalf("clusters: %+v %v", clusters, err)
	}
	// what it says of itself is shown to admins
	if err := gw.Report(ctx, gwlink.Report{Version: "v0.8.0", Clusters: []string{"a"}}); err != nil {
		t.Fatalf("report: %v", err)
	}
	w.until("the report is recorded", func() bool {
		_, out, _ := w.call("root", "GET", "/api/admin/gateways", "")
		list, _ := out["items"].([]any)
		if len(list) != 1 {
			return false
		}
		g, _ := list[0].(map[string]any)
		cl, _ := g["clusters"].([]any)
		return g["version"] == "v0.8.0" && len(cl) == 1 && g["joined"] == true && g["lastSeen"] != nil
	})

	// a replica of the gateway keeps a link to the console; over it the
	// console reaches that replica's live-session endpoint, wherever it runs
	w.s.GatewayLinks = webui.NewGatewayLinks(k8sManager.GetAPIReader(), "console-0", nil)
	defer w.s.GatewayLinks.Close()
	lctx, stopLink := context.WithCancel(ctx)
	defer stopLink()
	ln := tunnel.NewListener()
	replica := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte("live endpoint of gw-pod-1: " + r.Method + " " + r.URL.Path))
	})}
	go func() { _ = replica.Serve(ln) }()
	defer replica.Close()
	connect := func(name string, key *agent.Identity) *tunnel.Client {
		c := tunnel.NewClient(tunnel.ClientOptions{
			Hello: tunnel.Hello{Kind: gwlink.LinkKind, Name: name},
			Sign:  func(nonce []byte) []byte { return key.SignFor(gwlink.LinkContext, nonce) },
			Idle:  2, BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
			Handle: func(_ context.Context, purpose []byte, conn net.Conn) {
				if string(purpose) != string(gwlink.PurposeLive) {
					_ = conn.Close()
					return
				}
				ln.Offer(conn)
			},
			Dial: func(context.Context) (net.Conn, error) {
				c, _, err := websocket.Dial(lctx, "ws"+strings.TrimPrefix(web.URL, "http")+"/gateway/connect", nil)
				if err != nil {
					return nil, err
				}
				c.SetReadLimit(-1)
				return websocket.NetConn(lctx, c, websocket.MessageBinary), nil
			},
		})
		go func() { _ = c.Run(lctx) }()
		return c
	}
	connect("b300.gw-pod-1", id)
	connect("b300.gw-imposter", other) // the name of a gateway, not its key
	var addr string
	w.until("the console holds a link to the replica", func() bool {
		var ok bool
		addr, ok = w.s.GatewayLinks.AddrOf("gw-pod-1")
		return ok
	})
	hc := &http.Client{Transport: &http.Transport{DialContext: w.s.GatewayLinks.DialContext}, Timeout: 10 * time.Second}
	lresp, err := hc.Get("http://" + addr + "/live/session-1")
	if err != nil {
		t.Fatalf("the replica's endpoint over its link: %v", err)
	}
	lbody, _ := io.ReadAll(lresp.Body)
	_ = lresp.Body.Close()
	if string(lbody) != "live endpoint of gw-pod-1: GET /live/session-1" {
		t.Fatalf("over the link: %q", lbody)
	}
	if _, ok := w.s.GatewayLinks.AddrOf("gw-imposter"); ok {
		t.Fatal("a replica with another key got a link")
	}
	if _, ok := w.s.GatewayLinks.AddrOf("gw-pod-2"); ok {
		t.Fatal("a replica that never connected has an address")
	}

	// nobody else gets these answers
	stranger := gwlink.NewClient(gwlink.Options{Console: web.URL, Gateway: "b300", Identity: other})
	if _, _, err := stranger.Keys(ctx, "gl-alice"); err == nil || errors.Is(err, gwlink.ErrNotFound) {
		t.Fatalf("another key asking as b300: %v", err)
	}
	resp, err := http.Get(web.URL + "/gateway/v1/keys?user=gl-alice")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unsigned request: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	// a paused gateway is refused
	if code, out, _ := w.call("root", "PATCH", "/api/admin/gateways/b300", `{"paused":true}`); code != http.StatusOK {
		t.Fatalf("pause: %d %v", code, out)
	}
	w.until("the paused gateway is refused", func() bool {
		_, _, err := gw.Keys(ctx, "gl-alice")
		return err != nil && !errors.Is(err, gwlink.ErrNotFound)
	})
	if code, _, _ := w.call("root", "PATCH", "/api/admin/gateways/b300", `{"paused":false,"sshAddress":"ssh2.example.com"}`); code != http.StatusOK {
		t.Fatal("resume")
	}

	// a cluster can be pointed at a gateway; one that does not exist is refused
	if code, out, _ := w.call("root", "PATCH", "/api/admin/clusters/b", `{"gateway":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("a cluster pointed at a gateway that does not exist: %d %v", code, out)
	}
	if code, out, _ := w.call("root", "PATCH", "/api/admin/clusters/b", `{"gateway":"b300"}`); code != http.StatusOK {
		t.Fatalf("point b at b300: %d %v", code, out)
	}
	_, out, _ = w.call("gl-alice", "GET", "/api/clusters", "")
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"gateway":"b300"`) {
		t.Fatalf("the cluster's gateway is not shown: %s", raw)
	}

	// what users are shown for a DevPod of b: that gateway's address, the
	// login with the cluster, and port links on that gateway's domain
	hp, err := webui.NewHTTPProxy("{id}-{port}.console.test", "https", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	w.s.HTTPProxy = hp
	w.until("the console sees where b's gateway is", func() bool {
		_, me, _ := w.call("gl-alice", "GET", "/api/me", "")
		cl, _ := me["clusters"].(map[string]any)
		cb, _ := cl["b"].(map[string]any)
		ssh, _ := cb["ssh"].(map[string]any)
		return ssh["host"] == "ssh2.example.com" && ssh["port"] == float64(22) && ssh["loginSuffix"] == "b"
	})
	code, out, _ = w.call("gl-alice", "GET", "/api/clusters/b/devpods/gl-alice-only/http", "")
	if tpl, _ := out["urlTemplate"].(string); code != http.StatusOK || tpl != "https://"+linkID+"-{port}.apps.example.com" {
		t.Fatalf("a port link of b: %d %v", code, out)
	}
	// a ticket is given for that gateway's domain, and for no other
	code, out, _ = w.call("gl-alice", "POST", "/api/http/ticket", `{"host":"`+linkID+`-8080.apps.example.com","rd":"/"}`)
	if u, _ := out["url"].(string); code != http.StatusOK || !strings.HasPrefix(u, "https://"+linkID+"-8080.apps.example.com/") {
		t.Fatalf("a ticket for b's gateway: %d %v", code, out)
	}
	if code, out, _ := w.call("gl-alice", "POST", "/api/http/ticket", `{"host":"`+linkID+`-8080.apps.evil.example","rd":"/"}`); code != http.StatusBadRequest {
		t.Fatalf("a ticket for a domain no gateway has: %d %v", code, out)
	}

	if code, _, _ := w.call("root", "DELETE", "/api/admin/gateways/b300", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
}
