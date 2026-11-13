// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/mrhaoxx/devpod/internal/render"
)

// resourceGPU is the device plugin's resource name for a GPU.
const resourceGPU = corev1.ResourceName("nvidia.com/gpu")

// NodeUsage is what pods other than DevPods hold on a node: the sum of
// their requests.
type NodeUsage struct {
	CPU float64 `json:"cpu,omitempty"` // cores
	Mem float64 `json:"mem,omitempty"` // bytes
	GPU int     `json:"gpu,omitempty"`
}

// TallyNodes sums, per node, the requests of the pods placed there that
// have not finished, leaving out the DevPods' own pods (in
// devpodNamespace, labelled with their DevPod): the console counts those
// from the DevPods. A pod's request is its containers' together, or its
// largest init container's when that is more, resource by resource.
func TallyNodes(pods []*corev1.Pod, devpodNamespace string) map[string]NodeUsage {
	out := map[string]NodeUsage{}
	for _, p := range pods {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if _, ok := p.Labels[render.LabelDevPod]; ok && p.Namespace == devpodNamespace {
			continue
		}
		req := corev1.ResourceList{}
		for _, c := range p.Spec.Containers {
			for name, q := range c.Resources.Requests {
				cur := req[name]
				cur.Add(q)
				req[name] = cur
			}
		}
		for _, c := range p.Spec.InitContainers {
			for name, q := range c.Resources.Requests {
				if cur, ok := req[name]; !ok || q.Cmp(cur) > 0 {
					req[name] = q
				}
			}
		}
		for name, q := range p.Spec.Overhead {
			cur := req[name]
			cur.Add(q)
			req[name] = cur
		}
		u := out[p.Spec.NodeName]
		u.CPU += req.Cpu().AsApproximateFloat64()
		u.Mem += req.Memory().AsApproximateFloat64()
		if g, ok := req[resourceGPU]; ok {
			u.GPU += int(g.Value())
		}
		out[p.Spec.NodeName] = u
	}
	for n, u := range out {
		if u == (NodeUsage{}) {
			delete(out, n)
		}
	}
	return out
}

// PodUsage keeps a watch on all of the cluster's pods (only what
// TallyNodes reads of each) to tell what other workloads hold per node.
type PodUsage struct {
	inf             cache.SharedIndexInformer
	devpodNamespace string
}

// NewPodUsage returns a PodUsage; Run starts its watch.
func NewPodUsage(cs kubernetes.Interface, devpodNamespace string) *PodUsage {
	inf := informers.NewSharedInformerFactory(cs, 0).Core().V1().Pods().Informer()
	_ = inf.SetTransform(slimPod)
	return &PodUsage{inf: inf, devpodNamespace: devpodNamespace}
}

// Run watches until ctx ends.
func (u *PodUsage) Run(ctx context.Context) { u.inf.Run(ctx.Done()) }

// Nodes is the tally now; nil until the watch has its first list.
func (u *PodUsage) Nodes() map[string]NodeUsage {
	if !u.inf.HasSynced() {
		return nil
	}
	objs := u.inf.GetStore().List()
	pods := make([]*corev1.Pod, 0, len(objs))
	for _, o := range objs {
		if p, ok := o.(*corev1.Pod); ok {
			pods = append(pods, p)
		}
	}
	return TallyNodes(pods, u.devpodNamespace)
}

// slimPod keeps of a pod only what TallyNodes reads: a large cluster has
// many pods.
func slimPod(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	keep := func(cs []corev1.Container) []corev1.Container {
		out := make([]corev1.Container, len(cs))
		for i, c := range cs {
			out[i] = corev1.Container{Resources: corev1.ResourceRequirements{Requests: c.Resources.Requests}}
		}
		return out
	}
	s := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name, UID: p.UID, ResourceVersion: p.ResourceVersion},
		Spec:       corev1.PodSpec{NodeName: p.Spec.NodeName, Containers: keep(p.Spec.Containers), InitContainers: keep(p.Spec.InitContainers), Overhead: p.Spec.Overhead},
		Status:     corev1.PodStatus{Phase: p.Status.Phase},
	}
	if v, ok := p.Labels[render.LabelDevPod]; ok {
		s.Labels = map[string]string{render.LabelDevPod: v}
	}
	return s, nil
}
