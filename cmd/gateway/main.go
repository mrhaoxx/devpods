// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-gateway is the DevPod SSH gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		listenAddr      string
		hostKeyDir      string
		devpodNamespace string
		metricsAddr     string
		ldapSecretDir   string
		httpListenAddr      string
		httpBaseDomain      string
		httpSubdomainSuffix string
	)
	flag.StringVar(&listenAddr, "listen", ":22", "TCP address to listen on")
	flag.StringVar(&hostKeyDir, "host-key-dir", "/etc/devpod/gateway",
		"directory containing ssh_host_ed25519_key (the gateway host key) and a nested internal/ssh_host_ed25519_key (the gateway's outbound signing key)")
	flag.StringVar(&devpodNamespace, "devpod-namespace", "devpods",
		"namespace where DevPod objects live")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"address for the Prometheus /metrics endpoint")
	flag.StringVar(&ldapSecretDir, "ldap-secret-dir", "/etc/devpod/gateway/ldap",
		"directory holding the LDAP CA bundle ('ca.crt') and bind password ('password'). Required when GatewayConfig.spec.ldap is set.")
	flag.StringVar(&httpListenAddr, "http-listen", "",
		"HTTP reverse proxy listen address (e.g. :8090). Disabled when empty.")
	flag.StringVar(&httpBaseDomain, "http-base-domain", "",
		"base domain for HTTP proxy routing, e.g. ktaas.approaching-ai.com")
	flag.StringVar(&httpSubdomainSuffix, "http-subdomain-suffix", "",
		"fixed suffix stripped from subdomain before parsing, e.g. -dev for {name}-{port}-dev.{base}")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	metricsReg := prometheus.NewRegistry()
	gateway.MustRegisterMetrics(metricsReg)
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{}))
		server := &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		slog.Info("metrics_listening", "addr", metricsAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics_server", "err", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	hostSigner, internalSigner, err := loadKeys(hostKeyDir)
	if err != nil {
		slog.Error("load_keys", "err", err)
		os.Exit(1)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		slog.Error("load_kubeconfig", "err", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Error("create_clientset", "err", err)
		os.Exit(1)
	}

	c, err := newCachedClient(ctx, cfg)
	if err != nil {
		slog.Error("cache", "err", err)
		os.Exit(1)
	}

	gw, err := loadGatewayConfig(ctx, c)
	if err != nil {
		slog.Error("load_gatewayconfig", "err", err)
		os.Exit(1)
	}

	proxyKeys := map[string]string{}
	for _, k := range gw.Spec.TrustedProxyKeys {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Pubkey))
		if err != nil {
			slog.Error("parse_trusted_proxy_key", "alias", k.Alias, "err", err)
			os.Exit(1)
		}
		proxyKeys[ssh.FingerprintSHA256(pk)] = k.Alias
	}
	slog.Info("trusted_proxy_keys_loaded", "count", len(proxyKeys))

	srcs, err := buildIdentitySources(ctx, c, gw, ldapSecretDir)
	if err != nil {
		slog.Error("build_identity_sources", "err", err)
		os.Exit(1)
	}
	authn := gateway.NewAuthenticator(c, devpodNamespace).
		WithProxyKeys(proxyKeys).
		WithSources(srcs)
	dialer := gateway.NewDialer(internalSigner)

	bannerFn, err := buildBannerFunc(gw.Spec.Banner)
	if err != nil {
		slog.Error("parse_banner_template", "err", err)
		os.Exit(1)
	}

	if httpListenAddr != "" && httpBaseDomain != "" {
		hp := gateway.NewHTTPProxy(c, devpodNamespace, httpBaseDomain, httpSubdomainSuffix)
		httpServer := &http.Server{
			Addr:              httpListenAddr,
			Handler:           hp,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("http_proxy_listening", "addr", httpListenAddr, "base_domain", httpBaseDomain)
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("http_proxy", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			_ = httpServer.Close()
		}()
	}

	recorder := gateway.NewEventRecorder(c, clientset, devpodNamespace)

	runErr := run(ctx, listenAddr, gw.Spec.Listen, hostSigner, authn, dialer, proxyKeys, bannerFn, recorder)

	// Drain identity sources on graceful shutdown so e.g. the LDAP
	// pooled conn is closed cleanly (server sees a TCP FIN, not the
	// kernel reclaiming the socket on process exit). crdSource has
	// no Close method; the type assertion just skips it.
	for _, src := range srcs {
		if closer, ok := src.(interface{ Close() error }); ok {
			if cerr := closer.Close(); cerr != nil {
				slog.Warn("identity_source_close", "source", src.Name(), "err", cerr)
			}
		}
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		slog.Error("run", "err", runErr)
		os.Exit(1)
	}
}

// bannerData is the field set exposed to the banner template. Every
// field is computable WITHOUT any cluster lookup so we don't trust the
// pre-auth login name beyond syntactic parsing.
type bannerData struct {
	Now      time.Time
	ClientIP string
	Login    string // raw conn.User(), e.g. "alice+smoke"
	User     string // parsed user part, "" on bad format
	Pod      string // parsed pod part, "" on bad format
	Host     string // gateway pod hostname
}

// buildBannerFunc parses the configured template once. Empty template
// → returns nil, meaning no banner is sent. Invalid template fails
// fast at gateway startup.
func buildBannerFunc(tmplSrc string) (func(meta ssh.ConnMetadata) string, error) {
	if strings.TrimSpace(tmplSrc) == "" {
		return nil, nil
	}
	tmpl, err := template.New("banner").Parse(tmplSrc)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	host, _ := os.Hostname()
	return func(meta ssh.ConnMetadata) string {
		login := meta.User()
		user, pod, _ := gateway.ParseLoginName(login)
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, bannerData{
			Now:      time.Now(),
			ClientIP: meta.RemoteAddr().String(),
			Login:    login,
			User:     user,
			Pod:      pod,
			Host:     host,
		}); err != nil {
			// Stay silent on render failure; the banner is best-effort
			// and we don't want a malformed substitution to confuse the
			// client's auth flow.
			return ""
		}
		return buf.String()
	}, nil
}

// loadKeys reads the gateway's host key + internal signing key from disk.
//
// Layout (matches the Helm chart in this plan):
//
//	/etc/devpod/gateway/ssh_host_ed25519_key             — gateway host key
//	/etc/devpod/gateway/internal/ssh_host_ed25519_key    — gateway internal key
//
// The internal-key fallback path /etc/devpod/gateway/internal_key is kept
// for backward-compat with simpler dev setups.
func loadKeys(dir string) (ssh.Signer, ssh.Signer, error) {
	hostBytes, err := os.ReadFile(filepath.Join(dir, "ssh_host_ed25519_key"))
	if err != nil {
		return nil, nil, fmt.Errorf("read host key: %w", err)
	}
	host, err := ssh.ParsePrivateKey(hostBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse host key: %w", err)
	}

	candidates := []string{
		filepath.Join(dir, "internal", "ssh_host_ed25519_key"),
		filepath.Join(dir, "internal_key"),
	}
	var intBytes []byte
	var readErr error
	for _, p := range candidates {
		intBytes, readErr = os.ReadFile(p)
		if readErr == nil {
			break
		}
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("read internal key (looked in %v): %w", candidates, readErr)
	}
	internal, err := ssh.ParsePrivateKey(intBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse internal key: %w", err)
	}
	return host, internal, nil
}

// newCachedClient builds an informer-backed client.Reader over User and
// DevPod kinds, starts the cache, and waits for the initial list to sync.
func newCachedClient(ctx context.Context, cfg *rest.Config) (client.Reader, error) {
	cch, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create cache: %w", err)
	}

	if _, err := cch.GetInformer(ctx, &devpodv1alpha1.User{}); err != nil {
		return nil, fmt.Errorf("informer User: %w", err)
	}
	if _, err := cch.GetInformer(ctx, &devpodv1alpha1.DevPod{}); err != nil {
		return nil, fmt.Errorf("informer DevPod: %w", err)
	}
	if _, err := cch.GetInformer(ctx, &devpodv1alpha1.GatewayConfig{}); err != nil {
		return nil, fmt.Errorf("informer GatewayConfig: %w", err)
	}

	go func() { _ = cch.Start(ctx) }()
	if !cch.WaitForCacheSync(ctx) {
		return nil, errors.New("cache failed to sync")
	}
	return cch, nil
}

// loadGatewayConfig reads the cluster-singleton GatewayConfig/default.
// One-shot: trustedProxyKeys are frozen at startup. To roll the keys,
// the operator restarts the gateway Deployment.
func loadGatewayConfig(ctx context.Context, r client.Reader) (*devpodv1alpha1.GatewayConfig, error) {
	var gc devpodv1alpha1.GatewayConfig
	if err := r.Get(ctx, types.NamespacedName{Name: "default"}, &gc); err != nil {
		return nil, fmt.Errorf("get gatewayconfig/default: %w", err)
	}
	return &gc, nil
}

func run(ctx context.Context, addr string, listen devpodv1alpha1.ListenSpec, hostSigner ssh.Signer, authn *gateway.Authenticator, dialer *gateway.Dialer, proxyKeys map[string]string, bannerFn func(ssh.ConnMetadata) string, recorder *gateway.EventRecorder) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	if listen.ProxyProtocol.Enabled {
		var trusted []*net.IPNet
		for _, c := range listen.ProxyProtocol.TrustedCIDRs {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				return fmt.Errorf("trusted CIDR %q: %w", c, err)
			}
			trusted = append(trusted, n)
		}
		ln = gateway.WrapProxyProtocolListener(ln, trusted, 5*time.Second)
		slog.Info("proxy_protocol_enabled", "trusted_cidrs", listen.ProxyProtocol.TrustedCIDRs)
	}

	slog.Info("listening", "addr", addr)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	g, _ := errgroup.WithContext(ctx)
	var connCount atomic.Uint64
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return g.Wait()
			}
			return fmt.Errorf("accept: %w", err)
		}
		id := connCount.Add(1)
		slog.Info("accept", "id", id, "from", conn.RemoteAddr().String())
		g.Go(func() error {
			handle(ctx, id, conn, hostSigner, authn, dialer, proxyKeys, bannerFn, recorder)
			return nil
		})
	}
}

func handle(parent context.Context, id uint64, conn net.Conn, hostSigner ssh.Signer, authn *gateway.Authenticator, dialer *gateway.Dialer, proxyKeys map[string]string, bannerFn func(ssh.ConnMetadata) string, recorder *gateway.EventRecorder) {
	defer conn.Close()

	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-devpod-gateway",
	}
	if bannerFn != nil {
		cfg.BannerCallback = bannerFn
	}
	cfg.AddHostKey(hostSigner)

	cfg.PublicKeyCallback = func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		res, err := authn.Authenticate(ctx, meta.User(), key)
		if err != nil {
			// Classify error → short reason; figure out auth_path/alias
			// for the audit log + metric label.
			reason := classifyAuthErr(err)
			parsedUser, parsedPod, _ := gateway.ParseLoginName(meta.User())
			fp := ssh.FingerprintSHA256(key)
			ap := "direct"
			alias := ""
			if a, ok := proxyKeys[fp]; ok {
				ap = "trusted_proxy"
				alias = a
			}
			// Pull the partial AuthPath off the error so the deny
			// audit row surfaces LastSourceErr (LDAP outage etc.) —
			// otherwise an LDAP failure shows up as bare
			// "user_not_found" with no upstream clue.
			var authErr *gateway.AuthError
			var lastSourceErr string
			if errors.As(err, &authErr) {
				lastSourceErr = authErr.AuthPath.LastSourceErr
			}
			slog.Warn("auth_rejected", "id", id, "login", meta.User(), "reason", err)
			gateway.AuthFailure(slog.Default(), reason, ap, alias, fp, parsedUser, parsedPod, lastSourceErr)
			gateway.AuthFailuresTotal.WithLabelValues(reason, ap).Inc()
			if parsedPod != "" {
				recorder.AuthRejected(parent, parsedPod, parsedUser, meta.RemoteAddr().String(), reason)
			}
			return nil, err
		}
		slog.Info("auth_ok", "id", id, "user", res.User, "pod", res.DevPodName, "endpoint", res.Endpoint)
		// JSON-encode AuthPath into Extensions so handle() can pick it
		// up after NewServerConn.
		apJSON, _ := json.Marshal(res.AuthPath)
		return &ssh.Permissions{
			Extensions: map[string]string{
				"devpod.io/user":      res.User,
				"devpod.io/devpod":    res.DevPodName,
				"devpod.io/endpoint":  res.Endpoint,
				"devpod.io/auth-path": string(apJSON),
				"devpod.io/pubkey-fp": ssh.FingerprintSHA256(key),
			},
		}, nil
	}

	srvConn, srvChans, srvReqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer srvConn.Close()

	// Decode AuthPath carried via Permissions.Extensions.
	var ap gateway.AuthPath
	if raw := srvConn.Permissions.Extensions["devpod.io/auth-path"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &ap)
	}
	clientIP := conn.RemoteAddr().String()
	fp := srvConn.Permissions.Extensions["devpod.io/pubkey-fp"]
	sessionID := fmt.Sprintf("sid-%d", id)
	gateway.SessionOpen(slog.Default(), sessionID, ap, clientIP, fp)

	start := time.Now()
	stats := &gateway.ProxyStats{}
	sessionResult := "ok"
	closeReason := "client_disconnect"

	defer func() {
		dur := time.Since(start)
		gateway.SessionClose(slog.Default(), sessionID, dur,
			stats.BytesClientToBackend.Load(), stats.BytesBackendToClient.Load(), closeReason)
		gateway.SessionsTotal.WithLabelValues(ap.User, ap.Pod, ap.Kind, sessionResult).Inc()
		gateway.SessionDurationSeconds.WithLabelValues(ap.User, ap.Pod, ap.Kind).Observe(dur.Seconds())
		recorder.SessionDisconnected(parent, ap.Pod, ap.User, closeReason)
	}()

	endpoint := srvConn.Permissions.Extensions["devpod.io/endpoint"]
	dctx, dcancel := context.WithTimeout(parent, gateway.DialTimeout)
	cliConn, cliChans, cliReqs, err := dialer.Dial(dctx, endpoint)
	dcancel()
	if err != nil {
		slog.Warn("dial_failed", "id", id, "endpoint", endpoint, "err", err)
		gateway.DialFailuresTotal.WithLabelValues(ap.Pod, classifyDialErr(err)).Inc()
		recorder.DialFailed(parent, ap.Pod, endpoint, err.Error())
		sessionResult = "error"
		closeReason = "dial_failed"
		return
	}
	defer cliConn.Close()

	recorder.SessionConnected(parent, ap.Pod, ap.User, clientIP, ap.Kind)
	slog.Info("proxy_start", "id", id)
	if err := gateway.Proxy(srvConn, srvChans, srvReqs, cliConn, cliChans, cliReqs, stats); err != nil {
		slog.Info("proxy_end", "id", id, "err", err)
		// "disconnected by user" (SSH_DISCONNECT_BY_APPLICATION, reason 11)
		// is the normal way an OpenSSH client closes; not a proxy error.
		if isCleanDisconnect(err) {
			closeReason = "client_disconnect"
		} else {
			sessionResult = "error"
			closeReason = "proxy_error"
		}
	} else {
		slog.Info("proxy_end", "id", id)
	}
}

// isCleanDisconnect returns true when err looks like a normal SSH
// SSH_MSG_DISCONNECT initiated by the peer (typically the OpenSSH
// client hanging up after the last channel closes).
func isCleanDisconnect(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "disconnected by user") || strings.Contains(msg, "reason 11")
}

// classifyAuthErr maps an Authenticate error to a short reason string
// used as a Prometheus label and audit log field.
func classifyAuthErr(err error) string {
	switch {
	case errors.Is(err, gateway.ErrUserNotFound):
		return "user_not_found"
	case errors.Is(err, gateway.ErrPubkeyMismatch):
		return "key_mismatch"
	case errors.Is(err, gateway.ErrAccessDenied):
		return "access_denied"
	case errors.Is(err, gateway.ErrDevPodNotFound):
		return "devpod_not_found"
	case errors.Is(err, gateway.ErrDevPodNotReady):
		return "devpod_not_ready"
	case errors.Is(err, gateway.ErrLoginNameFormat):
		return "bad_login_format"
	default:
		return "other"
	}
}

// buildIdentitySources composes the ordered identity-source chain
// from the cluster's GatewayConfig.
//
//   - CRD source is always present and queried first.
//   - LDAP source is appended when gw.Spec.LDAP != nil. The bind
//     password and CA bundle live as files under ldapSecretDir; the
//     chart mounts the referenced Secrets there.
func buildIdentitySources(
	ctx context.Context,
	c client.Reader,
	gw *devpodv1alpha1.GatewayConfig,
	ldapSecretDir string,
) ([]gateway.IdentitySource, error) {
	srcs := []gateway.IdentitySource{gateway.NewCRDSource(c)}
	if gw.Spec.LDAP == nil {
		return srcs, nil
	}
	lc := gateway.LDAPConfig{
		URL:              gw.Spec.LDAP.URL,
		CAPath:           filepath.Join(ldapSecretDir, "ca.crt"),
		BindDN:           gw.Spec.LDAP.BindDN,
		BindPasswordPath: filepath.Join(ldapSecretDir, "password"),
		BaseDN:           gw.Spec.LDAP.BaseDN,
		UserFilter:       gw.Spec.LDAP.UserFilter,
		PubkeyAttribute:  gw.Spec.LDAP.PubkeyAttribute,
		RequestTimeout:   time.Duration(gw.Spec.LDAP.RequestTimeoutSeconds) * time.Second,
		CacheTTL:         time.Duration(gw.Spec.LDAP.CacheTTLSeconds) * time.Second,
		NegativeCacheTTL: time.Duration(gw.Spec.LDAP.NegativeCacheTTLSeconds) * time.Second,
		StaleGrace:       time.Duration(gw.Spec.LDAP.StaleGraceSeconds) * time.Second,
	}
	ldapSrc, err := gateway.NewLDAPSource(ctx, lc)
	if err != nil {
		return nil, fmt.Errorf("ldap source: %w", err)
	}
	return append(srcs, ldapSrc), nil
}

// classifyDialErr maps a Dialer error to a short reason string used as
// a Prometheus label.
func classifyDialErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "i/o timeout"):
		return "timeout"
	case strings.Contains(msg, "connection refused"):
		return "refused"
	default:
		return "other"
	}
}
