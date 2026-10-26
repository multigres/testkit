// SPDX-License-Identifier: Apache-2.0

package fakeapi

import (
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/applyconfigurations"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/testing"
	"sigs.k8s.io/structured-merge-diff/v6/typed"
)

// newTypeConverter builds the converter controller-runtime's fake uses when
// nobody supplies one: the client-go scheme's typed converter, falling back to a
// deduced one for kinds the scheme does not describe (client/fake/client.go,
// Build). Mirroring it exactly is what keeps server-side apply working, since a
// CRD-shaped test type has no typed converter and would otherwise fail every
// Apply. It is built once per builder because constructing the client-go
// scheme and its OpenAPI converter is by far the most expensive part of an
// apply.
func newTypeConverter() managedfields.TypeConverter {
	clientGoScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(clientGoScheme); err != nil {
		panic(fmt.Sprintf("failed to construct client-go scheme: %v", err))
	}
	return multiTypeConverter{upstream: []managedfields.TypeConverter{
		applyconfigurations.NewTypeConverter(clientGoScheme),
		managedfields.NewDeducedTypeConverter(),
	}}
}

// newManagedTracker is client-go's field-managed tracker over conv, as the fake
// builds it by default.
func newManagedTracker(s *runtime.Scheme, conv managedfields.TypeConverter) testing.ObjectTracker {
	return testing.NewFieldManagedObjectTracker(
		s,
		serializer.NewCodecFactory(s).UniversalDecoder(),
		conv,
	)
}

// multiTypeConverter tries each converter in turn and returns the first
// success. It is copied from controller-runtime's pkg/client/fake/typeconverter.go
// (Copyright The Kubernetes Authors, Apache-2.0, the same licence as this
// module) because the type is unexported there and the default tracker cannot
// be mirrored without it.
type multiTypeConverter struct {
	upstream []managedfields.TypeConverter
}

func (m multiTypeConverter) ObjectToTyped(
	r runtime.Object,
	o ...typed.ValidationOptions,
) (*typed.TypedValue, error) {
	var errs []error
	for _, u := range m.upstream {
		res, err := u.ObjectToTyped(r, o...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return res, nil
	}
	return nil, fmt.Errorf("failed to convert Object to TypedValue: %w", kerrors.NewAggregate(errs))
}

func (m multiTypeConverter) TypedToObject(v *typed.TypedValue) (runtime.Object, error) {
	var errs []error
	for _, u := range m.upstream {
		res, err := u.TypedToObject(v)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return res, nil
	}
	return nil, fmt.Errorf("failed to convert TypedValue to Object: %w", kerrors.NewAggregate(errs))
}

// generationTracker wraps the default tracker and keeps metadata.generation the
// way a CRD's API server does: 1 on create, then one more for each write that
// changes anything but metadata and status, and one more when deletion begins.
//
// It sits below controller-runtime's versionedTracker, which has already turned
// a status-subresource write into "the stored object with only status replaced"
// by the time it reaches here, so a status-only write compares equal and leaves
// generation alone. The bump is made on the object handed down, inside the one
// write, so the watch sees a single event and resourceVersion moves once.
type generationTracker struct {
	testing.ObjectTracker
	newScratch func() testing.ObjectTracker
}

func newGenerationTracker(s *runtime.Scheme) *generationTracker {
	conv := newTypeConverter()
	return &generationTracker{
		ObjectTracker: newManagedTracker(s, conv),
		newScratch:    func() testing.ObjectTracker { return newManagedTracker(s, conv) },
	}
}

var _ testing.ObjectTracker = (*generationTracker)(nil)

// Add seeds an object, as WithObjects does. A generation the fixture already
// carries is kept as fixture state, since a test that seeds one means it; only
// 0 becomes 1. Create, by contrast, always sets 1.
//
// It also validates seeded managedFields. The fake does that itself only when
// it built the tracker, and a custom tracker switches it off, so a malformed
// entry would otherwise be cleared silently instead of failing the build.
//
// obj is always one object: the fake's versionedTracker splits a seeded list
// into its items before calling Add.
func (t *generationTracker) Add(obj runtime.Object) error {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if err := managedfields.ValidateManagedFields(acc.GetManagedFields()); err != nil {
		return fmt.Errorf("invalid managedFields on %T: %w", obj, err)
	}
	if err := defaultGeneration(obj); err != nil {
		return err
	}
	return t.ObjectTracker.Add(obj)
}

func (t *generationTracker) Create(
	gvr schema.GroupVersionResource,
	obj runtime.Object,
	ns string,
	opts ...metav1.CreateOptions,
) error {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	// Always 1, whatever the client sent, as a real API server does. Add is the
	// seeding path and keeps a fixture's own value.
	acc.SetGeneration(1)
	return t.ObjectTracker.Create(gvr, obj, ns, opts...)
}

func (t *generationTracker) Update(
	gvr schema.GroupVersionResource,
	obj runtime.Object,
	ns string,
	opts ...metav1.UpdateOptions,
) error {
	if err := t.stampGeneration(gvr, obj, ns); err != nil {
		return err
	}
	return t.ObjectTracker.Update(gvr, obj, ns, opts...)
}

func (t *generationTracker) Patch(
	gvr schema.GroupVersionResource,
	obj runtime.Object,
	ns string,
	opts ...metav1.PatchOptions,
) error {
	if err := t.stampGeneration(gvr, obj, ns); err != nil {
		return err
	}
	return t.ObjectTracker.Patch(gvr, obj, ns, opts...)
}

// Apply receives the apply configuration, which is a fragment and so cannot be
// compared with the stored object. The merged result is only built further
// down, out of reach, so it is computed here on a scratch tracker seeded with
// the stored object, and the answer is written into the configuration's
// metadata.generation before the real apply runs.
func (t *generationTracker) Apply(
	gvr schema.GroupVersionResource,
	cfg runtime.Object,
	ns string,
	opts ...metav1.PatchOptions,
) error {
	acc, err := meta.Accessor(cfg)
	if err != nil {
		return err
	}
	stored, err := t.Get(gvr, ns, acc.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		acc.SetGeneration(1)
	case err != nil:
		return err
	default:
		gen, err := t.appliedGeneration(gvr, stored, cfg, ns, opts)
		if err != nil {
			return err
		}
		acc.SetGeneration(gen)
	}
	return t.ObjectTracker.Apply(gvr, cfg, ns, opts...)
}

func (t *generationTracker) appliedGeneration(
	gvr schema.GroupVersionResource,
	stored, cfg runtime.Object,
	ns string,
	opts []metav1.PatchOptions,
) (int64, error) {
	storedAcc, err := meta.Accessor(stored)
	if err != nil {
		return 0, err
	}
	scratch := t.newScratch()
	if err := scratch.Add(stored.DeepCopyObject()); err != nil {
		return 0, err
	}
	force := true
	dry := metav1.PatchOptions{Force: &force}
	if len(opts) > 0 {
		dry.FieldManager = opts[0].FieldManager
	}
	if err := scratch.Apply(gvr, cfg.DeepCopyObject(), ns, dry); err != nil {
		return 0, err
	}
	acc, err := meta.Accessor(cfg)
	if err != nil {
		return 0, err
	}
	merged, err := scratch.Get(gvr, ns, acc.GetName(), metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	same, err := sameContent(stored, merged)
	if err != nil {
		return 0, err
	}
	if same {
		return storedAcc.GetGeneration(), nil
	}
	return storedAcc.GetGeneration() + 1, nil
}

func defaultGeneration(obj runtime.Object) error {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if acc.GetGeneration() == 0 {
		acc.SetGeneration(1)
	}
	return nil
}

// stampGeneration sets obj's generation from the stored object: unchanged when
// the content is the same, one higher when it differs. Whatever generation the
// caller sent is discarded, as a real server does.
func (t *generationTracker) stampGeneration(
	gvr schema.GroupVersionResource,
	obj runtime.Object,
	ns string,
) error {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	stored, err := t.Get(gvr, ns, acc.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	storedAcc, err := meta.Accessor(stored)
	if err != nil {
		return err
	}
	same, err := sameContent(stored, obj)
	if err != nil {
		return err
	}
	gen := storedAcc.GetGeneration()
	// Beginning a deletion bumps too, once (registry store markAsDeleting): an
	// object with finalizers gets its deletionTimestamp through an ordinary
	// update that compares equal here.
	beginsDeletion := storedAcc.GetDeletionTimestamp() == nil &&
		acc.GetDeletionTimestamp() != nil
	if !same || beginsDeletion {
		gen++
	}
	acc.SetGeneration(gen)
	return nil
}

// sameContent reports whether a and b differ only in metadata and status, the
// two top-level fields that never advance generation. apiVersion and kind are
// dropped as well: a stored object and an incoming one routinely differ in
// whether TypeMeta is stamped, and that is not a change.
func sameContent(a, b runtime.Object) (bool, error) {
	ua, err := content(a)
	if err != nil {
		return false, err
	}
	ub, err := content(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(ua, ub), nil
}

func content(obj runtime.Object) (map[string]any, error) {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("convert %T to unstructured: %w", obj, err)
	}
	// Copy rather than delete: for an Unstructured, ToUnstructured returns the
	// object's own map, and for a kind the scheme has no Go type for the fake
	// hands the tracker the caller's object itself.
	out := make(map[string]any, len(u))
	for k, v := range u {
		switch k {
		case "metadata", "status", "apiVersion", "kind":
		default:
			out[k] = v
		}
	}
	return out, nil
}
