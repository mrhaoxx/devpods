// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-webui serves the DevPod web UI: GitLab-OIDC login,
// template-mediated DevPod self-service, quota enforcement, and the
// embedded SPA.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		listen           string
		issuerURL        string
		clientID         string
		clientSecretFile string
		redirectURL      string
		userPrefix       string
		admins           string
		sessionKeyFile   string
		defaultQuotaFile string
		devpodNamespace  string
		koreMode         string
		tlsCert, tlsKey  string

		pubkeySelfService bool
		sshAdvertise      string
	)
	flag.StringVar(&listen, "listen", ":8080", "HTTP listen address")
	flag.StringVar(&issuerURL, "gitlab-issuer-url", "", "GitLab OIDC issuer URL (required)")
	flag.StringVar(&clientID, "oauth-client-id", "", "OAuth application client id (required)")
	flag.StringVar(&clientSecretFile, "oauth-client-secret-file", "", "file containing the OAuth client secret (required)")
	flag.StringVar(&redirectURL, "redirect-url", "", "external callback URL, e.g. https://devpod.example.com/auth/callback (required)")
	flag.StringVar(&userPrefix, "user-prefix", "", "prefix mapping GitLab usernames to DevPod users")
	flag.StringVar(&admins, "admins", "", "comma-separated GitLab usernames granted admin")
	flag.StringVar(&sessionKeyFile, "session-key-file", "", "file containing the session HMAC key, >= 32 bytes (required)")
	flag.StringVar(&defaultQuotaFile, "default-quota-file", "", "YAML/JSON UserQuota applied to users without spec.quota")
	flag.StringVar(&devpodNamespace, "devpod-namespace", "devpods", "namespace where DevPod objects live")
	flag.StringVar(&koreMode, "kore", "auto", "Kore integration: auto|on|off")
	flag.StringVar(&tlsCert, "tls-cert", "", "optional TLS certificate (default: TLS at the Ingress)")
	flag.StringVar(&tlsKey, "tls-key", "", "optional TLS key")
	flag.BoolVar(&pubkeySelfService, "pubkey-self-service", true,
		"allow users to manage SSH pubkeys via the UI; disable when keys are managed externally (LDAP)")
	flag.StringVar(&sshAdvertise, "ssh-advertise", "",
		"gateway address shown in SSH command lines, host or host:port (e.g. devpod.example.com:2222)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	for name, v := range map[string]string{
		"--gitlab-issuer-url": issuerURL, "--oauth-client-id": clientID,
		"--oauth-client-secret-file": clientSecretFile, "--redirect-url": redirectURL,
		"--session-key-file": sessionKeyFile,
	} {
		if v == "" {
			fatal(fmt.Errorf("%s is required", name))
		}
	}

	sessionKey, err := os.ReadFile(sessionKeyFile)
	if err != nil || len(sessionKey) < 32 {
		fatal(fmt.Errorf("session key: need >= 32 bytes from %s (err=%v)", sessionKeyFile, err))
	}
	clientSecret, err := os.ReadFile(clientSecretFile)
	if err != nil {
		fatal(fmt.Errorf("client secret: %w", err))
	}

	defaultQuota := devpodv1alpha1.UserQuota{}
	if defaultQuotaFile != "" {
		raw, err := os.ReadFile(defaultQuotaFile)
		if err != nil {
			fatal(fmt.Errorf("default quota: %w", err))
		}
		if err := yaml.UnmarshalStrict(raw, &defaultQuota); err != nil {
			fatal(fmt.Errorf("default quota: %w", err))
		}
	}

	restCfg := ctrl.GetConfigOrDie()

	koreEnabled := false
	switch koreMode {
	case "on":
		koreEnabled = true
	case "off":
	case "auto":
		dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
		if err == nil {
			if _, err := dc.ServerResourcesForGroupVersion("kore.zjusct.io/v1alpha1"); err == nil {
				koreEnabled = true
			}
		}
	default:
		fatal(fmt.Errorf("--kore must be auto|on|off, got %q", koreMode))
	}
	slog.Info("kore integration", "enabled", koreEnabled, "mode", koreMode)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&devpodv1alpha1.DevPod{}: {Namespaces: map[string]cache.Config{devpodNamespace: {}}},
				&corev1.Pod{}:            {Namespaces: map[string]cache.Config{devpodNamespace: {}}},
				&corev1.Event{}:          {Namespaces: map[string]cache.Config{devpodNamespace: {}}},
			},
		},
	})
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sshHost, sshPort := sshAdvertise, 22
	if h, p, err := net.SplitHostPort(sshAdvertise); err == nil {
		sshHost = h
		if n, perr := strconv.Atoi(p); perr == nil {
			sshPort = n
		}
	}

	sm := webui.NewSessionManager(sessionKey, 24*time.Hour)
	srv := &webui.Server{
		Client:       mgr.GetClient(),
		Reader:       mgr.GetAPIReader(),
		Cache:        mgr.GetCache(),
		NS:           devpodNamespace,
		Sessions:     sm,
		DefaultQuota: defaultQuota,
		KoreEnabled:  koreEnabled,
		Origin:       originOf(redirectURL),

		PubkeySelfService: pubkeySelfService,
		SSHHost:           sshHost,
		SSHPort:           sshPort,
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fatal(err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		fatal(fmt.Errorf("cache sync failed"))
	}

	oauth, err := webui.NewOAuth(ctx, webui.OAuthConfig{
		IssuerURL:    issuerURL,
		ClientID:     clientID,
		ClientSecret: strings.TrimSpace(string(clientSecret)),
		RedirectURL:  redirectURL,
		UserPrefix:   userPrefix,
		Admins:       splitNonEmpty(admins),
	}, mgr.GetClient(), sm)
	if err != nil {
		fatal(err)
	}
	srv.OAuth = oauth

	httpSrv := &http.Server{Addr: listen, Handler: srv.Routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("devpod-webui listening", "addr", listen)
	if tlsCert != "" {
		err = httpSrv.ListenAndServeTLS(tlsCert, tlsKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

// originOf reduces the redirect URL to its scheme://host origin for
// the CSRF Origin check.
func originOf(redirectURL string) string {
	if i := strings.Index(redirectURL, "://"); i > 0 {
		rest := redirectURL[i+3:]
		if j := strings.Index(rest, "/"); j > 0 {
			return redirectURL[:i+3] + rest[:j]
		}
	}
	return redirectURL
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(err error) {
	slog.Error("fatal", "err", err)
	os.Exit(1)
}
