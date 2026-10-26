// SPDX-License-Identifier: Apache-2.0

package fakeapi_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/multigres/testkit/assert"
	"github.com/multigres/testkit/fakeapi"
)

var widgetGV = schema.GroupVersion{Group: "example.test", Version: "v1"}

type WidgetSpec struct {
	Image    string `json:"image,omitempty"`
	Replicas int    `json:"replicas,omitempty"`
	Paused   bool   `json:"paused,omitempty"`
}

type WidgetStatus struct {
	Ready int `json:"ready,omitempty"`
}

// Widget is a CRD-shaped kind: spec and status, a List type, and no typed
// apply configuration, which is what makes server-side apply need the deduced
// type converter.
type Widget struct {
	metav1.TypeMeta   `             json:",inline"`
	metav1.ObjectMeta `             json:"metadata,omitempty"`
	Spec              WidgetSpec   `json:"spec,omitempty"`
	Status            WidgetStatus `json:"status,omitempty"`
}

func (w *Widget) DeepCopyObject() runtime.Object {
	out := *w
	w.DeepCopyInto(&out.ObjectMeta)
	return &out
}

type WidgetList struct {
	metav1.TypeMeta `         json:",inline"`
	metav1.ListMeta `         json:"metadata,omitempty"`
	Items           []Widget `json:"items"`
}

func (l *WidgetList) DeepCopyObject() runtime.Object {
	out := *l
	l.DeepCopyInto(&out.ListMeta)
	out.Items = make([]Widget, len(l.Items))
	for i := range l.Items {
		out.Items[i] = *l.Items[i].DeepCopyObject().(*Widget)
	}
	return &out
}

// Orphan is registered without a List type.
type Orphan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

func (o *Orphan) DeepCopyObject() runtime.Object {
	out := *o
	o.DeepCopyInto(&out.ObjectMeta)
	return &out
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	assert.NewAborting(t).NoError(corev1.AddToScheme(s))
	s.AddKnownTypes(widgetGV, &Widget{}, &WidgetList{})
	metav1.AddToGroupVersion(s, widgetGV)
	return s
}

func widget(name string) *Widget {
	return &Widget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       WidgetSpec{Image: "v1", Replicas: 1},
	}
}

func key(name string) client.ObjectKey { return client.ObjectKey{Namespace: "ns", Name: name} }

// generationOf reads through the client, as a consumer would.
func generationOf(t *testing.T, c client.Client, name string) int64 {
	t.Helper()
	got := &Widget{}
	assert.NewAborting(t).NoError(c.Get(t.Context(), key(name), got))
	return got.Generation
}

func newClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fakeapi.NewClientBuilder(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&Widget{}).
		Build()
}

func TestFieldSelectorMetadataName(t *testing.T) {
	c := assert.NewAborting(t)
	cl := newClient(t, widget("a"), widget("b"))

	list := &WidgetList{}
	c.NoError(cl.List(t.Context(), list, client.MatchingFields{"metadata.name": "b"}))
	c.Len(list.Items, 1)
	c.Eq("b", list.Items[0].Name)

	cms := &corev1.ConfigMapList{}
	c.NoError(cl.List(t.Context(), cms, client.MatchingFields{"metadata.name": "none"}))
	c.Len(cms.Items, 0)

	// A watch for one name sees that object's events and not its sibling's. The
	// sibling is written first, so an unfiltered watch would deliver it first.
	wi, err := cl.Watch(t.Context(), &WidgetList{}, client.MatchingFields{"metadata.name": "c"})
	c.NoError(err)
	defer wi.Stop()
	c.NoError(cl.Create(t.Context(), widget("d")))
	c.NoError(cl.Create(t.Context(), widget("c")))
	c.Eq("c", nextWidget(t, wi, watch.Added).Name)

	d := &Widget{}
	c.NoError(cl.Get(t.Context(), key("d"), d))
	d.Spec.Replicas = 4
	c.NoError(cl.Update(t.Context(), d))
	cc := &Widget{}
	c.NoError(cl.Get(t.Context(), key("c"), cc))
	cc.Spec.Replicas = 4
	c.NoError(cl.Update(t.Context(), cc))
	c.Eq("c", nextWidget(t, wi, watch.Modified).Name)
}

// nextWidget returns the next event's Widget, failing if none arrives or it is
// not of type typ.
func nextWidget(t *testing.T, wi watch.Interface, typ watch.EventType) *Widget {
	t.Helper()
	c := assert.NewAborting(t)
	select {
	case ev, ok := <-wi.ResultChan():
		c.True(ok, "watch closed")
		c.Eq(typ, ev.Type)
		w, isWidget := ev.Object.(*Widget)
		c.True(isWidget, "event object is %T", ev.Object)
		return w
	case <-time.After(5 * time.Second):
		c.Fail("no watch event")
	}
	return nil
}

func TestWatchRefusesOtherFieldSelectors(t *testing.T) {
	c := assert.NewAborting(t)
	cl := newClient(t)

	_, err := cl.Watch(t.Context(), &WidgetList{}, client.MatchingFields{"spec.image": "v1"})
	c.ErrorContains(err, "specifies selector on field spec.image")
	c.ErrorContains(err, "WithFieldIndex")

	not := client.MatchingFieldsSelector{
		Selector: fields.OneTermNotEqualSelector("metadata.name", "a"),
	}
	_, err = cl.Watch(t.Context(), &WidgetList{}, not)
	c.ErrorContains(err, "not in one of the two supported forms")
}

func TestWatchFilterStopsItsGoroutine(t *testing.T) {
	// synctest.Test fails the test if any goroutine started inside it is still
	// running when the function returns.
	synctest.Test(t, func(t *testing.T) {
		c := assert.NewAborting(t)
		cl := newClient(t)

		stopped, err := cl.Watch(
			t.Context(),
			&WidgetList{},
			client.MatchingFields{"metadata.name": "a"},
		)
		c.NoError(err)
		stopped.Stop()
		_, open := <-stopped.ResultChan()
		c.False(open, "result channel should close after Stop")

		// A goroutine parked on a send nobody will receive is the case a Stop
		// without a send-side arm would leak.
		blocked, err := cl.Watch(
			t.Context(),
			&WidgetList{},
			client.MatchingFields{"metadata.name": "a"},
		)
		c.NoError(err)
		c.NoError(cl.Create(t.Context(), widget("a")))
		synctest.Wait()
		blocked.Stop()
		synctest.Wait()
		select {
		case ev, open := <-blocked.ResultChan():
			c.False(open, "event %v still pending after Stop", ev.Type)
		default:
			c.Fail("result channel still open after Stop")
		}
	})
}

func TestWatchFilterEndsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := assert.NewAborting(t)
		cl := newClient(t)
		ctx, cancel := context.WithCancel(t.Context())
		wi, err := cl.Watch(ctx, &WidgetList{}, client.MatchingFields{"metadata.name": "a"})
		c.NoError(err)
		defer wi.Stop()
		cancel()
		synctest.Wait()
		select {
		case ev, open := <-wi.ResultChan():
			c.False(open, "event %v delivered after cancel", ev.Type)
		default:
			c.Fail("result channel still open after cancel")
		}
	})
}

func TestWatchFieldIndex(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	image := func(o client.Object) []string { return []string{o.(*Widget).Spec.Image} }
	cl := fakeapi.NewClientBuilder(newScheme(t), fakeapi.WithFieldIndex(&Widget{}, "spec.image", image)).
		Build()

	wi, err := cl.Watch(ctx, &WidgetList{}, client.MatchingFields{"spec.image": "v2"})
	c.NoError(err)
	defer wi.Stop()
	v1, v2 := widget("one"), widget("two")
	v2.Spec.Image = "v2"
	c.NoError(cl.Create(ctx, v1))
	c.NoError(cl.Create(ctx, v2))

	list := &WidgetList{}
	c.NoError(cl.List(ctx, list, client.MatchingFields{"spec.image": "v2"}))
	c.Len(list.Items, 1)
	c.Eq("two", list.Items[0].Name)
	c.Eq("two", nextWidget(t, wi, watch.Added).Name)
}

// An object that enters or leaves a selector on a mutable indexed field is
// reported as Added or Deleted, as a real server's watch cache does, including
// one that was already inside the selector when the watch opened.
func TestWatchFieldIndexTransitions(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	image := func(o client.Object) []string { return []string{o.(*Widget).Spec.Image} }
	cl := fakeapi.NewClientBuilder(newScheme(t), fakeapi.WithFieldIndex(&Widget{}, "spec.image", image)).
		Build()
	setImage := func(name, img string) string {
		w := &Widget{}
		c.NoError(cl.Get(ctx, key(name), w))
		w.Spec.Image = img
		c.NoError(cl.Update(ctx, w))
		return w.ResourceVersion
	}

	pre := widget("pre")
	pre.Spec.Image = "v2"
	c.NoError(cl.Create(ctx, pre))
	c.NoError(cl.Create(ctx, widget("never")))

	wi, err := cl.Watch(ctx, &WidgetList{}, client.MatchingFields{"spec.image": "v2"})
	c.NoError(err)
	defer wi.Stop()

	c.NoError(cl.Create(ctx, widget("a")))
	setImage("a", "v2")
	leaveRV := setImage("a", "v3")
	setImage("pre", "v3")
	c.NoError(cl.Delete(ctx, widget("never")))
	end := widget("z")
	end.Spec.Image = "v2"
	c.NoError(cl.Create(ctx, end))

	added := nextWidget(t, wi, watch.Added)
	c.Eq("a", added.Name)
	added.Spec.Image = "mutated"
	left := nextWidget(t, wi, watch.Deleted)
	c.Eq("a", left.Name)
	c.Eq("v2", left.Spec.Image)
	c.Eq(leaveRV, left.ResourceVersion)
	c.Eq("pre", nextWidget(t, wi, watch.Deleted).Name)
	c.Eq("z", nextWidget(t, wi, watch.Added).Name)
}

// The fake's Watch ignores label selectors, so an object a label selector
// excludes still produces events; it must not be reported as newly entering.
func TestWatchFieldIndexIgnoresLabelsWhenSeeding(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	image := func(o client.Object) []string { return []string{o.(*Widget).Spec.Image} }
	cl := fakeapi.NewClientBuilder(newScheme(t), fakeapi.WithFieldIndex(&Widget{}, "spec.image", image)).
		Build()
	o := widget("o")
	o.Spec.Image = "v2"
	o.Labels = map[string]string{"x": "other"}
	c.NoError(cl.Create(ctx, o))

	wi, err := cl.Watch(ctx, &WidgetList{},
		client.MatchingFields{"spec.image": "v2"}, client.MatchingLabels{"x": "want"})
	c.NoError(err)
	defer wi.Stop()

	got := &Widget{}
	c.NoError(cl.Get(ctx, key("o"), got))
	got.Spec.Replicas = 5
	c.NoError(cl.Update(ctx, got))
	c.Eq("o", nextWidget(t, wi, watch.Modified).Name)
}

// The probe for a kind fakeapi never indexed: an unstructured list of a GVK
// that is not in the scheme. List and Watch must refuse it alike.
func TestListAndWatchAgreeOnUnregisteredKinds(t *testing.T) {
	c := assert.NewAborting(t)
	cl := newClient(t)
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(
		schema.GroupVersionKind{Group: "other.test", Version: "v1", Kind: "ThingList"},
	)
	sel := client.MatchingFields{"metadata.name": "x"}

	c.Error(cl.List(t.Context(), list, sel))
	_, err := cl.Watch(t.Context(), list, sel)
	c.Error(err)
}

func TestWithInterceptorsComposeWithWatchFilter(t *testing.T) {
	c := assert.NewAborting(t)
	var gets, watches int
	cl := fakeapi.NewClientBuilder(newScheme(t), fakeapi.WithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			gets++
			return cl.Get(ctx, k, o, opts...)
		},
		Watch: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
			watches++
			return cl.Watch(ctx, l, opts...)
		},
	})).Build()

	wi, err := cl.Watch(t.Context(), &WidgetList{}, client.MatchingFields{"metadata.name": "a"})
	c.NoError(err)
	defer wi.Stop()
	c.NoError(cl.Create(t.Context(), widget("b")))
	c.NoError(cl.Create(t.Context(), widget("a")))
	c.Eq("a", nextWidget(t, wi, watch.Added).Name)
	c.NoError(cl.Get(t.Context(), key("a"), &Widget{}))

	c.Eq(1, gets)
	c.Eq(1, watches)
}

func TestFieldSelectorMetadataNamespace(t *testing.T) {
	c := assert.NewAborting(t)
	other := widget("a")
	other.Namespace = "other"
	cl := newClient(t, widget("a"), other)

	list := &WidgetList{}
	c.NoError(cl.List(t.Context(), list, client.MatchingFields{"metadata.namespace": "other"}))
	c.Len(list.Items, 1)
	c.Eq("other", list.Items[0].Namespace)

	wi, err := cl.Watch(
		t.Context(),
		&WidgetList{},
		client.MatchingFields{"metadata.namespace": "other"},
	)
	c.NoError(err)
	defer wi.Stop()
	c.NoError(cl.Create(t.Context(), widget("x")))
	created := widget("y")
	created.Namespace = "other"
	c.NoError(cl.Create(t.Context(), created))
	c.Eq("y", nextWidget(t, wi, watch.Added).Name)
}

func TestBuildAcceptsRealSchemes(t *testing.T) {
	clientGo := func() *runtime.Scheme {
		s := runtime.NewScheme()
		assert.NewAborting(t).NoError(clientgoscheme.AddToScheme(s))
		return s
	}
	cases := map[string]func() *runtime.Scheme{
		"client-go": clientGo,
		// PartialObjectMetadata has a List and no TypeMeta on a fresh instance.
		"with partial metadata": func() *runtime.Scheme {
			s := clientGo()
			assert.NewAborting(t).NoError(metav1.AddMetaToScheme(s))
			return s
		},
		// One Go type served in two versions, each with its List.
		"one type in two versions": func() *runtime.Scheme {
			s := newScheme(t)
			v2 := schema.GroupVersion{Group: widgetGV.Group, Version: "v2"}
			s.AddKnownTypes(v2, &Widget{}, &WidgetList{})
			return s
		},
		// A kind with no List, registered under two names.
		"aliased kind without a list": func() *runtime.Scheme {
			s := newScheme(t)
			s.AddKnownTypes(widgetGV, &Orphan{})
			s.AddKnownTypeWithName(widgetGV.WithKind("Alias"), &Orphan{})
			return s
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			s := build()
			assert.NewAborting(t).NotPanics(func() { fakeapi.NewClientBuilder(s).Build() })
		})
	}
}

func TestCreateSetsGenerationOne(t *testing.T) {
	c := assert.NewAborting(t)
	cl := newClient(t, widget("seeded"))
	c.Eq(int64(1), generationOf(t, cl, "seeded"))

	c.NoError(cl.Create(t.Context(), widget("made")))
	c.Eq(int64(1), generationOf(t, cl, "made"))
}

func TestSpecChangeBumpsGeneration(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name string
		edit func(t *testing.T, cl client.Client, w *Widget)
	}{
		{"Update", func(t *testing.T, cl client.Client, w *Widget) {
			w.Spec.Replicas = 2
			assert.NewAborting(t).NoError(cl.Update(ctx, w))
		}},
		{"MergePatch", func(t *testing.T, cl client.Client, w *Widget) {
			p := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":2}}`))
			assert.NewAborting(t).NoError(cl.Patch(ctx, w, p))
		}},
		{"JSONPatch", func(t *testing.T, cl client.Client, w *Widget) {
			p := client.RawPatch(
				types.JSONPatchType,
				[]byte(`[{"op":"replace","path":"/spec/replicas","value":2}]`),
			)
			assert.NewAborting(t).NoError(cl.Patch(ctx, w, p))
		}},
		{"Apply", func(t *testing.T, cl client.Client, w *Widget) {
			u := applyConfig(w.Name, 2)
			assert.NewAborting(t).
				NoError(cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("m"), client.ForceOwnership))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			cl := newClient(t, widget("w"))
			w := &Widget{}
			c.NoError(cl.Get(ctx, key("w"), w))
			c.Eq(int64(1), w.Generation)

			tc.edit(t, cl, w)
			c.Eq(int64(2), generationOf(t, cl, "w"))

			// Writing the same spec again is not a change.
			again := &Widget{}
			c.NoError(cl.Get(ctx, key("w"), again))
			tc.edit(t, cl, again)
			c.Eq(int64(2), generationOf(t, cl, "w"))
		})
	}
}

func applyConfig(name string, replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": widgetGV.String(),
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": "ns"},
		"spec":       map[string]any{"image": "v1", "replicas": replicas},
	}}
}

func TestStatusOnlyWriteKeepsGeneration(t *testing.T) {
	ctx := t.Context()
	t.Run("Update", func(t *testing.T) {
		c := assert.NewAborting(t)
		cl := newClient(t, widget("w"))
		w := &Widget{}
		c.NoError(cl.Get(ctx, key("w"), w))
		w.Status.Ready = 1
		c.NoError(cl.Status().Update(ctx, w))

		c.Eq(int64(1), generationOf(t, cl, "w"))
		got := &Widget{}
		c.NoError(cl.Get(ctx, key("w"), got))
		c.Eq(1, got.Status.Ready)
	})
	t.Run("Patch", func(t *testing.T) {
		c := assert.NewAborting(t)
		cl := newClient(t, widget("w"))
		w := &Widget{}
		c.NoError(cl.Get(ctx, key("w"), w))
		p := client.RawPatch(types.MergePatchType, []byte(`{"status":{"ready":2}}`))
		c.NoError(cl.Status().Patch(ctx, w, p))

		c.Eq(int64(1), generationOf(t, cl, "w"))
		got := &Widget{}
		c.NoError(cl.Get(ctx, key("w"), got))
		c.Eq(2, got.Status.Ready)
	})
}

func TestMetadataOnlyWriteKeepsGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t, widget("w"))
	w := &Widget{}
	c.NoError(cl.Get(ctx, key("w"), w))

	w.Labels = map[string]string{"a": "b"}
	w.Annotations = map[string]string{"c": "d"}
	w.Finalizers = []string{"example.test/hold"}
	c.NoError(cl.Update(ctx, w))
	c.Eq(int64(1), generationOf(t, cl, "w"))

	p := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"e":"f"}}}`))
	c.NoError(cl.Patch(ctx, w, p))
	c.Eq(int64(1), generationOf(t, cl, "w"))
}

func TestClientSentGenerationIsIgnored(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t, widget("w"))
	w := &Widget{}
	c.NoError(cl.Get(ctx, key("w"), w))

	w.Generation = 42
	w.Labels = map[string]string{"a": "b"}
	c.NoError(cl.Update(ctx, w))
	c.Eq(int64(1), generationOf(t, cl, "w"))

	w.Generation = 0
	w.Spec.Replicas = 5
	c.NoError(cl.Update(ctx, w))
	c.Eq(int64(2), generationOf(t, cl, "w"))

	w.Generation = 99
	w.Spec.Replicas = 6
	c.NoError(cl.Update(ctx, w))
	c.Eq(int64(3), generationOf(t, cl, "w"))
}

func TestBumpIsOneWatchEvent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t, widget("w"))

	w := &Widget{}
	c.NoError(cl.Get(ctx, key("w"), w))
	wi, err := cl.Watch(ctx, &WidgetList{}, client.InNamespace("ns"))
	c.NoError(err)
	defer wi.Stop()

	w.Spec.Replicas = 2
	c.NoError(cl.Update(ctx, w))

	select {
	case ev := <-wi.ResultChan():
		c.Eq(watch.Modified, ev.Type)
		got, ok := ev.Object.(*Widget)
		c.True(ok, "event object is %T", ev.Object)
		c.Eq(int64(2), got.Generation)
	case <-time.After(5 * time.Second):
		c.Fail("no event for the update")
	}
	select {
	case ev := <-wi.ResultChan():
		c.Fail("unexpected second event", ev.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestApplyStillWorks(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t)

	c.NoError(
		cl.Apply(
			ctx,
			client.ApplyConfigurationFromUnstructured(applyConfig("w", 1)),
			client.FieldOwner("m"),
		),
	)
	got := &Widget{}
	c.NoError(cl.Get(ctx, key("w"), got))
	c.Eq(1, got.Spec.Replicas)
	c.Eq("v1", got.Spec.Image)
	c.Eq(int64(1), got.Generation)

	c.NoError(
		cl.Apply(
			ctx,
			client.ApplyConfigurationFromUnstructured(applyConfig("w", 3)),
			client.FieldOwner("m"),
		),
	)
	c.NoError(cl.Get(ctx, key("w"), got))
	c.Eq(3, got.Spec.Replicas)
	c.Eq(int64(2), got.Generation)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Data:       map[string]string{"k": "v"},
	}
	c.NoError(cl.Create(ctx, cm))
	c.NoError(cl.Get(ctx, client.ObjectKeyFromObject(cm), cm))
}

func TestApplyFromSecondManagerDoesNotConflictOnGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t)

	c.NoError(
		cl.Apply(
			ctx,
			client.ApplyConfigurationFromUnstructured(applyConfig("w", 1)),
			client.FieldOwner("a"),
		),
	)

	second := applyConfig("w", 1)
	second.Object["spec"] = map[string]any{"image": "v1", "paused": true}
	c.NoError(
		cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(second), client.FieldOwner("b")),
	)
	c.Eq(int64(2), generationOf(t, cl, "w"))
}

// TestApplyManagersDoNotConflictOnGeneration pins that injecting generation into
// an apply configuration does not make a second field manager's unforced apply,
// or the first manager's later one, conflict on metadata.generation.
func TestApplyManagersDoNotConflictOnGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t)

	apply := func(owner string, spec map[string]any) {
		t.Helper()
		u := applyConfig("w", 1)
		u.Object["spec"] = spec
		c.NoError(
			cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(owner)),
		)
	}

	apply("a", map[string]any{"replicas": int64(1)})
	c.Eq(int64(1), generationOf(t, cl, "w"))

	apply("b", map[string]any{"paused": true})
	c.Eq(int64(2), generationOf(t, cl, "w"))

	apply("a", map[string]any{"replicas": int64(3)})
	c.Eq(int64(3), generationOf(t, cl, "w"))
}

func TestCreateIgnoresClientSentGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	cl := newClient(t)

	w := widget("w")
	w.Generation = 7
	c.NoError(cl.Create(t.Context(), w))
	c.Eq(int64(1), generationOf(t, cl, "w"))
}

func TestSeededGenerationIsKept(t *testing.T) {
	c := assert.NewAborting(t)
	w := widget("w")
	w.Generation = 7
	cl := newClient(t, w)
	c.Eq(int64(7), generationOf(t, cl, "w"))

	got := &Widget{}
	c.NoError(cl.Get(t.Context(), key("w"), got))
	got.Spec.Replicas = 9
	c.NoError(cl.Update(t.Context(), got))
	c.Eq(int64(8), generationOf(t, cl, "w"))
}

func TestDeletionWithFinalizerBumpsGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	held := widget("w")
	held.Finalizers = []string{"example.test/hold"}
	cl := newClient(t, held)
	c.Eq(int64(1), generationOf(t, cl, "w"))

	wi, err := cl.Watch(ctx, &WidgetList{}, client.InNamespace("ns"))
	c.NoError(err)
	defer wi.Stop()

	c.NoError(cl.Delete(ctx, held))
	got := &Widget{}
	c.NoError(cl.Get(ctx, key("w"), got))
	c.NotZero(got.DeletionTimestamp)
	c.Eq(int64(2), got.Generation)
	c.Eq(int64(2), nextWidget(t, wi, watch.Modified).Generation)

	// Deleting again does not restart a deletion already under way.
	c.NoError(cl.Delete(ctx, got))
	c.Eq(int64(2), generationOf(t, cl, "w"))
}

func TestSeededMalformedManagedFieldsFailBuild(t *testing.T) {
	c := assert.NewAborting(t)
	w := widget("w")
	w.ManagedFields = []metav1.ManagedFieldsEntry{{
		Manager:    "m",
		Operation:  metav1.ManagedFieldsOperationApply,
		APIVersion: widgetGV.String(),
		FieldsType: "FieldsV1",
		FieldsV1:   metav1.NewFieldsV1(`not json`),
	}}
	c.Panics(func() { newClient(t, w) })
}

func BenchmarkApplyToExisting(b *testing.B) {
	s := runtime.NewScheme()
	s.AddKnownTypes(widgetGV, &Widget{}, &WidgetList{})
	metav1.AddToGroupVersion(s, widgetGV)
	cl := fakeapi.NewClientBuilder(s).WithObjects(widget("w")).Build()
	cfg := client.ApplyConfigurationFromUnstructured(applyConfig("w", 2))
	b.ReportAllocs()
	for b.Loop() {
		if err := cl.Apply(
			b.Context(),
			cfg,
			client.FieldOwner("m"),
			client.ForceOwnership,
		); err != nil {
			b.Fatal(err)
		}
	}
}

// TestUnstructuredOfUnregisteredKind drives every generation-tracked write with
// an Unstructured of a kind the scheme has no Go type for. The fake hands such
// an object to the tracker as is, so the generation compare must not strip the
// caller's own map: doing so loses the name, and the write fails NotFound.
func TestUnstructuredOfUnregisteredKind(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t)
	gvk := schema.GroupVersionKind{Group: "other.test", Version: "v1", Kind: "Thing"}
	thing := func() *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		return u
	}

	u := thing()
	u.SetName("t")
	u.SetNamespace("ns")
	u.SetFinalizers([]string{"example.test/hold"})
	c.NoError(unstructured.SetNestedField(u.Object, "a", "spec", "image"))
	c.NoError(cl.Create(ctx, u))
	c.Eq(int64(1), u.GetGeneration())

	c.NoError(unstructured.SetNestedField(u.Object, "b", "spec", "image"))
	c.NoError(cl.Update(ctx, u))
	c.Eq("t", u.GetName(), "Update kept the caller's name")
	c.Eq(int64(2), u.GetGeneration())
	kept := []string{"example.test/hold"}
	c.EqDeep(kept, u.GetFinalizers(), "Update kept the caller's finalizers")

	patch := []byte(`{"spec":{"image":"c"}}`)
	c.NoError(cl.Patch(ctx, u, client.RawPatch(types.MergePatchType, patch)))
	c.Eq(int64(3), u.GetGeneration())

	c.NoError(cl.Delete(ctx, u))
	got := thing()
	c.NoError(cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "t"}, got))
	c.NotZero(got.GetDeletionTimestamp())
	c.Eq(int64(4), got.GetGeneration())
}

// TestUnstructuredOfKnownKindSeesItsGeneration drives Create, Update and Patch
// with an Unstructured of a kind the scheme types. The fake converts such an
// object to a typed copy before the tracker, so the generation the tracker sets
// has to reach the caller's object some other way, as resourceVersion does.
func TestUnstructuredOfKnownKindSeesItsGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t)
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(widgetGV.WithKind("Widget"))
	u.SetName("w")
	u.SetNamespace("ns")
	c.NoError(unstructured.SetNestedField(u.Object, "a", "spec", "image"))

	c.NoError(cl.Create(ctx, u))
	c.Eq(int64(1), u.GetGeneration(), "Create")

	c.NoError(unstructured.SetNestedField(u.Object, "b", "spec", "image"))
	c.NoError(cl.Update(ctx, u))
	c.Eq(int64(2), u.GetGeneration(), "Update")

	patch := []byte(`{"spec":{"image":"c"}}`)
	c.NoError(cl.Patch(ctx, u, client.RawPatch(types.MergePatchType, patch)))
	c.Eq(int64(3), u.GetGeneration(), "Patch")
	c.Eq(int64(3), generationOf(t, cl, "w"))
}

// TestWatchDeliversADeleteDuringItsSeedingList deletes a matching object after
// the upstream watch is open and before fakeapi's seeding List, the one window
// where the object is in neither the seeded set nor yet gone from the stream.
// A real server sends Deleted whenever the deleted object matched.
func TestWatchDeliversADeleteDuringItsSeedingList(t *testing.T) {
	c := assert.NewAborting(t)
	w := widget("a")
	cl := fakeapi.NewClientBuilder(newScheme(t), fakeapi.WithInterceptors(interceptor.Funcs{
		Watch: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
			up, err := cl.Watch(ctx, l, opts...)
			if err != nil {
				return nil, err
			}
			return up, cl.Delete(ctx, w)
		},
	})).WithObjects(w).Build()

	wi, err := cl.Watch(t.Context(), &WidgetList{}, client.MatchingFields{"metadata.name": "a"})
	c.NoError(err)
	defer wi.Stop()
	c.Eq("a", nextWidget(t, wi, watch.Deleted).Name)
}

// TestWriteBackToleratesWritesThatStoreNothing covers the two successful writes
// after which the object is not in the store: an Update that removes the last
// finalizer from an object being deleted, and a dry-run Create. Reading the
// generation back must not turn either into an error.
func TestWriteBackToleratesWritesThatStoreNothing(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	cl := newClient(t)
	newThing := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(widgetGV.WithKind("Widget"))
		u.SetName(name)
		u.SetNamespace("ns")
		return u
	}

	u := newThing("held")
	u.SetFinalizers([]string{"example.test/hold"})
	c.NoError(cl.Create(ctx, u))
	c.NoError(cl.Delete(ctx, u))
	c.NoError(cl.Get(ctx, key("held"), u))
	u.SetFinalizers(nil)
	c.NoError(cl.Update(ctx, u), "an Update that finalizes a deletion succeeds")
	c.True(apierrors.IsNotFound(cl.Get(ctx, key("held"), newThing("held"))), "the object is gone")

	dry := newThing("dry")
	c.NoError(cl.Create(ctx, dry, client.DryRunAll), "a dry-run Create succeeds")
	c.True(apierrors.IsNotFound(cl.Get(ctx, key("dry"), newThing("dry"))), "nothing was stored")
}

// TestDryRunWritesLeaveTheCallersGeneration pins that a dry-run write reports
// what controller-runtime's fake reports: the generation the caller sent. The
// fake neither refuses a dry-run Create of a stored name nor bumps on a
// dry-run Update, so reading the stored object back would hand the caller
// another object's generation in the first case and a stale one in the second.
func TestDryRunWritesLeaveTheCallersGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := t.Context()
	stored := widget("w")
	stored.Generation = 5
	cl := newClient(t, stored)
	thing := func() *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(widgetGV.WithKind("Widget"))
		u.SetName("w")
		u.SetNamespace("ns")
		return u
	}

	created := thing()
	c.NoError(cl.Create(ctx, created, client.DryRunAll))
	c.Eq(int64(0), created.GetGeneration(), "dry-run Create")

	updated := thing()
	c.NoError(cl.Get(ctx, key("w"), updated))
	updated.SetGeneration(7)
	c.NoError(unstructured.SetNestedField(updated.Object, "b", "spec", "image"))
	c.NoError(cl.Update(ctx, updated, client.DryRunAll))
	c.Eq(int64(7), updated.GetGeneration(), "dry-run Update")
	c.Eq(int64(5), generationOf(t, cl, "w"))
}
