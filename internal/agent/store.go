// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SecretStore keeps a key in a Secret (under "key"), made on the first
// start: a mounted Secret could not be written.
func SecretStore(kc kubernetes.Interface, namespace, name string) Store {
	return secretStore{kc: kc, ns: namespace, name: name}
}

type secretStore struct {
	kc       kubernetes.Interface
	ns, name string
}

func (s secretStore) Load() ([]byte, error) {
	sec, err := s.kc.CoreV1().Secrets(s.ns).Get(context.Background(), s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sec.Data["key"], nil
}

func (s secretStore) Save(b []byte) error {
	_, err := s.kc.CoreV1().Secrets(s.ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.ns},
		Data:       map[string][]byte{"key": b},
	}, metav1.CreateOptions{})
	return err
}
