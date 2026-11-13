// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// devpod-agent runs in a cluster of DevPods and dials out to the console
// that manages it. See internal/agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// version is set at build time.
var version = "dev"

func main() {
	var (
		cluster, console, namespace, devpodNamespace string
		identitySecret, identityFile, tokenFile      string
		capabilities, controller                     string
		idle                                         int
		telemetryListen                              string
	)
	flag.StringVar(&cluster, "cluster", "", "this cluster's name, as registered at the console (required)")
	flag.StringVar(&console, "console", "", "the console's address for agents, e.g. https://devpod.example.com (required)")
	flag.StringVar(&namespace, "namespace", "devpod-system", "namespace the agent runs in: its identity Secret and the controller Deployment are here")
	flag.StringVar(&devpodNamespace, "devpod-namespace", "devpods", "namespace of the DevPods: gateways are connected to DevPods of this namespace and to nothing else")
	flag.StringVar(&identitySecret, "identity-secret", "devpod-agent-identity", "Secret holding the agent's own key; made on the first start")
	flag.StringVar(&identityFile, "identity-file", "", "keep the key in this file instead of a Secret (outside a cluster)")
	flag.StringVar(&tokenFile, "join-token-file", "", "file with the one-time token from the console; used once, on the first start")
	flag.StringVar(&capabilities, "capabilities", "", "what this cluster offers, comma separated: snapshots, reservations, telemetry")
	flag.StringVar(&controller, "controller-deployment", "devpod-controller", "the controller's Deployment, whose image tag is reported as its version")
	flag.IntVar(&idle, "idle-connections", 8, "unused data connections kept on each instance of the console and of each gateway")
	flag.StringVar(&telemetryListen, "telemetry-listen", "", "where the agent takes the telemetry batches of this cluster's DevPods, to take them to the console over its link (empty: no telemetry)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if cluster == "" || console == "" {
		fatal(errors.New("--cluster and --console are required"))
	}

	// in a cluster: the Pod's service account; outside one: --kubeconfig or $KUBECONFIG
	cfg, err := config.GetConfig()
	if err != nil {
		fatal(fmt.Errorf("kubernetes config: %w", err))
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var store agent.Store = agent.SecretStore(kc, namespace, identitySecret)
	if identityFile != "" {
		store = agent.FileStore(identityFile)
	}
	id, err := agent.LoadOrCreateIdentity(store)
	if err != nil {
		fatal(err)
	}
	slog.Info("agent_identity", "cluster", cluster, "publicKey", id.PublicKey())

	if tokenFile != "" {
		if err := join(ctx, console, cluster, tokenFile, id); err != nil {
			fatal(fmt.Errorf("join: %w", err))
		}
	}

	// the DevPods of this cluster, kept in memory: what a gateway asks to
	// be connected to is checked against them
	scheme := runtime.NewScheme()
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
	devpods, err := cache.New(cfg, cache.Options{Scheme: scheme, ByObject: map[client.Object]cache.ByObject{
		&devpodv1alpha1.DevPod{}: {Namespaces: map[string]cache.Config{devpodNamespace: {}}},
	}})
	if err != nil {
		fatal(err)
	}
	go func() {
		if err := devpods.Start(ctx); err != nil {
			fatal(fmt.Errorf("devpod cache: %w", err))
		}
	}()

	// one new connection to the console, or to a gateway: a WebSocket
	ws := func(address string) (net.Conn, error) {
		endpoint := strings.Replace(strings.TrimRight(address, "/"), "http", "ws", 1) + "/agent/connect"
		dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(dctx, endpoint, nil)
		if err != nil {
			return nil, err
		}
		c.SetReadLimit(-1) // streams carry whatever their two ends say
		// the connection lives as long as the agent, not as long as the dial
		return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
	}

	// the DevPods' telemetry goes to the console over the agent's link: the
	// console opens telemetry streams, the batches go on them
	var telemetry *agent.TelemetryLink
	if telemetryListen != "" {
		telemetry = agent.NewTelemetryLink()
		ts := &http.Server{Addr: telemetryListen, Handler: telemetry.Forward(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			slog.Info("telemetry_over_link", "listen", telemetryListen)
			if err := ts.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatal(fmt.Errorf("--telemetry-listen: %w", err))
			}
		}()
		go func() {
			<-ctx.Done()
			_ = ts.Close()
		}()
	}

	// what other workloads hold on each node, for the console to count
	usage := agent.NewPodUsage(kc, devpodNamespace)
	go usage.Run(ctx)
	caps := splitList(capabilities)
	if telemetry != nil {
		caps = append(caps, agent.CapTelemetryLink)
	}
	a, err := agent.New(agent.Options{
		Cluster: cluster, Identity: id, API: cfg,
		Namespace: devpodNamespace, DevPods: devpods,
		DialGateway:   func(_ context.Context, address string) (net.Conn, error) { return ws(address) },
		GatewayTunnel: tunnel.ClientOptions{Idle: idle},
		// the gateways that serve this cluster, for its DevPods to trust:
		// the controller reads them from this ConfigMap
		PublishGatewayKeys: func(ctx context.Context, keys []string) error {
			return publishGatewayKeys(ctx, kc, devpodNamespace, keys)
		},
		Telemetry: telemetry,
		Info: func(ctx context.Context) agent.Info {
			in := agent.Info{AgentVersion: version, Capabilities: caps, Nodes: usage.Nodes()}
			if v, err := kc.Discovery().ServerVersion(); err == nil {
				in.KubernetesVersion = v.GitVersion
			}
			if d, err := kc.AppsV1().Deployments(namespace).Get(ctx, controller, metav1.GetOptions{}); err == nil && len(d.Spec.Template.Spec.Containers) > 0 {
				img := d.Spec.Template.Spec.Containers[0].Image
				if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
					in.ControllerVersion = img[i+1:]
				}
			}
			return in
		},
	})
	if err != nil {
		fatal(err)
	}
	slog.Info("agent_start", "cluster", cluster, "console", console, "version", version)
	err = a.Run(ctx, tunnel.ClientOptions{
		Idle: idle,
		Dial: func(context.Context) (net.Conn, error) { return ws(console) },
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
}

func fatal(err error) {
	slog.Error("agent_fatal", "err", err)
	os.Exit(1)
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// publishGatewayKeys writes the gateways' internal keys into the ConfigMap
// the controller reads (one authorized_keys line each).
func publishGatewayKeys(ctx context.Context, kc kubernetes.Interface, ns string, keys []string) error {
	data := map[string]string{devpodv1alpha1.GatewayKeysKey: ""}
	if len(keys) > 0 {
		data[devpodv1alpha1.GatewayKeysKey] = strings.Join(keys, "\n") + "\n"
	}
	cms := kc.CoreV1().ConfigMaps(ns)
	cur, err := cms.Get(ctx, devpodv1alpha1.GatewayKeysConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.GatewayKeysConfigMap, Namespace: ns,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "devpod-agent"}},
			Data: data,
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if cur.Data[devpodv1alpha1.GatewayKeysKey] == data[devpodv1alpha1.GatewayKeysKey] {
		return nil
	}
	cur.Data = data
	_, err = cms.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}
