// SPDX-License-Identifier: Apache-2.0

package managedfields_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/multigres/testkit/managedfields"
)

var counter atomic.Int64

func newNS(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("mf-%d", counter.Add(1))},
	}
	if err := k8s.Create(t.Context(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

func newPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{"a": "b"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "c", Image: "example.invalid/img:1"},
		}},
	}
}

func mustEntries(t *testing.T, obj client.Object) []managedfields.Entry {
	t.Helper()
	es, err := managedfields.Entries(obj)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	return es
}

func find(es []managedfields.Entry, manager, sub string) *managedfields.Entry {
	for i := range es {
		if es[i].Manager == manager && es[i].Subresource == sub {
			return &es[i]
		}
	}
	return nil
}

func TestEntriesDecodesAnApplyAndAStatusSubresource(t *testing.T) {
	ctx := t.Context()
	pod := newPod(newNS(t), "p")
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := managedfields.Seed(ctx, k8s, pod, "applier"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}

	es := mustEntries(t, pod)
	main := find(es, "applier", "")
	if main == nil || main.Operation != metav1.ManagedFieldsOperationApply {
		t.Fatalf("no main Apply entry for applier in %+v", es)
	}
	if !slices.Contains(main.Paths, `.metadata.labels.a`) {
		t.Errorf("paths %q lack .metadata.labels.a", main.Paths)
	}
	var sawStatus bool
	for _, e := range es {
		if e.Subresource == "status" {
			sawStatus = true
			if !slices.Contains(e.Paths, ".status.phase") {
				t.Errorf("status entry paths %q lack .status.phase", e.Paths)
			}
		}
	}
	if !sawStatus {
		t.Errorf("no entry with subresource status in %+v", es)
	}
}

func TestEntriesDecodesAKeyedListElement(t *testing.T) {
	ctx := t.Context()
	pod := newPod(newNS(t), "p")
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: "example.com/ready", Status: corev1.ConditionTrue},
	}
	if err := managedfields.Seed(ctx, k8s, pod, "cond", managedfields.OnStatus()); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}

	e := find(mustEntries(t, pod), "cond", "status")
	if e == nil {
		t.Fatalf("no status entry for cond: %+v", mustEntries(t, pod))
	}
	want := `.status.conditions[type="example.com/ready"]`
	if !slices.Contains(e.Paths, want) {
		t.Errorf("paths %q lack %s", e.Paths, want)
	}
}

func TestEntriesSortingIsIndependentOfInputOrder(t *testing.T) {
	mk := func(manager, sub string, op metav1.ManagedFieldsOperationType, raw string) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{
			Manager: manager, Subresource: sub, Operation: op,
			FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(raw),
		}
	}
	forward := []metav1.ManagedFieldsEntry{
		mk(
			"a",
			"",
			metav1.ManagedFieldsOperationApply,
			`{"f:metadata":{"f:labels":{"f:z":{},"f:a":{}}}}`,
		),
		mk("a", "", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{}}`),
		mk("a", "status", metav1.ManagedFieldsOperationUpdate, `{"f:status":{"f:phase":{}}}`),
		mk("b", "", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{}}`),
	}
	reversed := slices.Clone(forward)
	slices.Reverse(reversed)

	a := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", ManagedFields: forward}}
	b := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", ManagedFields: reversed}}
	ea, eb := mustEntries(t, a), mustEntries(t, b)
	if fmt.Sprint(ea) != fmt.Sprint(eb) {
		t.Fatalf("order depends on input:\n%v\n%v", ea, eb)
	}
	var got []string
	for _, e := range ea {
		got = append(got, e.Manager+"/"+e.Subresource+"/"+string(e.Operation))
	}
	want := []string{"a//Apply", "a//Update", "a/status/Update", "b//Update"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %q, want %q", got, want)
	}
	if !slices.IsSorted(ea[0].Paths) || len(ea[0].Paths) != 2 {
		t.Errorf("paths not sorted or wrong: %q", ea[0].Paths)
	}
	// fieldpath iterates list indices numerically ([9] before [10]) while
	// string order is the reverse, so this only passes if Entries sorts the
	// rendered strings itself.
	idx := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "x", ManagedFields: []metav1.ManagedFieldsEntry{
			mk("c", "", metav1.ManagedFieldsOperationApply, `{"f:l":{"i:10":{},"i:9":{}}}`),
		}},
	}
	if got := mustEntries(t, idx)[0].Paths; !slices.Equal(got, []string{".l[10]", ".l[9]"}) {
		t.Errorf("paths = %q, want string order", got)
	}
	if m := managedfields.Managers(b); !slices.Equal(m, []string{"a", "b"}) {
		t.Errorf("Managers = %q", m)
	}
}

// Two Update entries for one manager on different apiVersions tie on (manager,
// subresource, operation) and differ only in paths, which is the case the
// path-list tiebreak exists for.
func TestEntriesTiedOnKeyAreOrderedByPaths(t *testing.T) {
	mk := func(raw string) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{
			Manager: "m", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(raw),
		}
	}
	x, y := mk(`{"f:spec":{"f:a":{}}}`), mk(`{"f:spec":{"f:b":{}}}`)
	for name, in := range map[string][]metav1.ManagedFieldsEntry{
		"xy": {x, y},
		"yx": {y, x},
	} {
		t.Run(name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", ManagedFields: in}}
			es := mustEntries(t, pod)
			if len(es) != 2 || es[0].Paths[0] != ".spec.a" || es[1].Paths[0] != ".spec.b" {
				t.Errorf("tied entries not ordered by paths: %+v", es)
			}
		})
	}
}

func TestOwnersMatchesExactlyNotByPrefix(t *testing.T) {
	obj := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "x", ManagedFields: []metav1.ManagedFieldsEntry{
			{
				Manager:    "deep",
				Operation:  metav1.ManagedFieldsOperationApply,
				FieldsType: "FieldsV1",
				FieldsV1:   metav1.NewFieldsV1(`{"f:metadata":{"f:labels":{"f:abc":{}}}}`),
			},
			{
				Manager:    "exact",
				Operation:  metav1.ManagedFieldsOperationApply,
				FieldsType: "FieldsV1",
				FieldsV1:   metav1.NewFieldsV1(`{"f:metadata":{"f:labels":{"f:ab":{}}}}`),
			},
			{
				Manager:     "status",
				Subresource: "status",
				Operation:   metav1.ManagedFieldsOperationUpdate,
				FieldsType:  "FieldsV1",
				FieldsV1:    metav1.NewFieldsV1(`{"f:metadata":{"f:labels":{"f:ab":{}}}}`),
			},
		}},
	}
	if got := managedfields.Owners(
		obj,
		".metadata.labels.ab",
	); !slices.Equal(
		got,
		[]string{"exact", "status"},
	) {
		t.Errorf(
			"Owners(.metadata.labels.ab) = %q, want exact and status (any subresource, no prefix match)",
			got,
		)
	}
	if got := managedfields.Owners(obj, ".metadata.labels"); len(got) != 0 {
		t.Errorf("Owners(.metadata.labels) = %q, want none", got)
	}
	if got := managedfields.Owners(obj, ".metadata.labels.a"); len(got) != 0 {
		t.Errorf("Owners(.metadata.labels.a) = %q, want none", got)
	}
}

func TestSeedAppliesAsManagerAndLeavesObjUntouched(t *testing.T) {
	ctx := t.Context()
	pod := newPod(newNS(t), "p")
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	before := pod.DeepCopy()
	if err := managedfields.Seed(ctx, k8s, pod, "seeder"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(before) != fmt.Sprint(pod) {
		t.Errorf("Seed mutated obj:\nbefore %v\nafter  %v", before, pod)
	}

	live := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), live); err != nil {
		t.Fatal(err)
	}
	e := find(mustEntries(t, live), "seeder", "")
	if e == nil || e.Operation != metav1.ManagedFieldsOperationApply {
		t.Fatalf("no Apply entry for seeder: %+v", mustEntries(t, live))
	}
	if !slices.Contains(e.Paths, `.spec.containers[name="c"].image`) {
		t.Errorf("seeder paths %q lack the container image", e.Paths)
	}
}

func TestSeedOnStatusRecordsTheStatusSubresource(t *testing.T) {
	ctx := t.Context()
	pod := newPod(newNS(t), "p")
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	if err := managedfields.Seed(ctx, k8s, pod, "stat", managedfields.OnStatus()); err != nil {
		t.Fatal(err)
	}
	live := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), live); err != nil {
		t.Fatal(err)
	}
	if find(mustEntries(t, live), "stat", "") != nil {
		t.Errorf("OnStatus produced a main-resource entry: %+v", mustEntries(t, live))
	}
	e := find(mustEntries(t, live), "stat", "status")
	if e == nil || e.Operation != metav1.ManagedFieldsOperationApply {
		t.Fatalf("no status Apply entry: %+v", mustEntries(t, live))
	}
	if !slices.Contains(e.Paths, ".status.phase") {
		t.Errorf("paths %q lack .status.phase", e.Paths)
	}
	// Documented on OnStatus: the whole object is applied, so the status
	// manager also claims the metadata the apiserver keeps for the subresource.
	if !slices.Contains(e.Paths, ".metadata.labels.a") {
		t.Errorf("paths %q lack .metadata.labels.a, OnStatus doc is stale", e.Paths)
	}
}

func TestRenameManagerRenamesOnlyMatchingEntries(t *testing.T) {
	ctx := t.Context()
	pod := newPod(newNS(t), "p")
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := managedfields.Seed(ctx, k8s, pod, "old"); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	if err := managedfields.Seed(ctx, k8s, pod, "old", managedfields.OnStatus()); err != nil {
		t.Fatal(err)
	}
	if err := managedfields.Seed(ctx, k8s, pod, "other"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	before := mustEntries(t, pod)

	if err := managedfields.RenameManager(ctx, k8s, pod, "old", "new"); err != nil {
		t.Fatal(err)
	}
	live := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), live); err != nil {
		t.Fatal(err)
	}
	after := mustEntries(t, live)

	if len(after) != len(before) {
		t.Fatalf("entry count %d -> %d:\n%+v\n%+v", len(before), len(after), before, after)
	}
	for _, name := range []string{"old"} {
		if slices.Contains(managedfields.Managers(live), name) {
			t.Errorf("manager %q survived: %q", name, managedfields.Managers(live))
		}
	}
	if !slices.Contains(managedfields.Managers(live), "other") {
		t.Errorf("unrelated manager lost: %q", managedfields.Managers(live))
	}
	for _, b := range before {
		want := b.Manager
		if want == "old" {
			want = "new"
		}
		a := find(after, want, b.Subresource)
		if a == nil || !slices.Equal(a.Paths, b.Paths) || a.Operation != b.Operation {
			t.Errorf("entry %+v did not carry over as %q: got %+v", b, want, a)
		}
	}
	owners := managedfields.Owners(live, ".metadata.labels.a")
	if !slices.Contains(owners, "new") || !slices.Contains(owners, "other") ||
		slices.Contains(owners, "old") {
		t.Errorf("label owners = %q, want new and other and not old", owners)
	}
}

func TestRenameManagerOnAStaleObjectFailsAndChangesNothing(t *testing.T) {
	ctx := t.Context()
	pod := newPod(newNS(t), "p")
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := managedfields.Seed(ctx, k8s, pod, "old"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	stale := pod.DeepCopy()

	pod.Labels["moved"] = "on"
	if err := k8s.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	live := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), live); err != nil {
		t.Fatal(err)
	}
	before := mustEntries(t, live)

	err := managedfields.RenameManager(ctx, k8s, stale, "old", "new")
	if err == nil {
		t.Fatal("RenameManager on a stale object returned nil")
	}
	if errors.Is(err, managedfields.ErrNoMatch) {
		t.Errorf("a stale resourceVersion reported as ErrNoMatch: %v", err)
	}
	again := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != live.ResourceVersion ||
		fmt.Sprint(mustEntries(t, again)) != fmt.Sprint(before) {
		t.Errorf("a failed rename changed the object:\n%+v\n%+v", before, mustEntries(t, again))
	}
}

func TestRenameManagerWithNoMatchIsAnError(t *testing.T) {
	ctx := t.Context()
	pod := newPod(newNS(t), "p")
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	err := managedfields.RenameManager(ctx, k8s, pod, "nobody", "new")
	if err == nil {
		t.Fatal("renaming a manager that is not there returned nil")
	}
	if !errors.Is(err, managedfields.ErrNoMatch) {
		t.Errorf("error %v does not wrap ErrNoMatch", err)
	}
	for _, w := range []string{"nobody", "new", "Pod", "/p"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q lacks %q", err, w)
		}
	}
}
