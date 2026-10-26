// SPDX-License-Identifier: Apache-2.0

package fakeapi

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// watchFunc is the Watch interceptor: it calls through to the caller's Watch
// interceptor if there is one, else the fake, and filters what comes back by
// the field selector in opts. controller-runtime's fake Watch hands the tracker
// only the namespace and drops the rest of the ListOptions, so without this a
// watch scoped to one object delivers every object's events.
func watchFunc(
	scheme *runtime.Scheme,
	indexes indexRegistry,
	next func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error),
) func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
	return func(
		ctx context.Context,
		c client.WithWatch,
		list client.ObjectList,
		opts ...client.ListOption,
	) (watch.Interface, error) {
		listOpts := client.ListOptions{}
		listOpts.ApplyOptions(opts)

		want, err := watchRequirements(scheme, indexes, list, listOpts)
		if err != nil {
			return nil, err
		}

		var upstream watch.Interface
		if next != nil {
			upstream, err = next(ctx, c, list, opts...)
		} else {
			upstream, err = c.Watch(ctx, list, opts...)
		}
		if err != nil || len(want) == 0 {
			return upstream, err
		}
		// The upstream watch is open before the seeding List, so an object
		// that enters the selector in between is delivered, and one deleted in
		// between is delivered as Deleted. One the List already saw can be
		// reported as Modified rather than Added, and one that entered and
		// left inside that window arrives as an Added and Deleted pair. An
		// object modified out of the selector inside the window is not
		// reported: neither the List nor the event shows it ever matched, so
		// it reads as a change made before the watch started, which the fake
		// never replays (it starts every watch from now, not from a
		// resourceVersion; see the package doc). A consumer that listed
		// earlier can keep such an object in its view.
		in, err := currentMembers(ctx, c, list, listOpts, want)
		if err != nil {
			upstream.Stop()
			return nil, err
		}
		return newFilteredWatch(ctx, upstream, want, in), nil
	}
}

// fieldReq is one equality requirement, evaluated as the fake's List does: the
// value must be among those the field's indexer yields for the object.
type fieldReq struct {
	fn    client.IndexerFunc
	value string
}

// watchRequirements returns the requirements a watch is restricted to. A field
// with no index known to fakeapi is refused, as the fake's List refuses a field
// with no registered index, and in the same shape (lower-cased, as lint
// requires), so a selector fails alike whichever call a test reaches it through.
func watchRequirements(
	scheme *runtime.Scheme,
	indexes indexRegistry,
	list client.ObjectList,
	opts client.ListOptions,
) ([]fieldReq, error) {
	if opts.FieldSelector == nil || opts.FieldSelector.Empty() {
		return nil, nil
	}
	gvk, err := apiutil.GVKForObject(list, scheme)
	if err != nil {
		return nil, err
	}
	gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")

	var want []fieldReq
	for _, req := range opts.FieldSelector.Requirements() {
		if req.Operator != selection.Equals && req.Operator != selection.DoubleEquals {
			return nil, fmt.Errorf(
				`field selector %s is not in one of the two supported forms "key==val" or "key=val"`,
				opts.FieldSelector,
			)
		}
		fn := indexes[gvk][req.Field]
		if fn == nil {
			return nil, fmt.Errorf("watch on GroupVersionKind %v specifies selector on field %s, "+
				"but fakeapi's watch filter knows no index for that field; WithFieldIndex "+
				"registers one usable by both List and Watch", gvk, req.Field)
		}
		want = append(want, fieldReq{fn: fn, value: req.Value})
	}
	return want, nil
}

// currentMembers lists the objects now matching want, so the filter knows which
// are already inside the selector when the first event arrives. Only the
// namespace and field selector are applied: the fake's Watch ignores labels, so
// an object a label selector excludes still produces events, and seeding it out
// of the set would mint an Added for an object that has existed all along.
func currentMembers(
	ctx context.Context,
	c client.WithWatch,
	list client.ObjectList,
	opts client.ListOptions,
	want []fieldReq,
) (map[types.NamespacedName]client.Object, error) {
	seed, ok := list.DeepCopyObject().(client.ObjectList)
	if !ok {
		return nil, fmt.Errorf("fakeapi: %T is not a client.ObjectList", list)
	}
	seedOpts := &client.ListOptions{Namespace: opts.Namespace, FieldSelector: opts.FieldSelector}
	if err := c.List(ctx, seed, seedOpts); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(seed)
	if err != nil {
		return nil, err
	}
	in := map[types.NamespacedName]client.Object{}
	for _, it := range items {
		if obj, ok := it.(client.Object); ok && matchesAll(want, obj) {
			in[client.ObjectKeyFromObject(obj)] = obj.DeepCopyObject().(client.Object)
		}
	}
	return in, nil
}

// filteredWatch delivers the events of the objects inside the selector, and
// reports an object that enters or leaves it as a real API server's watch cache
// does: entering is Added, leaving is Deleted carrying the previous object at
// the leaving event's resourceVersion. Events that carry no object metadata (an
// Error carrying a Status, say) pass unfiltered, since dropping them would hide
// a failed watch.
type filteredWatch struct {
	upstream watch.Interface
	want     []fieldReq
	in       map[types.NamespacedName]client.Object
	out      chan watch.Event
	stop     chan struct{}
	stopOnce sync.Once
}

func newFilteredWatch(
	ctx context.Context,
	upstream watch.Interface,
	want []fieldReq,
	in map[types.NamespacedName]client.Object,
) *filteredWatch {
	w := &filteredWatch{
		upstream: upstream,
		want:     want,
		in:       in,
		out:      make(chan watch.Event),
		stop:     make(chan struct{}),
	}
	go w.run(ctx)
	return w
}

func (w *filteredWatch) ResultChan() <-chan watch.Event { return w.out }

func (w *filteredWatch) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
		w.upstream.Stop()
	})
}

func (w *filteredWatch) run(ctx context.Context) {
	defer close(w.out)
	defer w.Stop()
	for {
		// select picks among ready arms at random, so a stop that is already
		// signalled must be checked on its own or it can lose to a ready event.
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case ev, ok := <-w.upstream.ResultChan():
			if !ok {
				return
			}
			ev, ok = w.filter(ev)
			if !ok {
				continue
			}
			select {
			case w.out <- ev:
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

// filter runs only on the watch's goroutine, so in needs no lock.
func (w *filteredWatch) filter(ev watch.Event) (watch.Event, bool) {
	obj, ok := ev.Object.(client.Object)
	if !ok {
		return ev, true
	}
	key := client.ObjectKeyFromObject(obj)
	prev, was := w.in[key]
	switch ev.Type {
	case watch.Added, watch.Modified:
		if matchesAll(w.want, obj) {
			w.in[key] = obj.DeepCopyObject().(client.Object)
			if was {
				ev.Type = watch.Modified
			} else {
				ev.Type = watch.Added
			}
			return ev, true
		}
		delete(w.in, key)
		if was {
			prev.SetResourceVersion(obj.GetResourceVersion())
			return watch.Event{Type: watch.Deleted, Object: prev}, true
		}
		return ev, false
	case watch.Deleted:
		delete(w.in, key)
		// A deletion inside the seeding window reaches here after the seeding
		// List no longer saw the object. A real server sends Deleted whenever
		// the deleted object matched, so decide from the object, not only from
		// membership.
		return ev, was || matchesAll(w.want, obj)
	default:
		return ev, true
	}
}

func matchesAll(want []fieldReq, obj client.Object) bool {
	for _, r := range want {
		if !slices.Contains(r.fn(obj), r.value) {
			return false
		}
	}
	return true
}
