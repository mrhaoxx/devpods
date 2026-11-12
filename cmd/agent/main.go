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
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// version is set at build time.
var version = "dev"

func main() {
	var (
		cluster, console, namespace, identitySecret, identityFile, tokenFile, capabilities, controller string
		idle                                                                                           int
	)
	flag.StringVar(&cluster, "cluster", "", "this cluster's name, as registered at the console (required)")
	flag.StringVar(&console, "console", "", "the console's address for agents, e.g. https://devpod.example.com (required)")
	flag.StringVar(&namespace, "namespace", "devpod-system", "namespace the agent runs in: its identity Secret and the controller Deployment are here")
	flag.StringVar(&identitySecret, "identity-secret", "devpod-agent-identity", "Secret holding the agent's own key; made on the first start")
	flag.StringVar(&identityFile, "identity-file", "", "keep the key in this file instead of a Secret (outside a cluster)")
	flag.StringVar(&tokenFile, "join-token-file", "", "file with the one-time token from the console; used once, on the first start")
	flag.StringVar(&capabilities, "capabilities", "", "what this cluster offers, comma separated: snapshots, reservations, telemetry")
	flag.StringVar(&controller, "controller-deployment", "devpod-controller", "the controller's Deployment, whose image tag is reported as its version")
	flag.IntVar(&idle, "idle-connections", 8, "unused data connections kept on each console instance")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if cluster == "" || console == "" {
		fatal(errors.New("--cluster and --console are required"))
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		fatal(fmt.Errorf("in-cluster config: %w", err))
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var store agent.Store = secretStore{kc: kc, ns: namespace, name: identitySecret}
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

	caps := splitList(capabilities)
	a, err := agent.New(agent.Options{
		Cluster: cluster, Identity: id, API: cfg,
		Info: func(ctx context.Context) agent.Info {
			in := agent.Info{AgentVersion: version, Capabilities: caps}
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
	endpoint := strings.Replace(strings.TrimRight(console, "/"), "http", "ws", 1) + "/agent/connect"
	slog.Info("agent_start", "cluster", cluster, "console", endpoint, "version", version)
	err = a.Run(ctx, tunnel.ClientOptions{
		Idle: idle,
		Dial: func(context.Context) (net.Conn, error) {
			dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(dctx, endpoint, nil)
			if err != nil {
				return nil, err
			}
			c.SetReadLimit(-1) // streams carry whatever their two ends say
			// the connection lives as long as the agent, not as long as the dial
			return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
		},
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

// secretStore keeps the agent's key in a Secret of its own namespace.
type secretStore struct {
	kc       kubernetes.Interface
	ns, name string
}

func (s secretStore) Load() ([]byte, error) {
	sec, err := s.kc.CoreV1().Secrets(s.ns).Get(context.Background(), s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sec.Data["key"], nil
}

func (s secretStore) Save(b []byte) error {
	_, err := s.kc.CoreV1().Secrets(s.ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.ns},
		Data:       map[string][]byte{"key": b},
	}, metav1.CreateOptions{})
	return err
}
