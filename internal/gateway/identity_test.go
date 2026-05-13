// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

func TestCRDSource_NotFound_ReturnsIdentityNotFound(t *testing.T) {
	src := gateway.NewCRDSource(fakeClient(t))
	_, err := src.Resolve(context.Background(), "ghost")
	if !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want ErrIdentityNotFound", err)
	}
}

func TestCRDSource_FoundReturnsParsedKeys(t *testing.T) {
	_, lineA := ed25519Pubkey(t)
	_, lineB := ed25519Pubkey(t)
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{lineA, lineB}},
	}
	src := gateway.NewCRDSource(fakeClient(t, u))
	res, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Keys) != 2 {
		t.Fatalf("len Keys = %d, want 2", len(res.Keys))
	}
	if res.ServedStale {
		t.Errorf("ServedStale = true, want false for crdSource")
	}
}

func TestCRDSource_SkipsUnparseableLines(t *testing.T) {
	_, line := ed25519Pubkey(t)
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec: devpodv1alpha1.UserSpec{
			Pubkeys: []string{line, "not a key", "also-garbage"},
		},
	}
	src := gateway.NewCRDSource(fakeClient(t, u))
	res, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Keys) != 1 {
		t.Fatalf("len Keys = %d, want 1 (garbage lines dropped)", len(res.Keys))
	}
}

func TestCRDSource_PassesThroughRealError(t *testing.T) {
	src := gateway.NewCRDSource(boomReader{})
	_, err := src.Resolve(context.Background(), "alice")
	if err == nil || errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want non-NotFound transport error", err)
	}
}

func TestCRDSource_Name(t *testing.T) {
	src := gateway.NewCRDSource(fakeClient(t))
	if got := src.Name(); got != "crd" {
		t.Errorf("Name() = %q, want %q", got, "crd")
	}
}

// boomReader is a client.Reader that returns a non-NotFound error on
// every call, simulating an apiserver transport problem.
type boomReader struct{}

func (boomReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return errReaderBoom
}
func (boomReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return errReaderBoom
}

var errReaderBoom = errBoom{}

type errBoom struct{}

func (errBoom) Error() string { return "transport boom" }
