// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

func devpodAt(name, ns, endpoint string, phase devpodv1alpha1.DevPodPhase) *devpodv1alpha1.DevPod {
	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "alice", Running: true, Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}},
		Status: devpodv1alpha1.DevPodStatus{Phase: phase, Endpoint: endpoint},
	}
}

func devpods(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// What a gateway may be connected to: a running DevPod's sshd for SSH,
// any other port of a running DevPod for HTTP — and nothing else in the
// cluster.
func TestDialRule(t *testing.T) {
	r := devpods(t,
		devpodAt("alice-a", "devpods", "10.0.0.5:22", devpodv1alpha1.DevPodRunning),
		devpodAt("alice-hostnet", "devpods", "192.168.1.9:2201", devpodv1alpha1.DevPodRunning),
		devpodAt("alice-stopped", "devpods", "10.0.0.7:22", devpodv1alpha1.DevPodStopped),
		devpodAt("elsewhere", "other", "10.0.9.9:22", devpodv1alpha1.DevPodRunning),
	)
	for _, tc := range []struct {
		addr, purpose string
		ok            bool
		why           string
	}{
		{"10.0.0.5:22", "ssh", true, "a DevPod's sshd"},
		{"192.168.1.9:2201", "ssh", true, "a host-network DevPod's sshd, on its own port"},
		{"10.0.0.5:8080", "http", true, "another port of a DevPod"},
		{"10.0.0.5:22", "http", false, "sshd as an HTTP service"},
		{"10.0.0.5:2222", "ssh", false, "SSH to a port that is not the DevPod's sshd"},
		{"192.168.1.9:22", "ssh", false, "the node's own sshd behind a host-network DevPod"},
		{"10.0.0.6:22", "ssh", false, "an address no DevPod has"},
		{"10.0.0.6:80", "http", false, "an address no DevPod has"},
		{"10.0.0.7:22", "ssh", false, "a DevPod that is not running"},
		{"10.0.9.9:22", "ssh", false, "a DevPod of another namespace"},
		{"10.96.0.1:443", "http", false, "the API server"},
		{"kubernetes.default.svc:443", "http", false, "a name, not an address"},
		{"10.0.0.5:22", "", false, "no purpose"},
		{"10.0.0.5:22", "kube", false, "a purpose that is not one"},
		{"10.0.0.5", "ssh", false, "no port"},
		{"10.0.0.5:0", "http", false, "port 0"},
		{"10.0.0.5:99999", "http", false, "a port that is not one"},
	} {
		err := agent.DialAllowed(context.Background(), r, "devpods", tc.addr, tc.purpose)
		if (err == nil) != tc.ok {
			t.Errorf("%s (%s to %s): %v", tc.why, tc.purpose, tc.addr, err)
		}
	}
}

// gatewayEnd is a gateway's end: a tunnel server that proves itself with
// its own key, which agents reach by dialling out.
type gatewayEnd struct {
	srv *tunnel.Server
	id  *agent.Identity
	ctx context.Context
}

func newGatewayEnd(t *testing.T, ctx context.Context, cluster string, agentID *agent.Identity) *gatewayEnd {
	t.Helper()
	g := &gatewayEnd{ctx: ctx}
	g.id, _ = agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "gw")))
	g.srv = tunnel.NewServer(tunnel.ServerOptions{
		Instance: "gw-0", Instances: func() []string { return []string{"gw-0"} },
		Authorize: func(_ context.Context, h tunnel.Hello, nonce, proof []byte) (string, error) {
			if h.Kind != agent.Kind || h.Name != cluster {
				return "", io.ErrUnexpectedEOF
			}
			return h.Name, agent.Verify(agentID.PublicKey(), nonce, proof)
		},
		Sign:      func(nonce []byte) []byte { return g.id.SignFor(agent.GatewayProofContext, nonce) },
		Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond,
	})
	return g
}

func (g *gatewayEnd) dial(context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	go g.srv.Accept(g.ctx, b)
	return a, nil
}

// open asks the cluster's agent for a connection to addr and returns it
// once the agent has said it is there.
func (g *gatewayEnd) open(ctx context.Context, cluster, addr, purpose string) (net.Conn, error) {
	conn, err := g.srv.Dial(ctx, cluster, agent.DialPurpose(addr, purpose))
	if err != nil {
		return nil, err
	}
	if err := agent.AwaitDial(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func TestGateways_TheAgentDialsOutAndServesDialOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// a DevPod's "sshd": something that answers on a local port
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()

	id, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	console := newHub(t, ctx, "c1", id)
	gw := newGatewayEnd(t, ctx, "c1", id)
	rogue := newGatewayEnd(t, ctx, "c1", id) // answers at an address the console named, with another key
	ends := map[string]*gatewayEnd{"gw.example": gw, "rogue.example": rogue}

	a, err := agent.New(agent.Options{
		Cluster: "c1", Identity: id, API: &rest.Config{Host: api.URL}, InfoInterval: 20 * time.Millisecond,
		Namespace: "devpods",
		DevPods:   devpods(t, devpodAt("alice-a", "devpods", echo.Addr().String(), devpodv1alpha1.DevPodRunning)),
		DialGateway: func(ctx context.Context, address string) (net.Conn, error) {
			return ends[strings.TrimPrefix(address, "https://")].dial(ctx)
		},
		GatewayTunnel: tunnel.ClientOptions{Idle: 2, Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = a.Run(ctx, tunnel.ClientOptions{Dial: console.dial, Idle: 4, Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond})
	}()
	waitPeer(t, console, "c1")

	// the console tells the agent which gateways there are, and their keys
	list, err := console.srv.Dial(ctx, "c1", agent.PurposeGateways)
	if err != nil {
		t.Fatal(err)
	}
	defer list.Close()
	tell := func(gws ...agent.Gateway) {
		t.Helper()
		if err := json.NewEncoder(list).Encode(gws); err != nil {
			t.Fatal(err)
		}
	}
	tell(agent.Gateway{Name: "b300", Address: "https://gw.example", PublicKey: gw.id.PublicKey()},
		agent.Gateway{Name: "rogue", Address: "https://rogue.example", PublicKey: gw.id.PublicKey()})
	until := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	until("the agent connects to the gateway", func() bool { p, ok := gw.srv.Peers()["c1"]; return ok && p.Idle > 0 })
	// one that does not hold the key the console named gets nothing to use
	time.Sleep(100 * time.Millisecond)
	if p := rogue.srv.Peers()["c1"]; p.Idle != 0 {
		t.Fatalf("the agent keeps connections on a gateway that did not prove itself: %+v", p)
	}

	// SSH to the DevPod's sshd: bytes both ways
	conn, err := gw.open(ctx, "c1", echo.Addr().String(), "ssh")
	if err != nil {
		t.Fatalf("dial a DevPod's sshd: %v", err)
	}
	_, _ = conn.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("through the agent: %q %v", buf, err)
	}
	_ = conn.Close()
	// what the rule refuses is said, not just dropped
	if _, err := gw.open(ctx, "c1", "10.96.0.1:443", "http"); err == nil || !strings.Contains(err.Error(), "not a DevPod") {
		t.Fatalf("the API server through the agent: %v", err)
	}
	if _, err := gw.open(ctx, "c1", echo.Addr().String(), "http"); err == nil {
		t.Fatal("sshd's port as HTTP was allowed")
	}
	// a gateway gets connections to DevPods and nothing else: not the cluster's API
	k, err := gw.srv.Dial(ctx, "c1", agent.PurposeKube)
	if err != nil {
		t.Fatal(err)
	}
	_ = k.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = k.Write([]byte("GET /api/v1/namespaces HTTP/1.1\r\nHost: x\r\n\r\n"))
	if n, err := k.Read(make([]byte, 16)); err == nil {
		t.Fatalf("a gateway reached the cluster's API through the agent (%d bytes)", n)
	}
	_ = k.Close()
	// and the console gets no connections to DevPods
	d, err := console.srv.Dial(ctx, "c1", agent.DialPurpose(echo.Addr().String(), "ssh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.AwaitDial(d); err == nil {
		t.Fatal("the console was given a connection to a DevPod")
	}
	_ = d.Close()

	// the agent says which gateways it is connected to
	info, err := console.srv.Dial(ctx, "c1", agent.PurposeInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer info.Close()
	var got agent.Info
	dec := json.NewDecoder(info)
	until("info names the gateway", func() bool {
		_ = info.SetReadDeadline(time.Now().Add(2 * time.Second))
		return dec.Decode(&got) == nil && len(got.Gateways) == 1 && got.Gateways[0] == "b300"
	})

	// a gateway the console no longer names is left
	tell()
	until("the agent leaves the gateway", func() bool { _, ok := gw.srv.Peers()["c1"]; return !ok })
}
