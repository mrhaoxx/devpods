// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/render"
)

// fixedReader returns deterministic bytes, useful for ed25519 keygen in tests.
type fixedReader struct{ b byte }

func (r *fixedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

func TestHostKeySecret_ProducesEd25519PEMAndPub(t *testing.T) {
	dp := minimalDevPod()
	r := &fixedReader{b: 0x42}

	sec, err := render.HostKeySecret(dp, cfg(), r, []byte("ssh-ed25519 AAAA test\n"))
	if err != nil {
		t.Fatalf("HostKeySecret: %v", err)
	}

	if got, want := sec.Name, "alice-frontend-dev-hostkey"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	priv, ok := sec.Data["ssh_host_ed25519_key"]
	if !ok || len(priv) == 0 {
		t.Fatalf("missing ssh_host_ed25519_key")
	}
	if !bytes.Contains(priv, []byte("OPENSSH PRIVATE KEY")) {
		t.Errorf("private key not in OpenSSH PEM form: %s", priv[:30])
	}
	pub, ok := sec.Data["ssh_host_ed25519_key.pub"]
	if !ok || len(pub) == 0 {
		t.Fatalf("missing ssh_host_ed25519_key.pub")
	}
	if !strings.HasPrefix(string(pub), "ssh-ed25519 ") {
		t.Errorf("public key not in authorized_keys form: %q", string(pub[:20]))
	}

	// Round-trip the public key to verify it parses.
	if _, _, _, _, err := ssh.ParseAuthorizedKey(pub); err != nil {
		t.Errorf("public key does not parse: %v", err)
	}
}

func TestHostKeySecret_EmbedsAuthorizedKey(t *testing.T) {
	dp := minimalDevPod()
	authKey := []byte("ssh-ed25519 AAAA gateway-internal\n")

	sec, err := render.HostKeySecret(dp, cfg(), &fixedReader{b: 0x42}, authKey)
	if err != nil {
		t.Fatalf("HostKeySecret: %v", err)
	}
	if got := sec.Data["authorized_keys"]; !bytes.Equal(got, authKey) {
		t.Errorf("authorized_keys data mismatch: got %q, want %q", got, authKey)
	}
}

// Sanity check that ed25519 is the underlying algorithm.
func TestHostKeySecret_KeyTypeMarker(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(&fixedReader{b: 0x42})
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	sshpub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	if sshpub.Type() != "ssh-ed25519" {
		t.Errorf("type = %q, want ssh-ed25519", sshpub.Type())
	}
}
