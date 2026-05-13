// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// DefaultRand is the default randomness source used when callers do not
// inject one. Tests inject a deterministic reader.
var DefaultRand io.Reader = rand.Reader

// HostKeySecret renders the per-DevPod sshd host-key Secret.
//
// authorizedKey is the gateway internal public key (OpenSSH
// authorized_keys line). The sidecar entrypoint installs it as
// /etc/devpod/authorized_keys, which is sshd's sole trusted key —
// SSH clients reach the sidecar exclusively through the gateway,
// never directly.
//
// randSrc is the entropy source for the ed25519 host-key generation;
// pass nil to use DefaultRand.
//
// Note: even with a deterministic randSrc, the rendered private-key
// bytes are not byte-stable across calls; ssh.MarshalPrivateKey draws
// a random checkint from crypto/rand internally.
func HostKeySecret(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig, randSrc io.Reader, authorizedKey []byte) (*corev1.Secret, error) {
	if randSrc == nil {
		randSrc = DefaultRand
	}

	pub, priv, err := ed25519.GenerateKey(randSrc)
	if err != nil {
		return nil, fmt.Errorf("generating ed25519 key: %w", err)
	}

	privPEM, err := ssh.MarshalPrivateKey(priv, "devpod sshd host key")
	if err != nil {
		return nil, fmt.Errorf("marshaling private key: %w", err)
	}
	privBytes := pem.EncodeToMemory(privPEM)

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("ssh.NewPublicKey: %w", err)
	}
	pubBytes := ssh.MarshalAuthorizedKey(sshPub)

	sec := &corev1.Secret{
		ObjectMeta: ObjectMeta(HostKeySecretName(dp), cfg.Spec.DevPodNamespace, dp),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ssh_host_ed25519_key":     privBytes,
			"ssh_host_ed25519_key.pub": pubBytes,
			"authorized_keys":          append([]byte(nil), authorizedKey...),
		},
	}
	return sec, nil
}
