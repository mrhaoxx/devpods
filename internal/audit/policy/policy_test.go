// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func act(a devpodv1alpha1.PolicyAction) *devpodv1alpha1.PolicyAction { return &a }

func TestResolve_NilNilAllowsEverything(t *testing.T) {
	if got := Resolve(nil, nil); got != (Effective{}) {
		t.Fatalf("got %+v", got)
	}
	if got := Resolve(&devpodv1alpha1.AuditPolicy{}, &devpodv1alpha1.AuditPolicy{}); got != (Effective{}) {
		t.Fatalf("empty policies: got %+v", got)
	}
}

func TestResolve_UserOverridesField(t *testing.T) {
	def := &devpodv1alpha1.AuditPolicy{Download: act(devpodv1alpha1.PolicyDeny), Upload: act(devpodv1alpha1.PolicyDeny)}
	user := &devpodv1alpha1.AuditPolicy{Download: act(devpodv1alpha1.PolicyAllow)}
	got := Resolve(def, user)
	if got.DenyDownload {
		t.Error("user Allow must override default Deny")
	}
	if !got.DenyUpload {
		t.Error("default Upload Deny must survive when user leaves it unset")
	}
}

func TestResolve_ForwardingSubfieldsMergeIndependently(t *testing.T) {
	rec := true
	def := &devpodv1alpha1.AuditPolicy{Forwarding: &devpodv1alpha1.ForwardingPolicy{Local: act(devpodv1alpha1.PolicyDeny), RecordContent: &rec}}
	user := &devpodv1alpha1.AuditPolicy{Forwarding: &devpodv1alpha1.ForwardingPolicy{Remote: act(devpodv1alpha1.PolicyDeny)}}
	got := Resolve(def, user)
	if !got.DenyLocalForward || !got.DenyRemoteForward || !got.RecordForwardContent {
		t.Fatalf("got %+v", got)
	}
}

func TestResolve_ScalarsAndFailMode(t *testing.T) {
	q := resource.MustParse("100Mi")
	closed := devpodv1alpha1.FailClosed
	def := &devpodv1alpha1.AuditPolicy{MaxFileSize: &q, FailMode: &closed,
		AgentForwarding: act(devpodv1alpha1.PolicyDeny), X11Forwarding: act(devpodv1alpha1.PolicyDeny)}
	got := Resolve(def, nil)
	want := Effective{MaxFileSize: 104857600, FailClosed: true, DenyAgentForward: true, DenyX11: true}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	open := devpodv1alpha1.FailOpen
	if Resolve(def, &devpodv1alpha1.AuditPolicy{FailMode: &open}).FailClosed {
		t.Fatal("user Open must override default Closed")
	}
}

func TestHasDeny(t *testing.T) {
	if (Effective{DenyAgentForward: true}).HasDeny() {
		t.Error("forwarding rules are not content denies")
	}
	for _, e := range []Effective{{DenyDownload: true}, {DenyUpload: true}, {MaxFileSize: 1}} {
		if !e.HasDeny() {
			t.Errorf("%+v should have a deny", e)
		}
	}
}

func TestWarnings_VSCodeBypass(t *testing.T) {
	if got := (Effective{DenyDownload: true}).Warnings(); !reflect.DeepEqual(got, []string{WarnVSCodeBypass}) {
		t.Fatalf("got %v", got)
	}
	if got := (Effective{DenyDownload: true, DenyLocalForward: true}).Warnings(); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := (Effective{}).Warnings(); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}
