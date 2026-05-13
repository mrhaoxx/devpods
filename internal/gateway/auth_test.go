// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

// fakeClient builds a controller-runtime fake client preloaded with the
// passed objects and the DevPod scheme.
func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// ed25519Pubkey returns a fresh public key + its authorized_keys-line form.
func ed25519Pubkey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey(), string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func runningDevPod(name, owner string, collaborators ...string) *devpodv1alpha1.DevPod {
	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:         owner,
			Collaborators: collaborators,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
		Status: devpodv1alpha1.DevPodStatus{
			Phase:    devpodv1alpha1.DevPodRunning,
			Endpoint: "10.0.0.1:22",
		},
	}
}

func TestAuthenticate_OwnerSucceeds(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		runningDevPod("hello", "alice"),
	)

	a := gateway.NewAuthenticator(c, "devpods")
	res, err := a.Authenticate(context.Background(), "alice+hello", pk)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.User != "alice" || res.DevPodName != "hello" || res.Endpoint != "10.0.0.1:22" {
		t.Errorf("got %+v", res)
	}
}

func TestAuthenticate_CollaboratorSucceeds(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "bob"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		runningDevPod("hello", "alice", "bob"),
	)

	res, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "bob+hello", pk)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.User != "bob" || res.DevPodName != "hello" {
		t.Errorf("got %+v", res)
	}
}

func TestAuthenticate_NotOwnerNotCollab_Denied(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "carol"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		runningDevPod("hello", "alice"),
	)

	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "carol+hello", pk)
	if !errors.Is(err, gateway.ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
}

func TestAuthenticate_UnknownUser_Denied(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	// DevPod must exist so the identity-source path is reached; the
	// post-Task-3 Authenticator fetches DevPod up front so a missing
	// DevPod would short-circuit to ErrDevPodNotFound.
	c := fakeClient(t, runningDevPod("hello", "alice"))
	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "nobody+hello", pk)
	if !errors.Is(err, gateway.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestAuthenticate_PubkeyMismatch_Denied(t *testing.T) {
	_, line := ed25519Pubkey(t)
	other, _ := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{line}},
		},
		runningDevPod("hello", "alice"),
	)
	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "alice+hello", other)
	if !errors.Is(err, gateway.ErrPubkeyMismatch) {
		t.Fatalf("err = %v, want ErrPubkeyMismatch", err)
	}
}

func TestAuthenticate_DevPodNotRunning_Denied(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	dp := runningDevPod("hello", "alice")
	dp.Status.Phase = devpodv1alpha1.DevPodPending
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		dp,
	)
	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "alice+hello", pk)
	if !errors.Is(err, gateway.ErrDevPodNotReady) {
		t.Fatalf("err = %v, want ErrDevPodNotReady", err)
	}
}

func TestAuthenticate_BadLoginName_Format(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	c := fakeClient(t)
	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "no-plus-here", pk)
	if !errors.Is(err, gateway.ErrLoginNameFormat) {
		t.Fatalf("err = %v, want ErrLoginNameFormat", err)
	}
}

func TestAuthenticate_TrustedProxy_Succeeds(t *testing.T) {
	// alice has key A; the proxy presents key B which is in the trusted-proxy index.
	pkUser, pkUserLine := ed25519Pubkey(t)
	pkProxy, _ := ed25519Pubkey(t)
	_ = pkUser // user's own key, present in spec.pubkeys; the test uses pkProxy below
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkUserLine}},
		},
		runningDevPod("hello", "alice"),
	)
	proxyIdx := map[string]string{ssh.FingerprintSHA256(pkProxy): "corp"}
	a := gateway.NewAuthenticator(c, "devpods").WithProxyKeys(proxyIdx)
	res, err := a.Authenticate(context.Background(), "alice+hello", pkProxy)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.AuthPath.Kind != "trusted_proxy" {
		t.Errorf("AuthPath.Kind = %q, want trusted_proxy", res.AuthPath.Kind)
	}
	if res.AuthPath.Alias != "corp" {
		t.Errorf("AuthPath.Alias = %q, want corp", res.AuthPath.Alias)
	}
	if res.AuthPath.User != "alice" || res.AuthPath.Pod != "hello" {
		t.Errorf("AuthPath.User/Pod wrong: %+v", res.AuthPath)
	}
}

func TestAuthenticate_TrustedProxy_DevPodNotFoundRejected(t *testing.T) {
	// Trusted-proxy auth bypasses identity-source resolution (the
	// proxy attests to the user — required so LDAP-only users with
	// no User CRD remain reachable via proxy), but the DevPod must
	// still exist. This test pins that the up-front DevPod Get
	// happens *before* the proxy short-circuit, so a bogus user+pod
	// pair is still rejected.
	pkProxy, _ := ed25519Pubkey(t)
	c := fakeClient(t) // no DevPods at all
	proxyIdx := map[string]string{ssh.FingerprintSHA256(pkProxy): "corp"}
	a := gateway.NewAuthenticator(c, "devpods").WithProxyKeys(proxyIdx)
	_, err := a.Authenticate(context.Background(), "ghost+hello", pkProxy)
	if !errors.Is(err, gateway.ErrDevPodNotFound) {
		t.Errorf("err = %v, want ErrDevPodNotFound", err)
	}
}

func TestAuthenticate_TrustedProxy_OwnerCheckStillApplies(t *testing.T) {
	// Proxy claims alice+bob-pod where bob owns bob-pod. Expect access denied.
	pkProxy, _ := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}, Spec: devpodv1alpha1.UserSpec{Pubkeys: []string{}}},
		&devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "bob"}, Spec: devpodv1alpha1.UserSpec{Pubkeys: []string{}}},
		runningDevPod("bob-pod", "bob"),
	)
	proxyIdx := map[string]string{ssh.FingerprintSHA256(pkProxy): "corp"}
	a := gateway.NewAuthenticator(c, "devpods").WithProxyKeys(proxyIdx)
	_, err := a.Authenticate(context.Background(), "alice+bob-pod", pkProxy)
	if !errors.Is(err, gateway.ErrAccessDenied) {
		t.Errorf("err = %v, want ErrAccessDenied", err)
	}
}

func TestAuthenticate_DirectAuth_PathFieldPopulated(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		runningDevPod("hello", "alice"),
	)
	res, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "alice+hello", pk)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.AuthPath.Kind != "direct" {
		t.Errorf("AuthPath.Kind = %q, want direct", res.AuthPath.Kind)
	}
	if res.AuthPath.User != "alice" || res.AuthPath.Pod != "hello" {
		t.Errorf("AuthPath user/pod wrong: %+v", res.AuthPath)
	}
	if res.AuthPath.Source != "crd" {
		t.Errorf("AuthPath.Source = %q, want %q (default source path)", res.AuthPath.Source, "crd")
	}
}

// stubSource is a deterministic IdentitySource for decision-table
// tests. Set keys to ssh keys for a "known" user; absence in `known`
// means ErrIdentityNotFound; set boom to simulate a hard transport
// error; set stale=true to advertise ServedStale on Resolve hits.
type stubSource struct {
	name    string
	known   map[string][]ssh.PublicKey
	boom    error
	stale   bool
	callLog []string
}

func (s *stubSource) Name() string { return s.name }
func (s *stubSource) Resolve(_ context.Context, u string) (gateway.ResolveResult, error) {
	s.callLog = append(s.callLog, u)
	if s.boom != nil {
		return gateway.ResolveResult{}, s.boom
	}
	k, ok := s.known[u]
	if !ok {
		return gateway.ResolveResult{}, gateway.ErrIdentityNotFound
	}
	return gateway.ResolveResult{Keys: k, ServedStale: s.stale}, nil
}

func TestAuth_DecisionTable_CRDMatch(t *testing.T) {
	pk, line := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{line}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), "devpods").
		WithSources([]gateway.IdentitySource{gateway.NewCRDSource(fakeClient(t, u))})
	got, err := a.Authenticate(context.Background(), "alice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "crd" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "crd")
	}
}

func TestAuth_DecisionTable_CRDMissLDAPMatch_Fallback(t *testing.T) {
	_, lineCRD := ed25519Pubkey(t) // CRD-stored, NOT offered
	pkLDAP, _ := ed25519Pubkey(t)  // offered by client, only in LDAP
	dp := runningDevPod("smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{lineCRD}},
	}
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"alice": {pkLDAP}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), "devpods").
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t, u)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "alice+smoke", pkLDAP)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "ldap" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "ldap")
	}
}

func TestAuth_DecisionTable_LDAPOnly(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "lalice")
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"lalice": {pk}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp), "devpods").
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "lalice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "ldap" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "ldap")
	}
}

func TestAuth_DecisionTable_NoSourceKnowsUser(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "ghost")
	a := gateway.NewAuthenticator(fakeClient(t, dp), "devpods").
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t)),
			&stubSource{name: "ldap", known: map[string][]ssh.PublicKey{}},
		})
	_, err := a.Authenticate(context.Background(), "ghost+smoke", pk)
	if !errors.Is(err, gateway.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestAuth_DecisionTable_KnownButKeyMismatch(t *testing.T) {
	_, lineCRD := ed25519Pubkey(t)
	other, _ := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{lineCRD}},
	}
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"alice": {}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), "devpods").
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t, u)),
			ldap,
		})
	_, err := a.Authenticate(context.Background(), "alice+smoke", other)
	if !errors.Is(err, gateway.ErrPubkeyMismatch) {
		t.Fatalf("err = %v, want ErrPubkeyMismatch", err)
	}
}

func TestAuth_DecisionTable_LDAPErrCRDMatch(t *testing.T) {
	pk, line := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{line}},
	}
	ldap := &stubSource{
		name: "ldap",
		boom: errors.New("ldap unreachable"),
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), "devpods").
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t, u)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "alice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "crd" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "crd")
	}
}

func TestAuth_DecisionTable_TrustedProxyShortCircuits(t *testing.T) {
	pkProxy, _ := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "alice")
	idx := map[string]string{ssh.FingerprintSHA256(pkProxy): "fw1"}
	calls := &stubSource{name: "stub"}
	a := gateway.NewAuthenticator(fakeClient(t, dp), "devpods").
		WithSources([]gateway.IdentitySource{calls}).
		WithProxyKeys(idx)
	got, err := a.Authenticate(context.Background(), "alice+smoke", pkProxy)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Kind != "trusted_proxy" {
		t.Errorf("kind = %q, want trusted_proxy", got.AuthPath.Kind)
	}
	if len(calls.callLog) != 0 {
		t.Errorf("source consulted on proxy path: %v", calls.callLog)
	}
}

func TestAuth_AuthError_CarriesLastSourceErrOnDeny(t *testing.T) {
	// CRD knows alice (so anyKnown=true) but the offered key
	// mismatches, AND the LDAP source hard-errs — so the final
	// verdict is ErrPubkeyMismatch with AuthPath.LastSourceErr
	// populated from the LDAP failure. The caller must be able to
	// errors.As-recover the AuthError to feed LastSourceErr into the
	// audit row.
	_, lineCRD := ed25519Pubkey(t) // not offered
	offered, _ := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{lineCRD}},
	}
	ldap := &stubSource{name: "ldap", boom: errors.New("dial: connection refused")}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), "devpods").
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t, u)),
			ldap,
		})
	_, err := a.Authenticate(context.Background(), "alice+smoke", offered)
	if !errors.Is(err, gateway.ErrPubkeyMismatch) {
		t.Fatalf("err = %v, want ErrPubkeyMismatch", err)
	}
	var authErr *gateway.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err is not *AuthError: %T", err)
	}
	if got := authErr.AuthPath.LastSourceErr; !strings.HasPrefix(got, "ldap:") {
		t.Errorf("LastSourceErr = %q, want prefix %q", got, "ldap:")
	}
	if authErr.AuthPath.User != "alice" || authErr.AuthPath.Pod != "smoke" {
		t.Errorf("AuthPath user/pod wrong: %+v", authErr.AuthPath)
	}
}

func TestAuth_AuthError_EarlyFailHasEmptyLastSourceErr(t *testing.T) {
	// Login-name parse failure happens BEFORE any source runs; the
	// AuthError must still be returned but AuthPath.LastSourceErr
	// stays empty.
	pk, _ := ed25519Pubkey(t)
	_, err := gateway.NewAuthenticator(fakeClient(t), "devpods").
		Authenticate(context.Background(), "no-plus-here", pk)
	var authErr *gateway.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err is not *AuthError: %T", err)
	}
	if authErr.AuthPath.LastSourceErr != "" {
		t.Errorf("LastSourceErr = %q, want empty for pre-source failure", authErr.AuthPath.LastSourceErr)
	}
}

func TestAuth_DecisionTable_StaleLDAPSurfacesServedStale(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	dp := runningDevPod("smoke", "lalice")
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"lalice": {pk}},
		stale: true,
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp), "devpods").
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "lalice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if !got.AuthPath.ServedStale {
		t.Errorf("ServedStale = false, want true")
	}
}
