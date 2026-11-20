// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/httpauth"
)

// Found is a DevPod and the cluster it is in ("" where the gateway
// serves the one cluster it runs in).
type Found struct {
	Cluster string
	DevPod  *devpodv1alpha1.DevPod
	// Node: the machine its Pod is on; "" = none or not known
	Node string
}

// Directory answers which DevPod a login or a port-link address means.
// A gateway in a cluster reads that cluster (NewKubeDirectory); a gateway
// of several clusters asks the console.
type Directory interface {
	// Lookup returns the DevPods "<user>+<pod>" could mean — "<user>-<pod>"
	// first, the bare "<pod>" for DevPods older than owner-scoped names —
	// in cluster, or in every cluster when it is empty.
	Lookup(ctx context.Context, user, pod, cluster string) ([]Found, error)
	// ByHTTPID returns the DevPod whose port links carry id; nil if none.
	ByHTTPID(ctx context.Context, id string) (*Found, error)
}

// NewKubeDirectory reads the DevPods of the namespace through r.
func NewKubeDirectory(r client.Reader, namespace string) Directory {
	return &kubeDirectory{c: r, ns: namespace, now: time.Now}
}

type kubeDirectory struct {
	c   client.Reader
	ns  string
	now func() time.Time
	// pods reads a DevPod's Pod for the machine it is on; nil = not read.
	// Not the DevPod cache: a Pod informer there needs RBAC over every
	// Pod and would hold a login until it synced.
	pods client.Reader

	mu    sync.Mutex
	ids   map[string]string // port-link id → DevPod name
	idsAt time.Time
}

func (d *kubeDirectory) Lookup(ctx context.Context, user, pod, cluster string) ([]Found, error) {
	if cluster != "" {
		return nil, nil // one cluster, without a name
	}
	var dp devpodv1alpha1.DevPod
	err := d.c.Get(ctx, types.NamespacedName{Name: DevPodResourceName(user, pod), Namespace: d.ns}, &dp)
	if apierrors.IsNotFound(err) {
		err = d.c.Get(ctx, types.NamespacedName{Name: pod, Namespace: d.ns}, &dp)
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []Found{{DevPod: &dp, Node: d.nodeOf(ctx, &dp)}}, nil
}

// nodeOf: the machine dp's Pod is on; "" when there is no pods reader,
// no Pod, or it cannot be read soon — what a session records is best
// effort, a login never waits on it.
func (d *kubeDirectory) nodeOf(ctx context.Context, dp *devpodv1alpha1.DevPod) string {
	if d.pods == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var pod corev1.Pod
	if err := d.pods.Get(ctx, types.NamespacedName{Name: dp.Name, Namespace: d.ns}, &pod); err != nil {
		return ""
	}
	return pod.Spec.NodeName
}

// NewKubeDirectoryWithPods is NewKubeDirectory that also says the machine
// a DevPod's Pod is on, read through pods (an uncached reader: the gateway
// may read Pods of the DevPod namespace, no more).
func NewKubeDirectoryWithPods(r, pods client.Reader, namespace string) Directory {
	return &kubeDirectory{c: r, ns: namespace, now: time.Now, pods: pods}
}

func (d *kubeDirectory) ByHTTPID(ctx context.Context, id string) (*Found, error) {
	if !httpauth.ValidID(id) {
		return nil, nil
	}
	d.mu.Lock()
	name, ok := d.ids[id]
	// an id drawn a moment ago is not in the index yet: a miss rebuilds
	// it (at most once per idsMissRefresh, so unknown ids stay cheap)
	if age := d.now().Sub(d.idsAt); d.ids == nil || age > idsTTL || (!ok && age > idsMissRefresh) {
		var list devpodv1alpha1.DevPodList
		if err := d.c.List(ctx, &list, client.InNamespace(d.ns)); err == nil {
			ids := make(map[string]string, len(list.Items))
			for i := range list.Items {
				if v := list.Items[i].Annotations[httpauth.IDAnnotation]; v != "" {
					ids[v] = list.Items[i].Name
				}
			}
			d.ids, d.idsAt = ids, d.now()
		}
		name, ok = d.ids[id]
	}
	d.mu.Unlock()
	if !ok {
		return nil, nil
	}
	var dp devpodv1alpha1.DevPod
	if err := d.c.Get(ctx, client.ObjectKey{Name: name, Namespace: d.ns}, &dp); err != nil || dp.Annotations[httpauth.IDAnnotation] != id {
		return nil, nil
	}
	return &Found{DevPod: &dp, Node: d.nodeOf(ctx, &dp)}, nil
}
