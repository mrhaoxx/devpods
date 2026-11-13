// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/gateway"
	"github.com/mrhaoxx/devpod/internal/gwlink"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// A gateway of several clusters (--console): it asks the console which
// DevPod a login means and for users' keys, and reaches DevPods over
// connections the clusters' agents make to it. It needs no route to any
// cluster, and nothing of a cluster's API.

type hubFlags struct {
	console, name, identityFile, identitySecret, namespace, joinTokenFile, agentListen, instance, instanceSelector *string
}

func addHubFlags(fs *flag.FlagSet) *hubFlags {
	host, _ := os.Hostname()
	return &hubFlags{
		console: fs.String("console", "", "the console's address, e.g. https://devpod.example.com: serve the clusters that console manages instead of the cluster this gateway runs in"),
		name:    fs.String("gateway-name", "", "this gateway's name, as registered at the console (with --console)"),
		identityFile: fs.String("gateway-identity-file", "",
			"keep the gateway's own key in this file instead of a Secret"),
		identitySecret: fs.String("gateway-identity-secret", "devpod-gateway-identity", "Secret holding the gateway's own key; made on the first start"),
		namespace:      fs.String("gateway-namespace", envOr("POD_NAMESPACE", "devpod-system"), "namespace the gateway runs in (default: $POD_NAMESPACE)"),
		joinTokenFile:  fs.String("join-token-file", "", "file with the one-time token from the console; used once, on the first start"),
		agentListen:    fs.String("agent-listen", ":8443", "HTTP address the clusters' agents connect to (/agent/connect, a WebSocket; TLS is the route's in front)"),
		instance:       fs.String("instance", envOr("POD_NAME", host), "this replica's name among the gateway's replicas"),
		instanceSelector: fs.String("instance-selector", "",
			"label selector of the gateway's Pods; agents connect to every one of them. Empty: this replica alone"),
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func (f *hubFlags) enabled() bool { return *f.console != "" }

// hub is a gateway's connections: to the console (questions) and from
// the agents (the way to DevPods).
type hub struct {
	link *gwlink.Client
	srv  *tunnel.Server

	id                      *agent.Identity
	console, name, instance string
	// internalKey: the key this gateway signs into DevPods with, reported
	// for the clusters it serves to trust
	internalKey string
	// proxies take the keys the console logs in with (its web terminal)
	proxies *gateway.ProxyKeys
}

func (f *hubFlags) start(ctx context.Context, kc kubernetes.Interface, internalKey string, proxies *gateway.ProxyKeys) (*hub, error) {
	if *f.name == "" {
		return nil, errors.New("--console needs --gateway-name")
	}
	var store agent.Store = agent.SecretStore(kc, *f.namespace, *f.identitySecret)
	if *f.identityFile != "" {
		store = agent.FileStore(*f.identityFile)
	}
	id, err := agent.LoadOrCreateIdentity(store)
	if err != nil {
		return nil, err
	}
	slog.Info("gateway_identity", "gateway", *f.name, "publicKey", id.PublicKey())
	if *f.joinTokenFile != "" {
		token, err := os.ReadFile(*f.joinTokenFile)
		if err != nil {
			return nil, err
		}
		jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = gwlink.Join(jctx, nil, *f.console, *f.name, strings.TrimSpace(string(token)), id)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("join: %w", err)
		}
	}
	h := &hub{link: gwlink.NewClient(gwlink.Options{Console: *f.console, Gateway: *f.name, Identity: id}),
		id: id, console: *f.console, name: *f.name, instance: *f.instance, internalKey: internalKey, proxies: proxies}
	instances := func() []string { return []string{*f.instance} }
	if *f.instanceSelector != "" {
		instances = podNames(kc, *f.namespace, *f.instanceSelector, *f.instance)
	}
	h.srv = tunnel.NewServer(tunnel.ServerOptions{
		Instance: *f.instance, Instances: instances,
		// an agent is who it says when its proof matches the key the
		// console holds for that cluster
		Authorize: func(ctx context.Context, hello tunnel.Hello, nonce, proof []byte) (string, error) {
			if hello.Kind != agent.Kind {
				return "", fmt.Errorf("%q does not connect here", hello.Kind)
			}
			known, _, err := h.link.Clusters(ctx)
			if err != nil {
				return "", errors.New("the gateway cannot ask the console which clusters there are")
			}
			for _, c := range known {
				if c.Name == hello.Name {
					return c.Name, agent.VerifyFor(c.AgentPublicKey, agent.AtGatewayContext(*f.name), nonce, proof)
				}
			}
			return "", errors.New("no such cluster")
		},
		Sign: func(nonce []byte) []byte { return id.SignFor(agent.GatewayProofContext, nonce) },
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /agent/connect", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(-1)
		h.srv.Accept(r.Context(), websocket.NetConn(r.Context(), c, websocket.MessageBinary))
	})
	server := &http.Server{Addr: *f.agentListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *f.agentListen)
	if err != nil {
		return nil, fmt.Errorf("--agent-listen: %w", err)
	}
	go func() {
		slog.Info("agents_listening", "addr", *f.agentListen)
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("agents_server", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		h.srv.Close()
		_ = server.Close()
	}()
	go h.report(ctx)
	return h, nil
}

// report tells the console, now and every half minute, which clusters'
// agents this gateway holds connections from; the console answers with
// the keys it logs in with as a trusted proxy.
func (h *hub) report(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var last []string
	for {
		peers := h.srv.Peers()
		names := make([]string, 0, len(peers))
		for name := range peers {
			names = append(names, name)
		}
		slices.Sort(names)
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ans, err := h.link.Report(rctx, gwlink.Report{Version: version, Clusters: names, InternalKey: h.internalKey})
		cancel()
		if err != nil {
			slog.Warn("gateway_report", "err", err)
		} else {
			last = names // what the console was told; a report that failed is said again soon
			h.trustConsole(ans.TrustedProxyKeys)
		}
		// a change is said soon, not at the next half minute
		soon := time.NewTimer(2 * time.Second)
	wait:
		for {
			select {
			case <-ctx.Done():
				soon.Stop()
				return
			case <-tick.C:
				break wait
			case <-soon.C:
				now := make([]string, 0, len(last))
				for name := range h.srv.Peers() {
					now = append(now, name)
				}
				slices.Sort(now)
				if !slices.Equal(now, last) {
					break wait
				}
				soon.Reset(2 * time.Second)
			}
		}
		soon.Stop()
	}
}

// trustConsole makes the keys the console said it logs in with this
// gateway's trusted proxies besides the GatewayConfig's: the console
// already says which keys each user has, so this trusts it with nothing
// more.
func (h *hub) trustConsole(keys []devpodv1alpha1.TrustedProxyKey) {
	idx := map[string]string{}
	for _, k := range keys {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Pubkey))
		if err != nil || k.Alias == "" {
			slog.Warn("console_proxy_key", "alias", k.Alias, "err", fmt.Sprint(err))
			continue
		}
		idx[ssh.FingerprintSHA256(pk)] = k.Alias
	}
	h.proxies.SetConsole(idx)
}

// linkConsole keeps connections from this replica to every instance of
// the console: the console cannot dial a gateway that is elsewhere. Over
// them it reaches live (watching a session, ending one; nil: not here)
// and this gateway's SSH, which the returned listener gives out.
func (h *hub) linkConsole(ctx context.Context, live http.Handler) net.Listener {
	ln, sshLn := tunnel.NewListener(), tunnel.NewListener()
	if live != nil {
		server := &http.Server{Handler: live, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = server.Serve(ln) }()
		go func() {
			<-ctx.Done()
			_ = server.Close()
		}()
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = sshLn.Close()
	}()
	endpoint := strings.Replace(strings.TrimRight(h.console, "/"), "http", "ws", 1) + "/gateway/connect"
	c := tunnel.NewClient(tunnel.ClientOptions{
		Hello: tunnel.Hello{Kind: gwlink.LinkKind, Name: h.name + "." + h.instance},
		Sign:  func(nonce []byte) []byte { return h.id.SignFor(gwlink.LinkContext, nonce) },
		Idle:  4,
		Dial: func(context.Context) (net.Conn, error) {
			dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(dctx, endpoint, nil)
			if err != nil {
				return nil, err
			}
			c.SetReadLimit(-1)
			return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
		},
		Handle: func(_ context.Context, purpose []byte, conn net.Conn) {
			switch {
			case string(purpose) == string(gwlink.PurposeSSH):
				sshLn.Offer(fromConsole{conn})
			case string(purpose) == string(gwlink.PurposeLive) && live != nil:
				ln.Offer(conn)
			default:
				_ = conn.Close() // the console is given these here, nothing else
			}
		},
		OnError: func(err error) { slog.Warn("gateway_console_link", "err", err.Error()) },
	})
	go func() { _ = c.Run(ctx) }()
	return sshLn
}

// fromConsole is an SSH connection the console made over the link: its
// source reads "console".
type fromConsole struct{ net.Conn }

func (fromConsole) RemoteAddr() net.Addr { return consoleAddr{} }

type consoleAddr struct{}

func (consoleAddr) Network() string { return "tunnel" }
func (consoleAddr) String() string  { return "console" }

// dial is the way to a DevPod's address: through its cluster's agent.
func (h *hub) dial(ctx context.Context, t gateway.Target) (net.Conn, error) {
	conn, err := h.srv.Dial(ctx, t.Cluster, agent.DialPurpose(t.Addr, t.Purpose, t.DevPodUID, t.User))
	if errors.Is(err, tunnel.ErrPeerOffline) {
		return nil, fmt.Errorf("cluster %q is not connected to this gateway", t.Cluster)
	}
	if err != nil {
		return nil, err
	}
	// the connection says the agent's end of it as the DevPod sees it: the
	// session records that, and a shell's commands are matched by it
	c, err := agent.AwaitDial(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// sources swaps the cluster's User objects for the keys the console holds.
func (h *hub) sources(srcs []gateway.IdentitySource) []gateway.IdentitySource {
	out := []gateway.IdentitySource{gateway.NewConsoleSource(h.link)}
	for _, s := range srcs {
		if s.Name() != "crd" {
			out = append(out, s)
		}
	}
	return out
}

// podNames lists the gateway's running Pods, asked at most every few
// seconds; this replica is always among them.
func podNames(kc kubernetes.Interface, ns, selector, self string) func() []string {
	var (
		mu   sync.Mutex
		last = []string{self}
		at   time.Time
	)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < 5*time.Second {
			return last
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pods, err := kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return last
		}
		names := []string{self}
		for _, p := range pods.Items {
			if p.Name != self && p.DeletionTimestamp == nil && p.Status.Phase == "Running" {
				names = append(names, p.Name)
			}
		}
		slices.Sort(names)
		last, at = names, time.Now()
		return last
	}
}
