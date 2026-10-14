// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// int32Clamped converts n to int32, clamping to math.MaxInt32 rather than
// wrapping on overflow. Every n this file passes through here is a pod or
// replica count from a test cluster, nowhere near that bound; the clamp
// exists so the conversion is safe by construction rather than by that
// observation, which is also what satisfies gosec's G115 without a nolint
// at every call site.
func int32Clamped(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	//nolint:gosec // G115: n <= math.MaxInt32 is checked immediately above.
	return int32(n)
}

// podTemplateHash is FNV-32a of the pod template's JSON encoding, formatted
// as 8 hex digits. encoding/json sorts map keys and never reorders struct
// fields, so two calls on an equal template always agree: this is what lets
// the hash stand in for "has the template changed" across sweeps without the
// sim keeping its own copy of the last one it saw.
//
// Real Kubernetes hashes a template with spew plus FNV, which this does not
// reproduce; nothing here compares against a real cluster's hash, so only
// stability within one sim run matters.
func podTemplateHash(tpl *corev1.PodTemplateSpec) string {
	data, err := json.Marshal(tpl)
	if err != nil {
		// Marshal fails only on a template holding something json cannot
		// encode, such as a channel, which the Kubernetes API types never
		// carry: a Pod spec round-trips through JSON to be written to etcd.
		panic(fmt.Sprintf("ctrltest: pod template does not marshal to JSON: %v", err))
	}
	h := fnv.New32a()
	_, _ = h.Write(data) // hash.Hash.Write never returns an error.
	return fmt.Sprintf("%08x", h.Sum32())
}

// isPodReady reports whether p's PodReady condition is True.
func isPodReady(p *corev1.Pod) bool {
	return conditionTrue(p.Status.Conditions, corev1.PodReady)
}

// controllerRef is the owner reference a workload sim sets on everything it
// creates for owner: Controller and BlockOwnerDeletion both true, as the real
// controllers that own pods and claims set them.
func controllerRef(owner metav1.Object, apiVersion, kind string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               owner.GetName(),
		UID:                owner.GetUID(),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// ownerRef is a non-controller owner reference to owner. Used for the
// StatefulSet PVC retention policy: a PVC a StatefulSet owns under
// whenDeleted:Delete is not "controlled by" it the way a pod is, since
// nothing else ever competes for the PVC's controller reference the way,
// say, two operators might race to control the same pod.
func ownerRef(owner metav1.Object, apiVersion, kind string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               owner.GetName(),
		UID:                owner.GetUID(),
		BlockOwnerDeletion: ptr.To(true),
	}
}

const (
	deploymentAPIVersion  = "apps/v1"
	deploymentKind        = "Deployment"
	statefulSetAPIVersion = "apps/v1"
	statefulSetKind       = "StatefulSet"
)

// mergeLabels returns a new map holding base's entries, then over's,
// overwriting on a shared key. Either may be nil. A fresh map every call: the
// result is about to become an object's own Labels, and sharing one between
// several objects would let a label change on one bleed into the others.
func mergeLabels(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// nextOrdinals returns the n smallest non-negative integers not already in
// used, in ascending order. Used by both workloads to pick deterministic
// names for pods it creates in one sweep: computed fresh from what currently
// exists rather than from any counter the sim remembers, so a name is never
// skipped or reused across a restart of the process running the sim.
func nextOrdinals(used map[int]bool, n int) []int {
	out := make([]int, 0, n)
	for i := 0; len(out) < n; i++ {
		if !used[i] {
			out = append(out, i)
		}
	}
	return out
}

// parseOrdinalSuffix extracts the trailing "-<n>" ordinal from a name with
// the given prefix, returning ok=false if the name does not have that shape.
// Used to recover which indices are already taken for a given Deployment pod
// hash, or a given StatefulSet, from pod names already in the cluster.
func parseOrdinalSuffix(name, prefix string) (n int, ok bool) {
	rest, found := strings.CutPrefix(name, prefix)
	if !found || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// deletionPriority sorts pods into the order a workload sim deletes them in,
// most deletable first: a pod at oldHash before one at the current hash,
// then a not-Ready pod before a Ready one, then the higher name before the
// lower. This is both how a scale-down picks its victims and how a rolling
// update picks the one old pod to retire, so it lives in one place rather
// than two copies that could drift apart.
func deletionPriority(pods []*corev1.Pod, oldHash func(*corev1.Pod) bool) {
	sort.Slice(pods, func(i, j int) bool {
		pi, pj := pods[i], pods[j]
		oi, oj := oldHash(pi), oldHash(pj)
		if oi != oj {
			return oi
		}
		ri, rj := isPodReady(pi), isPodReady(pj)
		if ri != rj {
			return !ri
		}
		return pi.Name > pj.Name
	})
}

// everyPodReady reports whether pods is non-empty and every pod in it is
// Ready. Empty is false rather than vacuously true: "every pod ready" guards
// an update step that should wait for pods to exist, not proceed because
// there are none yet.
func everyPodReady(pods []*corev1.Pod) bool {
	if len(pods) == 0 {
		return false
	}
	for _, p := range pods {
		if !isPodReady(p) {
			return false
		}
	}
	return true
}

// isControlledBy reports whether obj carries a controller reference to owner
// under apiVersion/kind. Checking the kind and version, not just the UID, is
// what keeps a workload sim from ever touching an object a consumer's own
// controller happens to also own by coincidence of UID reuse across kinds,
// which cannot happen with real UIDs but costs nothing to rule out here.
func isControlledBy(obj, owner metav1.Object, apiVersion, kind string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller &&
			ref.APIVersion == apiVersion && ref.Kind == kind && ref.UID == owner.GetUID() {
			return true
		}
	}
	return false
}

// podTemplateHashLabel is the label a Deployment-owned pod carries its
// template hash under, matching the key Kubernetes itself uses
// (apps.DefaultDeploymentUniqueLabelKey) so a test asserting on it reads the
// same as it would against a real cluster.
const podTemplateHashLabel = "pod-template-hash"

// podHash reads a pod's pod-template-hash label, empty if it has none.
func podHash(p *corev1.Pod) string {
	return p.Labels[podTemplateHashLabel]
}

// deploymentReplicas is spec.replicas, defaulting to 1 as the API server
// does; a fake client or a test-constructed object does not apply that
// default, so every reader of Spec.Replicas in this file goes through here or
// its StatefulSet counterpart rather than dereferencing the pointer directly.
func deploymentReplicas(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

func statefulSetReplicas(ss *appsv1.StatefulSet) int32 {
	if ss.Spec.Replicas == nil {
		return 1
	}
	return *ss.Spec.Replicas
}
