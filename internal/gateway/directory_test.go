// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"errors"
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

func TestParseLogin(t *testing.T) {
	for _, tc := range []struct{ in, user, pod, cluster string }{
		{"alice+dev", "alice", "dev", ""},
		{"alice+dev+mulan", "alice", "dev", "mulan"},
		{"st43+day-2+st-2", "st43", "day-2", "st-2"},
	} {
		user, pod, cluster, err := gateway.ParseLogin(tc.in)
		if err != nil || user != tc.user || pod != tc.pod || cluster != tc.cluster {
			t.Errorf("%q → %q %q %q %v", tc.in, user, pod, cluster, err)
		}
	}
	for _, bad := range []string{"alice", "alice+", "+dev", "alice+dev+", "alice+dev+a+b", "Alice+dev", "alice+dev+MULAN", "alice+dev +mulan"} {
		if _, _, _, err := gateway.ParseLogin(bad); !errors.Is(err, gateway.ErrInvalidLoginName) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	// the two-part parser still answers for a three-part login
	if user, pod, err := gateway.ParseLoginName("alice+dev+mulan"); err != nil || user != "alice" || pod != "dev" {
		t.Errorf("ParseLoginName with a cluster: %q %q %v", user, pod, err)
	}
}

// several is a directory of several clusters.
type several map[string][]*devpodv1alpha1.DevPod // cluster → DevPods

func (s several) Lookup(_ context.Context, user, pod, cluster string) ([]gateway.Found, error) {
	var out []gateway.Found
	for _, c := range []string{"a", "b", "c"} {
		if cluster != "" && cluster != c {
			continue
		}
		for _, dp := range s[c] {
			if dp.Name == gateway.DevPodResourceName(user, pod) {
				out = append(out, gateway.Found{Cluster: c, DevPod: dp})
			}
		}
	}
	return out, nil
}

func (s several) ByHTTPID(context.Context, string) (*gateway.Found, error) { return nil, nil }

func in(endpoint string, dp *devpodv1alpha1.DevPod) *devpodv1alpha1.DevPod {
	dp = dp.DeepCopy()
	dp.Status.Endpoint = endpoint
	return dp
}

func TestAuthenticate_SeveralClusters(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	other, _ := ed25519Pubkey(t)
	users := fakeClient(t,
		&devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}, Spec: devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}}},
		&devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "bob"}, Spec: devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}}},
	)
	dir := several{
		"a": {in("10.0.0.1:22", runningDevPod("alice-x", "alice")), in("10.0.0.3:22", runningDevPod("alice-only", "alice"))},
		"b": {in("10.1.0.1:2022", runningDevPod("alice-x", "alice")), in("10.1.0.9:22", runningDevPod("alice-shared", "alice", "bob"))},
		"c": {in("10.2.0.1:22", runningDevPod("alice-shared", "alice"))},
	}
	a := gateway.NewAuthenticator(users, "devpods").WithDirectory(dir)
	ctx := context.Background()

	// one cluster has the name: no cluster needed, and the answer says where
	res, err := a.Authenticate(ctx, "alice+only", pk)
	if err != nil || res.Cluster != "a" || res.Endpoint != "10.0.0.3:22" || len(res.Ambiguous) != 0 {
		t.Fatalf("a name one cluster has: %+v %v", res, err)
	}
	// the cluster can always be written
	res, err = a.Authenticate(ctx, "alice+x+b", pk)
	if err != nil || res.Cluster != "b" || res.Endpoint != "10.1.0.1:2022" {
		t.Fatalf("with the cluster: %+v %v", res, err)
	}
	if _, err := a.Authenticate(ctx, "alice+only+b", pk); !errors.Is(err, gateway.ErrDevPodNotFound) {
		t.Fatalf("a cluster that does not have it: %v", err)
	}
	// two clusters have it: the key is checked first, then the ways to write it are told
	res, err = a.Authenticate(ctx, "alice+x", pk)
	if err != nil || len(res.Ambiguous) != 2 || res.Ambiguous[0] != "alice+x+a" || res.Ambiguous[1] != "alice+x+b" || res.Endpoint != "" {
		t.Fatalf("a name two clusters have: %+v %v", res, err)
	}
	// someone without the key learns nothing of it
	if res, err := a.Authenticate(ctx, "alice+x", other); !errors.Is(err, gateway.ErrPubkeyMismatch) || res != nil {
		t.Fatalf("a name two clusters have, wrong key: %+v %v", res, err)
	}
	// what counts is what this user may log in to: bob is a collaborator in b only
	res, err = a.Authenticate(ctx, "bob+shared", pk)
	if !errors.Is(err, gateway.ErrDevPodNotFound) || res != nil {
		t.Fatalf("bob+shared looks for bob's own: %+v %v", res, err)
	}
}

func TestDialer_DialInGoesThroughTheClusterDialer(t *testing.T) {
	var gotCluster, gotAddr, gotPurpose string
	d := gateway.NewDialer(nil)
	d.Net = func(_ context.Context, cluster, addr, purpose string) (net.Conn, error) {
		gotCluster, gotAddr, gotPurpose = cluster, addr, purpose
		return nil, errors.New("no way there")
	}
	_, _, _, err := d.DialIn(context.Background(), "mulan", "10.0.0.1:22")
	if err == nil || gotCluster != "mulan" || gotAddr != "10.0.0.1:22" || gotPurpose != gateway.PurposeSSH {
		t.Fatalf("DialIn: %v %q %q %q", err, gotCluster, gotAddr, gotPurpose)
	}
}
