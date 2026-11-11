// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-controller runs the DevPod and User reconcilers.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
)

var setupLog = ctrl.Log.WithName("setup")

// die logs a setup error via the configured structured logger and exits.
func die(err error, msg string) {
	setupLog.Error(err, msg)
	os.Exit(1)
}

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		leaderElectionID     string
		enableLeaderElection bool
		devPodNamespace      string
		gatewayNamespace     string
		supervisorImage      string
		internalPubFile      string
		homeDirHostPath      string
		homeDirMountPrefix   string
		snapshotOptions      render.SnapshotOptions
		ingestURL            string
		ingestKeyFile        string
		tel                  = config.DefaultConfig()
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint bind address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "healthz/readyz bind address")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "enable leader election")
	flag.StringVar(&leaderElectionID, "leader-election-id", "devpod-controller", "leader election lock id")
	flag.StringVar(&devPodNamespace, "devpod-namespace", "devpods", "namespace where DevPod-owned objects live")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "devpod-system", "namespace where the DevPod gateway runs (referenced by per-owner NetworkPolicy)")
	flag.StringVar(&supervisorImage, "supervisor-image", "ghcr.io/mrhaoxx/devpod-supervisor:dev", "supervisor container image (initContainer payload: static sshd + supervisor binary)")
	flag.StringVar(&internalPubFile, "internal-pubkey-file", "",
		"path to the gateway internal public key (authorized_keys line); when set, embedded into per-DevPod host-key Secrets as the supervisor's authorized_keys entry")
	flag.StringVar(&homeDirHostPath, "home-host-path", "",
		"hostPath prefix for per-owner home directories (e.g. /mnt/afs/home); empty disables injection")
	flag.StringVar(&homeDirMountPrefix, "home-mount-prefix", "/home",
		"container mount prefix for home directories (e.g. /home → /home/{owner})")
	flag.StringVar(&snapshotOptions.DockerImage, "snapshot-image", "docker:cli",
		"container image for Docker snapshot Jobs (must contain docker CLI)")
	flag.StringVar(&snapshotOptions.ContainerdImage, "snapshot-containerd-image", "ghcr.io/mrhaoxx/devpod-snapshot:nerdctl-2.2.1",
		"container image for containerd snapshot Jobs (must contain nerdctl)")
	flag.StringVar(&snapshotOptions.ContainerdAddress, "snapshot-containerd-address", "/run/containerd/containerd.sock",
		"host containerd socket path; use /run/k3s/containerd/containerd.sock for RKE2")
	flag.StringVar(&snapshotOptions.ContainerdNamespace, "snapshot-containerd-namespace", "k8s.io",
		"containerd namespace containing the Kubernetes containers")
	flag.BoolVar(&snapshotOptions.ContainerdInsecureRegistry, "snapshot-containerd-insecure-registry", false,
		"allow HTTP and skip TLS certificate verification for registries accessed by nerdctl")

	opts := zap.Options{Development: true, Level: zapcore.InfoLevel}
	opts.BindFlags(flag.CommandLine)
	flag.StringVar(&ingestURL, "ingest-url", "",
		"telemetry ingest URL handed to supervisors (default http://devpod-webui-ingest.<gateway-namespace>.svc:8081/ingest/v1/batch)")
	flag.StringVar(&ingestKeyFile, "ingest-key-file", "", "file with the shared telemetry ingest HMAC key; empty = telemetry off")
	flag.DurationVar(&tel.MetricsInterval, "telemetry-metrics-interval", tel.MetricsInterval, "supervisor metrics sampling interval")
	flag.DurationVar(&tel.ProcScanInterval, "telemetry-proc-scan-interval", tel.ProcScanInterval, "supervisor /proc scan interval")
	flag.IntVar(&tel.TopN, "telemetry-topn", tel.TopN, "processes reported per metric besides the active ones")
	flag.BoolVar(&tel.GPU, "telemetry-gpu", tel.GPU, "collect GPU metrics (nvidia-smi)")
	flag.BoolVar(&tel.RDMA, "telemetry-rdma", tel.RDMA, "collect RDMA counters")
	flag.BoolVar(&tel.Hooks, "telemetry-hooks", tel.Hooks, "record shell commands via profile hooks")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var internalPub []byte
	if internalPubFile != "" {
		b, err := os.ReadFile(internalPubFile)
		if err != nil {
			die(err, "read internal pubkey")
		}
		internalPub = b
	}

	// Scope the informer cache to the namespaces the controller actually
	// writes into. Cluster-scoped types (User, GatewayConfig, the CRDs
	// themselves) are unaffected — they're returned regardless of the
	// DefaultNamespaces map. Without this, the manager would watch every
	// Pod/Secret/PVC in the cluster — both an RBAC over-permission
	// (combined with the ClusterRole) and a real memory inflator on
	// large clusters.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: gatewayNamespace,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				devPodNamespace: {},
			},
		},
	})
	if err != nil {
		die(err, "create manager")
	}

	// TODO(M2): replace this flag-derived GatewayConfig with a Watch on
	// the real GatewayConfig CR. The flags stay as bootstrap defaults
	// for tests / smoke deploys; production should read the cluster CR.
	gw := &devpodv1alpha1.GatewayConfig{
		Spec: devpodv1alpha1.GatewayConfigSpec{
			DevPodNamespace: devPodNamespace,
			SupervisorImage: supervisorImage,
		},
	}
	if homeDirHostPath != "" {
		gw.Spec.HomeDir = &devpodv1alpha1.HomeDirSpec{
			HostPathPrefix: homeDirHostPath,
			MountPrefix:    homeDirMountPrefix,
		}
	}

	if err := (&controllers.DevPodReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		GwConfig:           gw,
		GatewayNamespace:   gatewayNamespace,
		GatewayInternalPub: internalPub,
		Telemetry:          telemetrySettings(ingestKeyFile, ingestURL, gatewayNamespace, tel),
	}).SetupWithManager(mgr); err != nil {
		die(err, "set up DevPodReconciler")
	}
	if err := (&controllers.UserReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		die(err, "set up UserReconciler")
	}
	if err := (&controllers.DevPodSnapshotReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		SnapshotOptions: snapshotOptions,
	}).SetupWithManager(mgr); err != nil {
		die(err, "set up DevPodSnapshotReconciler")
	}

	if err := mgr.AddHealthzCheck("ping", func(_ *http.Request) error { return nil }); err != nil {
		die(err, "add healthz")
	}
	if err := mgr.AddReadyzCheck("ping", func(_ *http.Request) error { return nil }); err != nil {
		die(err, "add readyz")
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		die(err, "run manager")
	}
}

// telemetrySettings loads the ingest key; no key file = telemetry off.
func telemetrySettings(keyFile, url, gatewayNS string, defaults config.Config) *render.TelemetrySettings {
	if keyFile == "" {
		return nil
	}
	key, err := os.ReadFile(keyFile)
	if err != nil || len(bytes.TrimSpace(key)) < 32 {
		die(fmt.Errorf("ingest key %s: must hold >= 32 bytes (%v)", keyFile, err), "telemetry")
	}
	if url == "" {
		url = fmt.Sprintf("http://devpod-webui-ingest.%s.svc:%d/ingest/v1/batch", gatewayNS, render.IngestPort)
	}
	return &render.TelemetrySettings{IngestURL: url, Key: bytes.TrimSpace(key), Defaults: defaults}
}
