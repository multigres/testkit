// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
// genuinely absent, true with uid set for one that exists. The zero value is
// never looked at directly; resolveOwner's second return says whether this is
// meaningful at all.
type ownerResolution struct {
	found bool
	uid   types.UID
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

	// Foreground and orphan finalizers first, and in place on objs: an
	// orphan dependent has its owner reference stripped from the very
	// client.Object held in objs, so the background pass below sees the
	// post-strip state rather than reaching for the same owner again through
	// a reference that no longer exists in the cluster.
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
			_ = s.c.Delete(ctx, obj)
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
// which has a deletionTimestamp and the foregroundDeletion finalizer: delete
// every dependent of owner found among objs, and once none remain, remove the
// finalizer so the API server can finish deleting owner itself.
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
		_ = s.c.Delete(ctx, dep)
	}
	if remaining == 0 {
		s.removeFinalizer(ctx, owner, metav1.FinalizerDeleteDependents)
	}
}

// orphanDependents is one sweep of orphan deletion for owner, which has a
// deletionTimestamp and the orphan finalizer: strip owner's reference from
// every dependent found among objs, then remove the finalizer. Unlike
// foreground deletion this always finishes in one sweep, since stripping a
// reference is an action this sim takes directly rather than one it waits on
// another object's deletion to complete.
func (s *DataPlaneSim) orphanDependents(
	ctx context.Context,
	owner client.Object,
	objs []client.Object,
) {
	for _, dep := range objs {
		if !referencesUID(dep, owner.GetUID()) {
			continue
		}
		base, ok := dep.DeepCopyObject().(client.Object)
		if !ok {
			continue
		}
		kept := make([]metav1.OwnerReference, 0, len(dep.GetOwnerReferences()))
		for _, ref := range dep.GetOwnerReferences() {
			if ref.UID != owner.GetUID() {
				kept = append(kept, ref)
			}
		}
		dep.SetOwnerReferences(kept)
		_ = s.c.Patch(ctx, dep, client.StrategicMergeFrom(base))
	}
	s.removeFinalizer(ctx, owner, metav1.FinalizerOrphanDependents)
}

// removeFinalizer patches finalizer off obj with a strategic merge patch, so
// a finalizer another writer added to obj in the meantime survives: the same
// reasoning tickPods already applies to pod conditions.
func (s *DataPlaneSim) removeFinalizer(ctx context.Context, obj client.Object, finalizer string) {
	base, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return
	}
	kept := make([]string, 0, len(obj.GetFinalizers()))
	for _, f := range obj.GetFinalizers() {
		if f != finalizer {
			kept = append(kept, f)
		}
	}
	obj.SetFinalizers(kept)
	_ = s.c.Patch(ctx, obj, client.StrategicMergeFrom(base))
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
// the rest of the sweep. The second return is false when the kind cannot be
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

	pom := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: ref.APIVersion, Kind: ref.Kind},
	}
	err = s.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, pom)
	switch {
	case apierrors.IsNotFound(err):
		res := ownerResolution{found: false}
		cache[key] = &res
		return res, true
	case err != nil:
		// Transient or unexpected: not cached, so the next sweep tries again
		// rather than carrying a wrong answer forward for the rest of this
		// one.
		return ownerResolution{}, false
	default:
		res := ownerResolution{found: true, uid: pom.UID}
		cache[key] = &res
		return res, true
	}
}
