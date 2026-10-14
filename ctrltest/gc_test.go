// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/multigres/testkit/assert"
)

// gcMapper is a RESTMapper covering every kind these tests use, standing in
// for the dynamic one a real cluster gives Boot for free from discovery. A
// kind never added to it is exactly what exercises "unknown kind: treated as
// live".
func gcMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	m.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	m.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	m.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	return m
}

type gcFixture struct {
	c    *assert.C
	cl   client.Client
	sim  *DataPlaneSim
	gets atomic.Int32
}

// newGCFixture builds a sim with garbage collection on over kinds, seeded
// with objs. gets counts every Get the sim issues, which is what the caching
// test below needs to see without reaching into resolveOwner's private map.
func newGCFixture(t *testing.T, kinds []client.ObjectList, objs ...client.Object) *gcFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	c := assert.NewAborting(t)
	c.NoError(clientgoscheme.AddToScheme(scheme), "add to scheme")
	c.NoError(appsv1.AddToScheme(scheme), "add apps/v1 to scheme")

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	f := &gcFixture{c: c}
	f.cl = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(
			ctx context.Context, w client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*metav1.PartialObjectMetadata); ok {
				f.gets.Add(1)
			}
			return w.Get(ctx, key, obj, opts...)
		},
	})
	f.sim = NewDataPlaneSimWithOptions(f.cl, time.Hour, DataPlaneSimOptions{
		GarbageCollection: true,
		WatchedKinds:      kinds,
		RESTMapper:        gcMapper(),
	})
	return f
}

func (f *gcFixture) tick() { f.sim.tickGC(context.Background()) }

func (f *gcFixture) exists(gvk string, key client.ObjectKey) bool {
	f.c.Helper()
	var obj client.Object
	switch gvk {
	case "Pod":
		obj = &corev1.Pod{}
	case "ConfigMap":
		obj = &corev1.ConfigMap{}
	default:
		f.c.Fatalf("gcFixture.exists: unknown kind %q", gvk)
	}
	return f.cl.Get(context.Background(), key, obj) == nil
}

var podAndConfigMapKinds = []client.ObjectList{&corev1.PodList{}, &corev1.ConfigMapList{}}

func TestGCDisabledIsANoOp(t *testing.T) {
	dep := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: "ghost", UID: types.UID("ghost-uid")},
		},
	}}
	scheme := runtime.NewScheme()
	c := assert.NewAborting(t)
	c.NoError(clientgoscheme.AddToScheme(scheme), "add to scheme")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	sim := NewDataPlaneSim(cl, time.Hour) // GarbageCollection off by construction.

	sim.tickGC(context.Background())

	c.NoError(cl.Get(context.Background(), client.ObjectKeyFromObject(dep), &corev1.ConfigMap{}),
		"a dangling dependent was collected although GarbageCollection is off")
}

// TestGCBackgroundCascadeDeletesWhenEveryOwnerIsDangling pins the basic
// cascade: envtest's apiserver deletes an owner with no finalizers
// immediately, so by the time a sweep sees the dependent, the owner is
// already gone and nothing but this sim would ever clean up after it.
func TestGCBackgroundCascadeDeletesWhenEveryOwnerIsDangling(t *testing.T) {
	dep := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: "ghost", UID: types.UID("ghost-uid")},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, dep)
	f.tick()
	f.c.False(f.exists("ConfigMap", client.ObjectKeyFromObject(dep)),
		"a dependent with no live owner survived a sweep")
}

// TestGCKeepsADependentWithOneLiveOwner pins "every" in the design: one live
// owner among several references is enough to keep the dependent, even
// though another reference on the same object is dangling.
func TestGCKeepsADependentWithOneLiveOwner(t *testing.T) {
	owner := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "owner", Namespace: "ns", UID: types.UID("owner-uid"),
	}}
	dep := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: "ghost", UID: types.UID("ghost-uid")},
			{APIVersion: "v1", Kind: "Pod", Name: "owner", UID: types.UID("owner-uid")},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, owner, dep)
	f.tick()
	f.c.True(f.exists("ConfigMap", client.ObjectKeyFromObject(dep)),
		"a dependent with one live owner was collected")
}

// TestGCDeletesWhenOwnerUIDDoesNotMatch pins the name-reuse case: an object by
// the referenced name exists, but it is not the same object the reference was
// made to, which is exactly what a deleted-and-recreated owner looks like.
func TestGCDeletesWhenOwnerUIDDoesNotMatch(t *testing.T) {
	owner := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "owner", Namespace: "ns", UID: types.UID("new-uid"),
	}}
	dep := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: "owner", UID: types.UID("old-uid")},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, owner, dep)
	f.tick()
	f.c.False(f.exists("ConfigMap", client.ObjectKeyFromObject(dep)),
		"a dependent survived an owner reference whose UID no longer matches")
}

// TestGCKeepsADependentWithAnUnknownOwnerKind pins the guard against deleting
// on a guess: a kind the RESTMapper cannot map is never resolved, so a
// reference to it can never make a dependent eligible for cascade.
func TestGCKeepsADependentWithAnUnknownOwnerKind(t *testing.T) {
	dep := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "example.com/v1", Kind: "Widget", Name: "w", UID: types.UID("w-uid")},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, dep)
	f.tick()
	f.c.True(f.exists("ConfigMap", client.ObjectKeyFromObject(dep)),
		"a dependent of an unresolvable owner kind was collected")
}

// TestGCIgnoresADependentOfAnUnwatchedKind pins that garbage collection only
// ever lists WatchedKinds: a Secret here has a dangling owner but is never a
// dependent GC can see, because nothing told it to list Secrets.
func TestGCIgnoresADependentOfAnUnwatchedKind(t *testing.T) {
	dep := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: "ghost", UID: types.UID("ghost-uid")},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, dep) // SecretList is not among them.
	f.tick()
	got := &corev1.Secret{}
	f.c.NoError(f.cl.Get(context.Background(), client.ObjectKeyFromObject(dep), got),
		"a dependent of an unwatched kind was collected")
}

// TestGCForegroundDeletionCascadesThenRemovesFinalizer drives the sequence an
// explicit Foreground delete needs: the apiserver itself sets the
// deletionTimestamp and keeps the foregroundDeletion finalizer (the fake
// client reproduces this once the object already carries a finalizer), and
// this sim is the only thing that ever deletes the dependents or clears the
// finalizer in envtest. Two sweeps: the first deletes the dependents, and
// only the second, once a fresh List shows them gone, removes the finalizer
// and lets the owner itself finish deleting.
func TestGCForegroundDeletionCascadesThenRemovesFinalizer(t *testing.T) {
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "owner", Namespace: "ns", UID: types.UID("owner-uid"),
		Finalizers: []string{metav1.FinalizerDeleteDependents},
	}}
	dep1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "dep1",
		Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner-uid"},
		},
	}}
	dep2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "dep2",
		Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner-uid"},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, owner, dep1, dep2)
	f.c.NoError(f.cl.Delete(context.Background(), owner), "delete owner")

	f.tick()
	f.c.False(f.exists("Pod", client.ObjectKeyFromObject(dep1)), "dep1 survived the first sweep")
	f.c.False(f.exists("Pod", client.ObjectKeyFromObject(dep2)), "dep2 survived the first sweep")
	got := &corev1.ConfigMap{}
	f.c.NoError(f.cl.Get(context.Background(), client.ObjectKeyFromObject(owner), got),
		"owner was removed before its dependents were confirmed gone")
	f.c.Contains(got.Finalizers, metav1.FinalizerDeleteDependents, "finalizer removed too early")

	f.tick()
	f.c.Error(
		f.cl.Get(context.Background(), client.ObjectKeyFromObject(owner), &corev1.ConfigMap{}),
		"owner survived once its dependents were confirmed gone",
	)
}

// TestGCOrphanDeletionStripsReferenceAndRemovesFinalizer pins the other
// finalizer: the dependent survives, but the specific reference to the
// orphaning owner is gone from it, and the owner's own deletion completes in
// the same sweep since there is nothing left to wait on.
func TestGCOrphanDeletionStripsReferenceAndRemovesFinalizer(t *testing.T) {
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "owner", Namespace: "ns", UID: types.UID("owner-uid"),
		Finalizers: []string{metav1.FinalizerOrphanDependents},
	}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "other", Namespace: "ns", UID: types.UID("other-uid"),
	}}
	dep := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner-uid"},
			{APIVersion: "v1", Kind: "Pod", Name: "other", UID: "other-uid"},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, owner, other, dep)
	f.c.NoError(f.cl.Delete(context.Background(), owner), "delete owner")

	f.tick()

	got := &corev1.Pod{}
	f.c.NoError(f.cl.Get(context.Background(), client.ObjectKeyFromObject(dep), got), "get dep")
	f.c.Len(got.OwnerReferences, 1, "want only the orphaning owner's reference removed")
	f.c.Eq(types.UID("other-uid"), got.OwnerReferences[0].UID, "the surviving reference changed")

	f.c.Error(
		f.cl.Get(context.Background(), client.ObjectKeyFromObject(owner), &corev1.ConfigMap{}),
		"owner survived orphan deletion with its finalizer removed",
	)
}

// TestGCResolvesClusterScopedOwnersOutsideTheDependentsNamespace pins the
// RESTMapper-driven scope split: a namespaced Get for a cluster-scoped
// owner's name would miss it entirely in the fake client's per-namespace
// store, wrongly reporting it dangling. Namespace stands in for any
// cluster-scoped kind a consumer's own CRDs might own something by.
func TestGCResolvesClusterScopedOwnersOutsideTheDependentsNamespace(t *testing.T) {
	owner := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "cluster-owner", UID: types.UID("ns-uid"),
	}}
	dep := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "dep", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "Namespace", Name: "cluster-owner", UID: "ns-uid"},
		},
	}}
	f := newGCFixture(t, podAndConfigMapKinds, owner, dep)

	f.tick()
	f.c.True(f.exists("ConfigMap", client.ObjectKeyFromObject(dep)),
		"a dependent of a live cluster-scoped owner was collected")

	f.c.NoError(f.cl.Delete(context.Background(), owner), "delete the cluster-scoped owner")
	f.tick()
	f.c.False(f.exists("ConfigMap", client.ObjectKeyFromObject(dep)),
		"a dependent survived once its cluster-scoped owner was actually gone")
}

// TestGCCachesOwnerLookupsWithinASweep pins the point of ownerKey: many
// dependents of the same owner share one Get rather than issuing one per
// dependent, which at a quarter-second sweep interval is the difference
// between one API call and one per pod in a large StatefulSet.
func TestGCCachesOwnerLookupsWithinASweep(t *testing.T) {
	owner := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "owner", Namespace: "ns", UID: types.UID("owner-uid"),
	}}
	var deps []client.Object
	for _, name := range []string{"dep1", "dep2", "dep3"} {
		deps = append(deps, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: "owner", UID: "owner-uid"},
			},
		}})
	}
	f := newGCFixture(t, podAndConfigMapKinds, append([]client.Object{owner}, deps...)...)
	f.tick()
	f.c.Eq(int32(1), f.gets.Load(), "want one Get for three dependents of the same owner")
}
