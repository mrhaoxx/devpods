// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-controller runs the DevPod and User reconcilers.
package main

import (
	"flag"
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
		snapshotImage        string
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
	flag.StringVar(&snapshotImage, "snapshot-image", "docker:cli",
		"container image for snapshot Jobs (must contain docker CLI)")

	opts := zap.Options{Development: true, Level: zapcore.InfoLevel}
	opts.BindFlags(flag.CommandLine)
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
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
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
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		SnapshotImage: snapshotImage,
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
