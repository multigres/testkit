// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"reflect"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ownerKey identifies one owner lookup this sweep: what an OwnerReference
// names, plus the namespace the sim resolved it to (empty for a
// cluster-scoped kind). Caching on this rather than on the reference's UID is
// what lets every dependent of one Deployment share a single Get for it
// instead of issuing one per pod.
type ownerKey struct {
	apiVersion, kind, namespace, name string
}

// ownerResolution is what resolveOwner found: found is false for an owner
// genuinely absent, true with uid set for one that exists, and
// deletingDependents true for one that exists and is waiting on foreground
// deletion of its dependents. The zero value is never looked at directly;
// resolveOwner's second return says whether this is meaningful at all.
type ownerResolution struct {
	found              bool
	uid                types.UID
	deletingDependents bool
}

// tickGC acts as Kubernetes' garbage collector over DataPlaneSimOptions's
// WatchedKinds: background cascade for a dependent whose owners have all
// vanished, and the two finalizers real garbage collection drives,
// foregroundDeletion and orphan. A no-op unless GarbageCollection is set.
//
// envtest runs no garbage collector (see the DataPlaneSim doc comment), so
// without this a cascade delete silently leaves every dependent behind, and a
// foreground delete hangs forever: the API server sets the foregroundDeletion
// finalizer and a deletionTimestamp the moment the delete is requested, and
// nothing is left to clear the finalizer once the dependents are gone.
func (s *DataPlaneSim) tickGC(ctx context.Context) {
	if !s.opts.GarbageCollection || s.opts.RESTMapper == nil {
		return
	}

	objs, err := s.listWatched(ctx)
	if err != nil {
		return
	}
	s.sweepGC(ctx, objs)
}

// sweepGC is one garbage collection pass over objs, a List taken at some
// earlier point. Everything it decides from objs is a candidate only: by the
// time it acts, an owner may have been recreated under the same name, a
// dependent deleted and recreated, or a dependent adopted by a new owner, so
// every write re-reads its target first and is guarded by a precondition.
// See collect.
func (s *DataPlaneSim) sweepGC(ctx context.Context, objs []client.Object) {
	// Foreground and orphan finalizers first. Neither updates objs, so the
	// background pass below can still see a reference an orphan strip just
	// removed, pointing at an owner whose finalizer just came off. That is
	// safe only because collect judges each candidate from a live read, where
	// the stripped reference is gone.
	for _, obj := range objs {
		dt := obj.GetDeletionTimestamp()
		if dt.IsZero() {
			continue
		}
		switch {
		case slices.Contains(obj.GetFinalizers(), metav1.FinalizerDeleteDependents):
			s.foregroundDeleteDependents(ctx, obj, objs)
		case slices.Contains(obj.GetFinalizers(), metav1.FinalizerOrphanDependents):
			s.orphanDependents(ctx, obj, objs)
		}
	}

	cache := map[ownerKey]*ownerResolution{}
	for _, obj := range objs {
		refs := obj.GetOwnerReferences()
		if len(refs) == 0 {
			continue
		}
		dangling := true
		for _, ref := range refs {
			if !s.ownerDangling(ctx, cache, ref, obj.GetNamespace()) {
				dangling = false
				break
			}
		}
		if dangling {
			s.collect(ctx, obj)
		}
	}
}

// listWatched lists every object of every kind in DataPlaneSimOptions's
// WatchedKinds, across every namespace. A fresh List per kind rather than a
// cache: garbage collection has to see this sweep's deletions and finalizer
// removals, not a snapshot from whenever the kind was first listed.
func (s *DataPlaneSim) listWatched(ctx context.Context) ([]client.Object, error) {
	var out []client.Object
	for _, proto := range s.opts.WatchedKinds {
		list, ok := proto.DeepCopyObject().(client.ObjectList)
		if !ok {
			continue
		}
		if err := s.c.List(ctx, list); err != nil {
			return nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if obj, ok := item.(client.Object); ok {
				out = append(out, obj)
			}
		}
	}
	return out, nil
}

// foregroundDeleteDependents is one sweep of foreground deletion for owner,
// which has a deletionTimestamp and the foregroundDeletion finalizer: collect
// every dependent of owner found among objs, and once none remain, remove the
// finalizer so the API server can finish deleting owner itself. collect
// deletes a dependent whose only owners are gone or deleting, and strips the
// reference to owner from one that still has a live owner, which is what
// lets the finalizer come off without deleting an object someone else still
// owns.
//
// Two upstream refinements are not modelled. Every dependent is waited on,
// whether or not its reference sets blockOwnerDeletion, so a non-blocking
// dependent held up by a finalizer of its own also holds owner. And deletes
// carry no propagation policy, where upstream deletes a dependent that has
// dependents of its own in the foreground, so owner can be released before
// its grandchildren are gone.
//
// Deletes issued this sweep usually have not landed by the time remaining is
// counted, so the finalizer typically comes off one sweep later, once a fresh
// List shows the dependents actually gone. That is a property of running one
// List per sweep rather than re-listing after every write, and it matches the
// real garbage collector closely enough: nothing here depends on the
// finalizer coming off in the same sweep as the last dependent's delete.
func (s *DataPlaneSim) foregroundDeleteDependents(
	ctx context.Context,
	owner client.Object,
	objs []client.Object,
) {
	remaining := 0
	for _, dep := range objs {
		if !referencesUID(dep, owner.GetUID()) {
			continue
		}
		remaining++
		s.collect(ctx, dep)
	}
	if remaining == 0 {
		s.removeFinalizer(ctx, owner, metav1.FinalizerDeleteDependents)
	}
}

// orphanDependents is one sweep of orphan deletion for owner, which has a
// deletionTimestamp and the orphan finalizer: strip owner's reference from
// every dependent found among objs, then remove the finalizer. The finalizer
// comes off only once every strip has landed or found its dependent gone; one
// that failed, a conflict included, leaves it for the next sweep, because
// removing it early lets owner go while a dependent still names it, and the
// background pass would then collect the very dependent orphaning exists to
// keep.
//
// objs only says which dependents to look at. Each strip re-reads its
// dependent and patches under that read's resourceVersion, so it never lands
// on a recreated object of the same name or overwrites an owner reference
// added since: upstream orphanDependents in
// pkg/controller/garbagecollector/garbagecollector.go does the same with a
// UID precondition, falling back to deleteOwnerRefJSONMergePatch in patch.go,
// which carries the resourceVersion of a fresh read.
func (s *DataPlaneSim) orphanDependents(
	ctx context.Context,
	owner client.Object,
	objs []client.Object,
) {
	done := true
	for _, dep := range objs {
		if !referencesUID(dep, owner.GetUID()) {
			continue
		}
		live, err := s.getLive(ctx, dep)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			done = false
			continue
		}
		if !referencesUID(live, owner.GetUID()) {
			continue
		}
		err = s.stripOwnerRefs(ctx, live, func(uid types.UID) bool { return uid == owner.GetUID() })
		if err != nil && !apierrors.IsNotFound(err) {
			done = false
		}
	}
	if done {
		s.removeFinalizer(ctx, owner, metav1.FinalizerOrphanDependents)
	}
}

// removeFinalizer takes finalizer off owner, the object the sweep listed, as
// upstream's removeFinalizer (pkg/controller/garbagecollector/operations.go)
// does: read it fresh, and patch the finalizer list under that read's
// resourceVersion, retrying on conflict from a new read. A JSON merge patch
// replaces the whole list, so the lock is what keeps a concurrent change to
// the list; the retry is what keeps an owner under constant unrelated writes
// from holding its finalizer forever. The UID check keeps the patch off a
// recreated owner of the same name, which may itself be waiting on
// dependents this sweep's List never saw. A merge patch rather than a
// strategic one because the API server refuses strategic merge on custom
// resources.
func (s *DataPlaneSim) removeFinalizer(ctx context.Context, owner client.Object, finalizer string) {
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := s.getLive(ctx, owner)
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		if live.GetUID() != owner.GetUID() || !slices.Contains(live.GetFinalizers(), finalizer) {
			return nil
		}
		base := live.DeepCopy()
		live.SetFinalizers(slices.DeleteFunc(slices.Clone(live.GetFinalizers()),
			func(f string) bool { return f == finalizer }))
		return s.c.Patch(
			ctx,
			live,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}),
		)
	})
}

// collect is upstream's attemptToDeleteItem
// (pkg/controller/garbagecollector/garbagecollector.go) for one dependent the
// sweep's List marked as a candidate. listed is only a name: the List may be
// arbitrarily stale, and acting on it directly is how a sim deletes a
// recreated object that reuses the name, or an object that has since gained
// a live owner. So, as upstream does:
//
//   - Read the dependent live, and decide from that read alone.
//   - Classify each of its owners afresh, with no sweep cache: solid (exists
//     with the referenced UID and is not deleting its dependents), dangling
//     (absent, or the name now holds a different UID), or waiting (exists
//     and is under foreground deletion). An owner this sim cannot resolve
//     (an unmapped kind, a failed lookup) ends the attempt with nothing
//     done, so a guess never deletes or strips anything.
//   - With any solid owner, strip the references to dangling and waiting
//     owners instead of deleting.
//   - Otherwise delete, preconditioned on the UID and resourceVersion of the
//     live read, through deleteLive.
//
// Errors are dropped rather than returned: every failure here, a conflict
// included, means "decide again next sweep", which the next List does.
func (s *DataPlaneSim) collect(ctx context.Context, listed client.Object) {
	live, err := s.getLive(ctx, listed)
	if err != nil {
		return
	}
	refs := live.GetOwnerReferences()
	if len(refs) == 0 {
		return
	}
	var solid bool
	notSolid := map[types.UID]bool{}
	for _, ref := range refs {
		res, resolvable := s.resolveOwner(ctx, nil, ref, live.GetNamespace())
		switch {
		case !resolvable:
			// Neither solid nor dangling is known, so do nothing and let a
			// later sweep decide, as upstream's classifyReferences does by
			// returning the lookup error. Treating it as solid would strip a
			// foreground owner's reference on the strength of an owner never
			// verified to exist.
			return
		case !res.found || res.uid != ref.UID, res.deletingDependents:
			notSolid[ref.UID] = true
		default:
			solid = true
		}
	}
	if solid {
		if len(notSolid) > 0 {
			_ = s.stripOwnerRefs(ctx, live, func(uid types.UID) bool { return notSolid[uid] })
		}
		return
	}
	s.deleteLive(ctx, live)
}

// getLive reads obj's metadata straight from the API server into a fresh
// object. Metadata only, because that is all garbage collection decides on,
// and fresh rather than into a copy of obj, because decoding over a
// populated object keeps any field the response omits: an owner reference
// list emptied since obj was listed would survive the read.
func (s *DataPlaneSim) getLive(
	ctx context.Context,
	obj client.Object,
) (*metav1.PartialObjectMetadata, error) {
	gvk, err := s.c.GroupVersionKindFor(obj)
	if err != nil {
		return nil, err
	}
	live := &metav1.PartialObjectMetadata{}
	live.SetGroupVersionKind(gvk)
	if err := s.c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
		return nil, err
	}
	return live, nil
}

// deleteLive is upstream's deleteObject (operations.go): delete live under
// both its UID and its resourceVersion. The UID keeps the delete off a
// recreated object of the same name; the resourceVersion keeps it off an
// object whose owners changed after live was read.
//
// A resourceVersion conflict alone would starve collection of any object
// written to often, so, again as upstream does, a conflict is followed by one
// more read, and if the object is the same incarnation with exactly the owner
// references live was judged on, the delete is retried under the UID alone.
// Anything else (NotFound, a different UID, changed owners, any other error)
// is left to the next sweep to judge from scratch.
func (s *DataPlaneSim) deleteLive(ctx context.Context, live *metav1.PartialObjectMetadata) {
	uid, rv := live.GetUID(), live.GetResourceVersion()
	err := s.c.Delete(ctx, live, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if !apierrors.IsConflict(err) {
		return
	}
	again, err := s.getLive(ctx, live)
	if err != nil || again.GetUID() != uid || again.GetResourceVersion() == rv ||
		!reflect.DeepEqual(again.GetOwnerReferences(), live.GetOwnerReferences()) {
		return
	}
	_ = s.c.Delete(ctx, again, client.Preconditions{UID: &uid})
}

// stripOwnerRefs removes every owner reference on live whose UID drop
// matches, with a JSON merge patch under live's resourceVersion: the patch
// writes the whole remaining list, so the lock is what stops it erasing a
// reference added since live was read, and what keeps it off a recreated
// object of the same name. A merge patch rather than a strategic one because
// the API server refuses strategic merge on custom resources.
func (s *DataPlaneSim) stripOwnerRefs(
	ctx context.Context,
	live *metav1.PartialObjectMetadata,
	drop func(types.UID) bool,
) error {
	base := live.DeepCopy()
	kept := make([]metav1.OwnerReference, 0, len(live.GetOwnerReferences()))
	for _, ref := range live.GetOwnerReferences() {
		if !drop(ref.UID) {
			kept = append(kept, ref)
		}
	}
	live.SetOwnerReferences(kept)
	return s.c.Patch(
		ctx,
		live,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}),
	)
}

// referencesUID reports whether obj carries an owner reference to uid.
func referencesUID(obj client.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// ownerDangling reports whether ref no longer points at a live object: the
// referenced object does not exist, or exists with a different UID, which is
// what a name being reused after its original owner was deleted looks like.
// An owner kind this sim cannot resolve, because the RESTMapper does not know
// it or the reference is malformed, is reported as not dangling, so a guess
// never deletes something the sim might be wrong about.
func (s *DataPlaneSim) ownerDangling(
	ctx context.Context,
	cache map[ownerKey]*ownerResolution,
	ref metav1.OwnerReference,
	dependentNS string,
) bool {
	res, resolvable := s.resolveOwner(ctx, cache, ref, dependentNS)
	if !resolvable {
		return false
	}
	if !res.found {
		return true
	}
	return res.uid != ref.UID
}

// resolveOwner looks up the object ref names, caching the result in cache for
// the rest of the sweep, or with a nil cache, reading it fresh and caching
// nothing. The second return is false when the kind cannot be
// resolved at all (a malformed APIVersion, or one the RESTMapper does not
// map), as distinct from a successful lookup that found nothing.
func (s *DataPlaneSim) resolveOwner(
	ctx context.Context,
	cache map[ownerKey]*ownerResolution,
	ref metav1.OwnerReference,
	dependentNS string,
) (ownerResolution, bool) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return ownerResolution{}, false
	}
	gvk := gv.WithKind(ref.Kind)

	mapping, err := s.opts.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return ownerResolution{}, false
	}
	ns := dependentNS
	if mapping.Scope.Name() == meta.RESTScopeNameRoot {
		ns = ""
	}

	key := ownerKey{apiVersion: ref.APIVersion, kind: ref.Kind, namespace: ns, name: ref.Name}
	if cached, ok := cache[key]; ok {
		return *cached, true
	}
	remember := func(res ownerResolution) {
		if cache != nil {
			cache[key] = &res
		}
	}

	pom := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: ref.APIVersion, Kind: ref.Kind},
	}
	err = s.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, pom)
	switch {
	case apierrors.IsNotFound(err):
		res := ownerResolution{found: false}
		remember(res)
		return res, true
	case err != nil:
		// Transient or unexpected: not cached, so the next sweep tries again
		// rather than carrying a wrong answer forward for the rest of this
		// one.
		return ownerResolution{}, false
	default:
		res := ownerResolution{
			found: true,
			uid:   pom.UID,
			deletingDependents: !pom.DeletionTimestamp.IsZero() &&
				slices.Contains(pom.Finalizers, metav1.FinalizerDeleteDependents),
		}
		remember(res)
		return res, true
	}
}
