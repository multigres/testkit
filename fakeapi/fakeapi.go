// SPDX-License-Identifier: Apache-2.0

package fakeapi

import (
	"cmp"
	"slices"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The field selector keys a real API server serves for every resource.
const (
	nameField      = "metadata.name"
	namespaceField = "metadata.namespace"
)

// Option configures NewClientBuilder.
type Option func(*options)

type options struct {
	interceptors interceptor.Funcs
	fieldIndexes []fieldIndex
}

type fieldIndex struct {
	obj   client.Object
	field string
	fn    client.IndexerFunc
}

// indexRegistry holds the field extractors fakeapi knows, per kind. The fake
// keeps its own copy of the indexes it was given, out of reach of an
// interceptor, so the Watch filter needs this one to evaluate a selector the
// way the fake's List does.
type indexRegistry map[schema.GroupVersionKind]map[string]client.IndexerFunc

func (r indexRegistry) add(gvk schema.GroupVersionKind, field string, fn client.IndexerFunc) {
	if r[gvk] == nil {
		r[gvk] = map[string]client.IndexerFunc{}
	}
	r[gvk][field] = fn
}

// WithInterceptors installs f as the client's interceptors, alongside fakeapi's
// own. Use it instead of calling WithInterceptorFuncs on the returned builder,
// which holds a single Funcs value and so would replace fakeapi's Watch filter
// and the Create and Update wrappers that copy the stored generation back onto
// an Unstructured or PartialObjectMetadata. An f.Watch is called for every
// watch fakeapi accepts, and its result is what the field selector filter
// wraps; an f.Create or f.Update runs inside fakeapi's wrapper.
func WithInterceptors(f interceptor.Funcs) Option {
	return func(o *options) { o.interceptors = f }
}

// WithFieldIndex registers an index on obj's kind for field, usable by both List
// and Watch field selectors. Use it rather than the builder's WithIndex for any
// field that is also watched: the fake keeps its indexes where fakeapi's Watch
// filter cannot see them, so a field registered with WithIndex works on List
// only and is refused on Watch. Registering metadata.name or metadata.namespace
// here makes NewClientBuilder panic, as those are indexed already.
func WithFieldIndex(obj client.Object, field string, fn client.IndexerFunc) Option {
	return func(o *options) {
		o.fieldIndexes = append(o.fieldIndexes, fieldIndex{obj: obj, field: field, fn: fn})
	}
}

// NewClientBuilder returns controller-runtime's own *fake.ClientBuilder, already
// configured with scheme, metadata.name and metadata.namespace indexes for every
// kind in scheme that has a List type when NewClientBuilder runs, and an object
// tracker that maintains
// metadata.generation. Callers chain WithObjects, WithStatusSubresource and the
// rest as usual. Do not call WithObjectTracker or WithTypeConverters on the
// result: the first replaces the generation tracker, and controller-runtime
// refuses the second alongside a custom tracker. Registering metadata.name or
// metadata.namespace again with WithIndex panics at that call, as
// controller-runtime does for any duplicate index; with [WithFieldIndex] it
// panics in NewClientBuilder.
//
// Watch honours field selectors on the fields fakeapi indexes, which
// controller-runtime's fake does not: it hands the tracker only the namespace.
// Those are metadata.name and metadata.namespace, plus any field registered with
// [WithFieldIndex], so use that rather than WithIndex for a field you also
// watch. A selector on any other field is refused on Watch, as List refuses a
// field with no index. Do not call WithInterceptorFuncs on the result, which
// would replace the Watch filter and the generation write-back; pass
// [WithInterceptors] instead.
//
// Generation on create is 1 whatever the client sent; objects seeded with
// WithObjects keep a non-zero generation they carry and get 1 only when it is 0.
func NewClientBuilder(scheme *runtime.Scheme, opts ...Option) *fake.ClientBuilder {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	indexes := indexRegistry{}
	funcs := o.interceptors
	funcs.Watch = watchFunc(scheme, indexes, funcs.Watch)
	funcs.Create = createFunc(funcs.Create)
	funcs.Update = updateFunc(funcs.Update)

	b := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjectTracker(newGenerationTracker(scheme)).
		WithInterceptorFuncs(funcs)

	name := func(o client.Object) []string { return []string{o.GetName()} }
	namespace := func(o client.Object) []string { return []string{o.GetNamespace()} }
	for _, gvk := range indexableKinds(scheme) {
		obj, err := scheme.New(gvk)
		if err != nil {
			panic(err)
		}
		// WithIndex resolves the kind from the object, which fails for partial
		// metadata (no TypeMeta) and for a Go type served in several versions.
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		b = b.WithIndex(obj, nameField, name).WithIndex(obj, namespaceField, namespace)
		indexes.add(gvk, nameField, name)
		indexes.add(gvk, namespaceField, namespace)
	}
	for _, fi := range o.fieldIndexes {
		gvk, err := apiutil.GVKForObject(fi.obj, scheme)
		if err != nil {
			panic(err)
		}
		b = b.WithIndex(fi.obj, fi.field, fi.fn)
		indexes.add(gvk, fi.field, fi.fn)
	}
	return b
}

// indexableKinds returns, sorted, the kinds that a list call can reach: those
// with a <Kind>List registered in the same group and version, and whose Go type
// is a client.Object. Options types, internal versions and the List types
// themselves have no List of their own, and the fake would never be asked to
// filter them.
//
// The order is fixed so that a duplicate-registration panic, should one ever
// happen, names the same kind on every run.
func indexableKinds(scheme *runtime.Scheme) []schema.GroupVersionKind {
	var out []schema.GroupVersionKind
	for gvk := range scheme.AllKnownTypes() {
		if !scheme.Recognizes(gvk.GroupVersion().WithKind(gvk.Kind + "List")) {
			continue
		}
		obj, err := scheme.New(gvk)
		if err != nil {
			continue
		}
		if _, ok := obj.(client.Object); !ok {
			continue
		}
		out = append(out, gvk)
	}
	slices.SortFunc(out, func(a, b schema.GroupVersionKind) int {
		return cmp.Or(
			cmp.Compare(a.Group, b.Group),
			cmp.Compare(a.Version, b.Version),
			cmp.Compare(a.Kind, b.Kind),
		)
	})
	return out
}
