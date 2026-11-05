// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// These tests drive garbage collection from a List taken before the cluster
// moved on, which is the window a sweep under load really acts in: an owner
// recreated under the same name, a dependent recreated or adopted, a write
// landing between the sim's read and its delete. They run against envtest
// rather than the fake client because the fake enforces neither a UID
// precondition on Delete nor the foreground and orphan finalizers the API
// server adds.
//
// Dependents are Secrets and owners ServiceAccounts, two kinds the suite's own
// sim does not watch, so nothing but the sim each test builds and steps by
// hand ever collects a dependent or clears an owner's finalizer here. An
// owner of a watched kind would race that sim for its finalizers.

// staleGC is a garbage-collecting sim stepped by hand, over one test's
// namespace, whose reads of a dependent's live metadata can be intercepted.
type staleGC struct {
	*C
	ctx context.Context
	sim *DataPlaneSim

	// afterLiveGet, keyed by "Kind/name", runs after the sim's nth live
	// read of that object returns (n counting from 1), so a test can change
	// the cluster in the window between that read and the write it decides.
	afterLiveGet map[string]map[int]func()
	liveGets     map[string]int
}

func newStaleGC(t *testing.T) *staleGC {
	t.Helper()
	g := &staleGC{
		C:            shared.Case(t),
		ctx:          context.Background(),
		afterLiveGet: map[string]map[int]func(){},
		liveGets:     map[string]int{},
	}
	cl := interceptor.NewClient(shared.Client, interceptor.Funcs{
		Get: func(
			ctx context.Context, w client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			err := w.Get(ctx, key, obj, opts...)
			pom, ok := obj.(*metav1.PartialObjectMetadata)
			if !ok || key.Namespace != g.NS {
				return err
			}
			id := pom.Kind + "/" + key.Name
			g.liveGets[id]++
			if hook := g.afterLiveGet[id][g.liveGets[id]]; hook != nil {
				hook()
			}
			return err
		},
	})
	g.sim = NewDataPlaneSimWithOptions(cl, time.Hour, DataPlaneSimOptions{
		GarbageCollection: true,
		WatchedKinds:      []client.ObjectList{&corev1.SecretList{}, &corev1.ServiceAccountList{}},
		RESTMapper:        shared.Client.RESTMapper(),
	})
	return g
}

// after registers fn to run after the sim's nth live read of dependent name.
func (g *staleGC) after(name string, n int, fn func()) { g.afterKind("Secret", name, n, fn) }

// afterKind is after for an object of any kind.
func (g *staleGC) afterKind(kind, name string, n int, fn func()) {
	id := kind + "/" + name
	if g.afterLiveGet[id] == nil {
		g.afterLiveGet[id] = map[int]func(){}
	}
	g.afterLiveGet[id][n] = fn
}

// list is the sweep's List, narrowed to this test's namespace so a sweep
// never acts on another test's objects.
func (g *staleGC) list() []client.Object {
	g.Helper()
	objs, err := g.sim.listWatched(g.ctx)
	g.NoError(err, "list watched kinds")
	return slices.DeleteFunc(objs, func(o client.Object) bool { return o.GetNamespace() != g.NS })
}

func (g *staleGC) sweep(objs []client.Object) { g.sim.sweepGC(g.ctx, objs) }

func (g *staleGC) owner(name string) *corev1.ServiceAccount {
	g.Helper()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: g.NS}}
	g.NoError(shared.Client.Create(g.ctx, sa), "create owner %s", name)
	return sa
}

func (g *staleGC) dependent(name string, owners ...metav1.OwnerReference) *corev1.Secret {
	g.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: g.NS, OwnerReferences: owners,
	}}
	g.NoError(shared.Client.Create(g.ctx, s), "create dependent %s", name)
	return s
}

// recreate deletes dependent name and creates a new one in its place, with a
// new UID, owned by owners.
func (g *staleGC) recreate(name string, owners ...metav1.OwnerReference) *corev1.Secret {
	g.Helper()
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: g.NS}}
	g.NoError(shared.Client.Delete(g.ctx, old), "delete dependent %s", name)
	return g.dependent(name, owners...)
}

func (g *staleGC) deleteOwner(sa *corev1.ServiceAccount, policy metav1.DeletionPropagation) {
	g.Helper()
	err := shared.Client.Delete(g.ctx, sa, client.PropagationPolicy(policy))
	g.NoError(err, "delete owner %s", sa.Name)
}

// get returns dependent name, or nil if it is gone.
func (g *staleGC) get(name string) *corev1.Secret {
	g.Helper()
	s := &corev1.Secret{}
	err := shared.Client.Get(g.ctx, client.ObjectKey{Namespace: g.NS, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	g.NoError(err, "get dependent %s", name)
	return s
}

func (g *staleGC) getOwner(name string) *corev1.ServiceAccount {
	g.Helper()
	sa := &corev1.ServiceAccount{}
	err := shared.Client.Get(g.ctx, client.ObjectKey{Namespace: g.NS, Name: name}, sa)
	if apierrors.IsNotFound(err) {
		return nil
	}
	g.NoError(err, "get owner %s", name)
	return sa
}

func (g *staleGC) relabel(name string) {
	g.Helper()
	s := g.get(name)
	g.NotNil(s, "relabel: dependent %s is gone", name)
	base := s.DeepCopy()
	s.Labels = map[string]string{"bump": s.ResourceVersion}
	g.NoError(shared.Client.Patch(g.ctx, s, client.MergeFrom(base)), "relabel %s", name)
}

func saRef(sa *corev1.ServiceAccount) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ServiceAccount",
		Name:       sa.Name,
		UID:        sa.UID,
	}
}

func ownerUIDs(s *corev1.Secret) []types.UID {
	var out []types.UID
	for _, r := range s.OwnerReferences {
		out = append(out, r.UID)
	}
	return out
}

// TestGCStaleListSparesARecreatedDependent is an owner and its dependent
// both deleted and recreated under the same names after the sweep's List: the
// dependent the List saw is gone, and the one now holding its name belongs to
// a live owner. Deleting by the listed name would take the new one.
func TestGCStaleListSparesARecreatedDependent(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	g.dependent("pool-0", saRef(shard))
	g.dependent("orphan", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationBackground)

	objs := g.list()
	shard2 := g.owner("shard")
	fresh := g.recreate("pool-0", saRef(shard2))

	g.sweep(objs)

	got := g.get("pool-0")
	g.NotNil(got, "the recreated pool-0, owned by the live shard, was collected")
	g.Eq(fresh.UID, got.UID, "pool-0 is not the recreated incarnation")
	g.Nil(g.get("orphan"), "a dependent whose only owner is gone was not collected")
}

// TestGCStaleListSparesADependentAdoptedSinceTheList is the same dependent,
// same UID, listed while its only owner was dead and adopted by a live owner
// before the sweep acts. A UID precondition still matches it, so only judging
// its owners from a fresh read saves it. The reference to the dead owner is
// stripped, as the real collector does for a dependent with a live owner.
func TestGCStaleListSparesADependentAdoptedSinceTheList(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	data := g.dependent("data", saRef(shard))
	g.dependent("orphan", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationBackground)

	objs := g.list()
	anchor := g.owner("anchor")
	base := data.DeepCopy()
	data.OwnerReferences = append(data.OwnerReferences, saRef(anchor))
	g.NoError(shared.Client.Patch(g.ctx, data, client.MergeFrom(base)), "adopt data")

	g.sweep(objs)

	got := g.get("data")
	g.NotNil(got, "data, adopted by a live owner after the List, was collected")
	g.Eq(data.UID, got.UID, "data is not the same incarnation")
	g.ElementsMatch([]types.UID{anchor.UID}, ownerUIDs(got), "owners after the sweep")
	g.Nil(g.get("orphan"), "a dependent whose only owner is gone was not collected")
}

// TestGCDeleteIsPreconditionedOnTheLiveRead covers the window after the sim
// has read a dependent and judged it collectable: the dependent is recreated
// before the delete lands. Only a precondition on the read's UID and
// resourceVersion keeps the delete off the new object.
func TestGCDeleteIsPreconditionedOnTheLiveRead(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	g.dependent("pool-0", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationBackground)

	var fresh *corev1.Secret
	g.after("pool-0", 1, func() { fresh = g.recreate("pool-0") })

	g.sweep(g.list())

	got := g.get("pool-0")
	g.NotNil(got, "pool-0, recreated between the sim's read and its delete, was deleted")
	g.Eq(fresh.UID, got.UID, "pool-0 is not the recreated incarnation")
}

// TestGCRetriesADeleteThatConflictedOnUnrelatedWrites is the liveness side of
// the resourceVersion precondition: an orphan written to between the sim's
// read and its delete, without its owners changing, is still collected in
// the same sweep, so an object under constant unrelated writes is never
// starved.
func TestGCRetriesADeleteThatConflictedOnUnrelatedWrites(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	g.dependent("busy", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationBackground)

	g.after("busy", 1, func() { g.relabel("busy") })

	g.sweep(g.list())

	g.Nil(g.get("busy"), "an orphan with an unrelated write in flight was not collected")
}

// TestGCRetryIsStillPreconditionedOnUID is the retry after a conflict landing
// on a recreated object: the retry drops the resourceVersion, but not the UID.
func TestGCRetryIsStillPreconditionedOnUID(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	g.dependent("busy", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationBackground)

	var fresh *corev1.Secret
	g.after("busy", 1, func() { g.relabel("busy") })
	g.after("busy", 2, func() { fresh = g.recreate("busy") })

	g.sweep(g.list())

	got := g.get("busy")
	g.NotNil(got, "busy, recreated before the retried delete, was deleted")
	g.Eq(fresh.UID, got.UID, "busy is not the recreated incarnation")
}

// TestGCForegroundStripsADependentWithAnotherLiveOwner pins foreground
// deletion against a dependent that something else still owns: it loses its
// reference to the deleting owner rather than being deleted, a dependent with
// no other owner is deleted, and the owner's finalizer comes off once neither
// references it.
func TestGCForegroundStripsADependentWithAnotherLiveOwner(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	anchor := g.owner("anchor")
	g.dependent("shared", saRef(shard), saRef(anchor))
	g.dependent("solo", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationForeground)

	g.sweep(g.list())

	got := g.get("shared")
	g.NotNil(got, "a dependent with a live second owner was deleted by foreground deletion")
	g.ElementsMatch([]types.UID{anchor.UID}, ownerUIDs(got), "owners after the sweep")
	g.Nil(g.get("solo"), "a dependent of a foreground-deleted owner was not deleted")

	g.sweep(g.list())
	g.Nil(g.getOwner("shard"), "foreground owner still present once no dependent references it")
}

// TestGCOrphanLeavesTheFinalizerUntilEveryStripLands is an owner reference
// added to a dependent between the sim's read of it and its strip of the
// orphaning owner. The strip must not erase the new reference, and the
// owner's orphan finalizer must stay until a strip lands: removed early, the
// owner goes while the dependent still names it.
func TestGCOrphanLeavesTheFinalizerUntilEveryStripLands(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	anchor := g.owner("anchor")
	kept := g.dependent("kept", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationOrphan)

	g.after("kept", 1, func() {
		cur := g.get("kept")
		base := cur.DeepCopy()
		cur.OwnerReferences = append(cur.OwnerReferences, saRef(anchor))
		g.NoError(shared.Client.Patch(g.ctx, cur, client.MergeFrom(base)), "adopt kept")
	})

	g.sweep(g.list())

	got := g.get("kept")
	g.NotNil(got, "an orphaned dependent was deleted")
	g.Contains(
		ownerUIDs(got),
		anchor.UID,
		"the strip erased an owner reference added after the read",
	)
	owner := g.getOwner("shard")
	g.NotNil(owner, "orphan finalizer came off although the strip did not land")

	g.sweep(g.list())

	got = g.get("kept")
	g.NotNil(got, "an orphaned dependent was deleted")
	g.Eq(kept.UID, got.UID, "kept is not the same incarnation")
	g.ElementsMatch([]types.UID{anchor.UID}, ownerUIDs(got), "owners after orphaning")
	g.Nil(g.getOwner("shard"), "orphaning owner still present after its dependent was stripped")
}

// TestGCFinalizerRemovalSparesARecreatedOwner is a foreground-deleting owner
// that finished deleting and was recreated, under the same name, and is
// itself being foreground-deleted with a dependent still alive, all after the
// sweep's List. The listed owner has no dependents left, so the sweep removes
// its finalizer; landing on the new owner, that would let it go while its
// dependent still exists.
func TestGCFinalizerRemovalSparesARecreatedOwner(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	g.deleteOwner(shard, metav1.DeletePropagationForeground)

	objs := g.list()
	cur := g.getOwner("shard")
	base := cur.DeepCopy()
	cur.Finalizers = nil
	g.NoError(shared.Client.Patch(g.ctx, cur, client.MergeFrom(base)), "finish deleting shard")
	g.Nil(g.getOwner("shard"), "shard still present after its last finalizer came off")
	shard2 := g.owner("shard")
	g.dependent("child", saRef(shard2))
	g.deleteOwner(shard2, metav1.DeletePropagationForeground)

	g.sweep(objs)

	got := g.getOwner("shard")
	g.NotNil(got, "the recreated shard was released while its dependent still exists")
	g.Eq(shard2.UID, got.UID, "shard is not the recreated incarnation")
	g.NotNil(g.get("child"), "child was collected from a List that never saw it")

	g.sweep(g.list())
	g.Nil(g.get("child"), "a dependent of a foreground-deleted owner was not deleted")
	g.sweep(g.list())
	g.Nil(g.getOwner("shard"), "foreground owner still present once no dependent references it")
}

// TestGCStaleListSparesADependentReleasedSinceTheList is a dependent listed
// with only a dead owner whose owner references were all removed before the
// sweep acts. With no owners left it is nobody's dependent, and garbage
// collection has nothing to say about it.
func TestGCStaleListSparesADependentReleasedSinceTheList(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	data := g.dependent("data", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationBackground)

	objs := g.list()
	base := data.DeepCopy()
	data.OwnerReferences = nil
	g.NoError(shared.Client.Patch(g.ctx, data, client.MergeFrom(base)), "release data")

	g.sweep(objs)

	g.NotNil(g.get("data"), "data, released by every owner after the List, was collected")
}

// TestGCSparesADependentAdoptedAfterTheLiveRead is adoption inside the window
// between the sim's live read of a dependent and its delete: same UID, so only
// the resourceVersion precondition catches it, and the conflict retry must
// see the changed owners and stand down rather than retry under the UID alone.
func TestGCSparesADependentAdoptedAfterTheLiveRead(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	anchor := g.owner("anchor")
	data := g.dependent("data", saRef(shard))
	g.deleteOwner(shard, metav1.DeletePropagationBackground)

	g.after("data", 1, func() {
		cur := g.get("data")
		base := cur.DeepCopy()
		cur.OwnerReferences = append(cur.OwnerReferences, saRef(anchor))
		g.NoError(shared.Client.Patch(g.ctx, cur, client.MergeFrom(base)), "adopt data")
	})

	g.sweep(g.list())

	got := g.get("data")
	g.NotNil(got, "data, adopted between the sim's read and its delete, was deleted")
	g.Eq(data.UID, got.UID, "data is not the same incarnation")
}

// TestGCFinalizerRemovalRetriesOnUnrelatedWrites is a foreground owner with
// no dependents left, written to between the sim's read of it and its
// finalizer patch. The patch conflicts, and the retry from a fresh read must
// still release the owner in the same sweep, so an owner under constant
// writes is never held.
func TestGCFinalizerRemovalRetriesOnUnrelatedWrites(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	g.deleteOwner(shard, metav1.DeletePropagationForeground)

	g.afterKind("ServiceAccount", "shard", 1, func() {
		cur := g.getOwner("shard")
		base := cur.DeepCopy()
		cur.Labels = map[string]string{"bump": cur.ResourceVersion}
		g.NoError(shared.Client.Patch(g.ctx, cur, client.MergeFrom(base)), "relabel shard")
	})

	g.sweep(g.list())

	g.Nil(g.getOwner("shard"), "foreground owner held by an unrelated write in flight")
}

// TestGCForegroundLeavesADependentWithAnUnresolvableOwner is a dependent of a
// foreground-deleting owner whose other owner is of a kind the RESTMapper
// does not know. Whether that owner is live is unknown, so the dependent is
// neither deleted nor stripped of its reference to the deleting owner: the
// real collector retries the lookup rather than guess, and the owner waits.
func TestGCForegroundLeavesADependentWithAnUnresolvableOwner(t *testing.T) {
	g := newStaleGC(t)
	shard := g.owner("shard")
	unknown := metav1.OwnerReference{
		APIVersion: "example.com/v1", Kind: "Widget", Name: "w", UID: types.UID("widget-uid"),
	}
	g.dependent("data", saRef(shard), unknown)
	g.deleteOwner(shard, metav1.DeletePropagationForeground)

	g.sweep(g.list())

	got := g.get("data")
	g.NotNil(got, "a dependent with an unresolvable owner was deleted")
	g.Contains(
		ownerUIDs(got),
		shard.UID,
		"the reference to the deleting owner was stripped on a guess",
	)
	g.NotNil(g.getOwner("shard"), "foreground owner released while a dependent still references it")
}
