// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/gatewaycli"
	"github.com/mrhaoxx/devpod/internal/gwlink"
)

// The CLI runs in the console: a gateway that signed a user in opens a
// WebSocket on the console's HTTP port, its request signed as the
// gateway's, and speaks SSH on it — logging in as that user with the key
// it signs into DevPods with, as a DevPod would trust it. The CLI then
// answers as that user.
func TestCLIInTheConsole(t *testing.T) {
	w := setupWorld(t)
	ctx := context.Background()
	web := httptest.NewServer(w.h)
	defer web.Close()
	t.Cleanup(func() { _ = w.a.Delete(ctx, &devpodv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "cli-gw"}}) })

	// a gateway, joined, that has said which key it signs into DevPods with
	code, out, _ := w.call("root", "POST", "/api/admin/gateways", `{"name":"cli-gw","sshAddress":"ssh.example.com:22"}`)
	token, _ := out["joinToken"].(string)
	if code != http.StatusCreated {
		t.Fatalf("register: %d %v", code, out)
	}
	id, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "gw")))
	if err != nil {
		t.Fatal(err)
	}
	if err := gwlink.Join(ctx, nil, web.URL, "cli-gw", token, id); err != nil {
		t.Fatalf("join: %v", err)
	}
	gw := gwlink.NewClient(gwlink.Options{Console: web.URL, Gateway: "cli-gw", Identity: id})
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	internal, _ := ssh.NewSignerFromKey(priv)
	internalLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(internal.PublicKey())))
	w.until("the gateway's key is recorded", func() bool {
		_, err := gw.Report(ctx, gwlink.Report{Version: "test", InternalKey: internalLine})
		var g devpodv1alpha1.Gateway
		return err == nil && w.a.Get(ctx, types.NamespacedName{Name: "cli-gw"}, &g) == nil && g.Status.InternalKey == internalLine
	})

	// the console's CLI calls its own API over loopback, with a key of its own
	key := []byte("loopback-only-key")
	w.s.CLILoopbackKey = key
	w.s.CLI = &gatewaycli.Server{API: gatewaycli.NewAPI(web.URL, string(key))}

	login := func(as string, signer ssh.Signer, q gwlink.CLIRequest) (*ssh.Client, error) {
		t.Helper()
		conn, err := gw.DialCLI(ctx, q)
		if err != nil {
			return nil, err
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, "console", &ssh.ClientConfig{
			User: as, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return ssh.NewClient(c, chans, reqs), nil
	}
	alice := gwlink.CLIRequest{User: "gl-alice", Remote: "203.0.113.9:5555", AuthKind: "direct"}
	c, err := login("gl-alice", internal, alice)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	got, err := sess.CombinedOutput("whoami")
	if err != nil || !strings.Contains(string(got), "gl-alice") {
		t.Fatalf("whoami: %v %s", err, got)
	}
	_ = c.Close()

	// the login must be the user the request named, with the gateway's key
	if _, err := login("root", internal, alice); err == nil {
		t.Fatal("logged in as someone the gateway did not name")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)
	if _, err := login("gl-alice", otherSigner, alice); err == nil {
		t.Fatal("logged in with a key the gateway did not report")
	}
	// and the request must be a gateway's
	if _, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(web.URL, "http")+gwlink.CLIPath+"?user=gl-alice", nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unsigned request: %v %v", err, resp)
	}
	// the loopback key is good from loopback only
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.RemoteAddr = "10.1.2.3:4567"
	req.Header.Set("Authorization", "Bearer gwk_"+string(key))
	req.Header.Set("X-DevPod-User", "gl-alice")
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the loopback key from elsewhere: %d %s", rec.Code, rec.Body)
	}
}
