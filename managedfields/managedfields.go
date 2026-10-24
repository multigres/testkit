// SPDX-License-Identifier: Apache-2.0

package managedfields

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// ErrNoMatch is wrapped by the error RenameManager returns when no
// managedFields entry has the manager to rename. A caller sweeping objects that
// do not all carry that manager tells "nothing to rename" from a stale
// resourceVersion or an apiserver failure with errors.Is.
var ErrNoMatch = errors.New("no managedFields entry has that manager")

// Entry is one managedFields entry with its field set decoded.
type Entry struct {
	Manager string
	// Subresource is "" for the main resource, "status" and so on otherwise.
	Subresource string
	Operation   metav1.ManagedFieldsOperationType
	// Paths are fieldpath.Path.String() forms, sorted.
	Paths []string
}

// Entries decodes obj.GetManagedFields(), sorted by (Manager, Subresource,
// Operation) with Paths sorted.
//
// Entries that tie on all three (an Update entry per apiVersion, for one) fall
// back to their path lists, so the order never depends on the order the server
// happened to return them in.
func Entries(obj client.Object) ([]Entry, error) {
	var out []Entry
	for _, mf := range obj.GetManagedFields() {
		set := &fieldpath.Set{}
		if mf.FieldsV1 != nil {
			if err := set.FromJSON(mf.FieldsV1.GetRawReader()); err != nil {
				return nil, fmt.Errorf(
					"decode managedFields of %s (manager %q, subresource %q): %w",
					describe(obj),
					mf.Manager,
					mf.Subresource,
					err,
				)
			}
		}
		paths := []string{}
		set.Iterate(func(p fieldpath.Path) {
			paths = append(paths, p.String())
		})
		slices.Sort(paths)
		out = append(out, Entry{
			Manager:     mf.Manager,
			Subresource: mf.Subresource,
			Operation:   mf.Operation,
			Paths:       paths,
		})
	}
	slices.SortStableFunc(out, func(a, b Entry) int {
		return cmp.Or(
			cmp.Compare(a.Manager, b.Manager),
			cmp.Compare(a.Subresource, b.Subresource),
			cmp.Compare(string(a.Operation), string(b.Operation)),
			slices.Compare(a.Paths, b.Paths),
		)
	})
	return out, nil
}

// Managers returns the distinct manager names, sorted.
func Managers(obj client.Object) []string {
	seen := map[string]struct{}{}
	for _, mf := range obj.GetManagedFields() {
		seen[mf.Manager] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// Owners returns the sorted managers, on any subresource, that claim exactly
// path. A manager owning .spec.template does not own .spec.template.metadata.
//
// It panics if managedFields cannot be decoded. Returning nothing instead would
// make an ownership-absence assertion pass because the evidence was unreadable,
// and a real apiserver never emits a field set this package cannot decode.
func Owners(obj client.Object, path string) []string {
	entries, err := Entries(obj)
	if err != nil {
		panic(err)
	}
	seen := map[string]struct{}{}
	for _, e := range entries {
		if slices.Contains(e.Paths, path) {
			seen[e.Manager] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

type seedConfig struct {
	status bool
}

// SeedOption configures Seed.
type SeedOption func(*seedConfig)

// OnStatus applies against the status subresource.
//
// Seed applies the whole object, so the status manager is recorded for every
// path the apiserver keeps for that subresource, not only those under .status.
// Observed on a Pod: .metadata.labels and the server-defaulted .status.phase
// and .status.qosClass alongside the paths the test set. An assertion on a
// seeded manager should therefore name a distinctive path, and a count of its
// paths includes these.
func OnStatus() SeedOption {
	return func(c *seedConfig) { c.status = true }
}

// Seed server-side applies obj's current content under manager with
// client.ForceOwnership, so the object gains an Apply entry for manager with
// the field set obj carries. It applies a copy and leaves obj untouched.
//
// With OnStatus the field set recorded is wider than .status; see OnStatus.
//
// The server-populated identity fields (managedFields, resourceVersion, uid,
// generation, creationTimestamp) are stripped from the applied copy: an apply
// that carried a resourceVersion would become a precondition, and one that
// carried managedFields is rejected.
func Seed(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	manager string,
	opts ...SeedOption,
) error {
	var cfg seedConfig
	for _, o := range opts {
		o(&cfg)
	}

	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return fmt.Errorf("seed %s: %w", describe(obj), err)
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("seed %s: convert to unstructured: %w", describe(obj), err)
	}
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	u.SetManagedFields(nil)
	u.SetResourceVersion("")
	u.SetUID("")
	u.SetGeneration(0)
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
	if st, ok, _ := unstructured.NestedMap(u.Object, "status"); ok && len(st) == 0 {
		unstructured.RemoveNestedField(u.Object, "status")
	}

	ac := client.ApplyConfigurationFromUnstructured(u)
	if cfg.status {
		err = c.SubResource("status").
			Apply(ctx, ac, client.FieldOwner(manager), client.ForceOwnership)
	} else {
		err = c.Apply(ctx, ac, client.FieldOwner(manager), client.ForceOwnership)
	}
	if err != nil {
		return fmt.Errorf("seed %s as %q: %w", describe(obj), manager, err)
	}
	return nil
}

// jsonPatchOp is one RFC 6902 operation. A struct rather than a map so the
// marshalled patch has a fixed key order.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// RenameManager renames every managedFields entry of obj whose manager is from
// to to, with one JSON patch to the main resource: a test op on
// /metadata/resourceVersion first, then for each matching index a test on
// /metadata/managedFields/<i>/manager and a replace of it.
//
// The resourceVersion test is what makes the index addressing safe: indices
// are positions in the array as obj last saw it, so a stale obj must fail the
// patch rather than rename whatever now sits at those positions. The error is
// returned and the caller re-reads and retries. It is also an error for no
// entry to match, since a rename that renames nothing is a test that has
// misremembered who owns the object, and that error wraps ErrNoMatch.
//
// The patch response is written into obj, as client.Patch does, so obj holds
// the renamed object afterwards and its resourceVersion is current for a
// following call. A caller that wants the pre-rename object to compare against
// must pass a DeepCopy.
func RenameManager(ctx context.Context, c client.Client, obj client.Object, from, to string) error {
	ops := []jsonPatchOp{
		{Op: "test", Path: "/metadata/resourceVersion", Value: obj.GetResourceVersion()},
	}
	matched := 0
	for i, mf := range obj.GetManagedFields() {
		if mf.Manager != from {
			continue
		}
		matched++
		p := fmt.Sprintf("/metadata/managedFields/%d/manager", i)
		ops = append(ops,
			jsonPatchOp{Op: "test", Path: p, Value: from},
			jsonPatchOp{Op: "replace", Path: p, Value: to},
		)
	}
	if matched == 0 {
		return fmt.Errorf(
			"rename manager %q to %q on %s: %w (managers: %q)",
			from,
			to,
			describe(obj),
			ErrNoMatch,
			Managers(obj),
		)
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("rename manager on %s: %w", describe(obj), err)
	}
	if err := c.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, raw)); err != nil {
		return fmt.Errorf("rename manager %q to %q on %s: %w", from, to, describe(obj), err)
	}
	return nil
}

func describe(obj client.Object) string {
	name := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		name = ns + "/" + name
	}
	if gvk := obj.GetObjectKind().GroupVersionKind(); gvk.Kind != "" {
		return gvk.Kind + " " + name
	}
	return fmt.Sprintf("%T %s", obj, name)
}
