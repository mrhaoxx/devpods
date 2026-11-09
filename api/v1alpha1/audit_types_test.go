// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestAuditPolicy_EmptyMarshalsToEmptyObject(t *testing.T) {
	b, err := json.Marshal(AuditPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Fatalf("got %s, want {}", b)
	}
}

func TestAuditPolicy_RoundTrip(t *testing.T) {
	deny := PolicyDeny
	allow := PolicyAllow
	closed := FailClosed
	q := resource.MustParse("100Mi")
	rec := true
	in := AuditPolicy{
		Download:        &deny,
		Upload:          &allow,
		MaxFileSize:     &q,
		AgentForwarding: &deny,
		FailMode:        &closed,
		Forwarding:      &ForwardingPolicy{Local: &deny, RecordContent: &rec},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out AuditPolicy
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if *out.Download != PolicyDeny || *out.Upload != PolicyAllow || *out.AgentForwarding != PolicyDeny {
		t.Fatalf("actions not preserved: %s", b)
	}
	if out.X11Forwarding != nil || out.Forwarding.Remote != nil {
		t.Fatalf("unset fields must stay nil: %s", b)
	}
	if out.MaxFileSize.Cmp(q) != 0 {
		t.Fatalf("maxFileSize = %s", out.MaxFileSize.String())
	}
	if *out.FailMode != FailClosed || !*out.Forwarding.RecordContent || *out.Forwarding.Local != PolicyDeny {
		t.Fatalf("fields not preserved: %s", b)
	}
}

func TestCRD_AuditEnumsPresent(t *testing.T) {
	cases := map[string]string{
		"../../config/crd/bases/devpod.io_users.yaml":          "auditPolicy:",
		"../../config/crd/bases/devpod.io_gatewayconfigs.yaml": "audit:",
	}
	for path, field := range cases {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, want := range []string{field, "- Allow", "- Deny", "- Open", "- Closed"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: missing %q", path, want)
			}
		}
	}
}
