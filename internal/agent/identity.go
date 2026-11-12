// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Store keeps the agent's private key between starts.
type Store interface {
	Load() ([]byte, error) // nil, nil when there is none yet
	Save([]byte) error
}

type fileStore string

// FileStore keeps the key in a file (mode 0600).
func FileStore(path string) Store { return fileStore(path) }

func (f fileStore) Load() ([]byte, error) {
	b, err := os.ReadFile(string(f))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (f fileStore) Save(b []byte) error {
	if err := os.MkdirAll(filepath.Dir(string(f)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(string(f), b, 0o600)
}

// Identity is the agent's own key: made on the first start, never sent
// anywhere. The console knows the cluster by its public half.
type Identity struct {
	priv ed25519.PrivateKey
	line string
}

// LoadOrCreateIdentity reads the key from store, or makes and saves one.
func LoadOrCreateIdentity(store Store) (*Identity, error) {
	raw, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("agent identity: %w", err)
	}
	var seed []byte
	if raw != nil {
		if seed, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("agent identity: the stored key is not an Ed25519 seed")
		}
	} else {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := store.Save([]byte(base64.StdEncoding.EncodeToString(seed) + "\n")); err != nil {
			return nil, fmt.Errorf("agent identity: %w", err)
		}
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return nil, err
	}
	return &Identity{priv: priv, line: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))}, nil
}

// PublicKey is the identity as an authorized_keys line.
func (i *Identity) PublicKey() string { return i.line }

// Sign proves the identity over a nonce the console sent.
func (i *Identity) Sign(nonce []byte) []byte { return ed25519.Sign(i.priv, nonce) }

// Verify checks a proof against a registered public key (an
// authorized_keys line holding an Ed25519 key).
func Verify(publicKey string, nonce, proof []byte) error {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return fmt.Errorf("registered key: %w", err)
	}
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return errors.New("registered key: not usable")
	}
	ed, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return errors.New("registered key: not an Ed25519 key")
	}
	if !ed25519.Verify(ed, nonce, proof) {
		return errors.New("the proof does not match the registered key")
	}
	return nil
}
