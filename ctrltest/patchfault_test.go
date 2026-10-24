// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func invalidPatchClient(
	t *testing.T,
	funcs interceptor.Funcs,
	objs ...client.Object,
) client.Client {
	t.Helper()
	return interceptor.NewClient(fake.NewClientBuilder().WithObjects(objs...).Build(), funcs)
}

func cm(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}}
}

func cmKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: "ns", Name: name}
}

func labelPatch(t *testing.T, c client.Client, name string) error {
	t.Helper()
	obj := cm(name)
	return c.Patch(context.Background(), obj, client.RawPatch(
		"application/merge-patch+json", []byte(`{"data":{"k":"v"}}`)))
}

func matchName(name string) func(client.Object) bool {
	return func(o client.Object) bool { return o.GetName() == name }
}

func TestInvalidPatchFailsMatchingWithInvalid(t *testing.T) {
	c := invalidPatchClient(t, InvalidPatch(matchName("a"), 0), cm("a"))
	err := labelPatch(t, c, "a")
	if !apierrors.IsInvalid(err) {
		t.Fatalf("want IsInvalid, got %v", err)
	}
	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), cmKey("a"), got); err != nil {
		t.Fatal(err)
	}
	if got.Data["k"] != "" {
		t.Fatalf("failed patch was applied: %v", got.Data)
	}
}

func TestInvalidPatchPassesNonMatchingThrough(t *testing.T) {
	c := invalidPatchClient(t, InvalidPatch(matchName("a"), 0), cm("a"), cm("b"))
	if err := labelPatch(t, c, "b"); err != nil {
		t.Fatalf("non-matching patch failed: %v", err)
	}
	got := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), cmKey("b"), got); err != nil {
		t.Fatal(err)
	}
	if got.Data["k"] != "v" {
		t.Fatalf("patch not applied: %v", got.Data)
	}
}

func TestInvalidPatchPassesNonPatchCallsThrough(t *testing.T) {
	c := invalidPatchClient(t, InvalidPatch(func(client.Object) bool { return true }, 0), cm("a"))
	ctx := context.Background()

	got := &corev1.ConfigMap{}
	if err := c.Get(ctx, cmKey("a"), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Data = map[string]string{"x": "y"}
	if err := c.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := c.Create(ctx, cm("new")); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestInvalidPatchTimesLimitsFailures(t *testing.T) {
	c := invalidPatchClient(t, InvalidPatch(matchName("a"), 2), cm("a"))
	for i := range 2 {
		if err := labelPatch(t, c, "a"); !apierrors.IsInvalid(err) {
			t.Fatalf("patch %d: want IsInvalid, got %v", i, err)
		}
	}
	for i := range 3 {
		if err := labelPatch(t, c, "a"); err != nil {
			t.Fatalf("patch %d after limit: %v", i, err)
		}
	}
}

func TestInvalidPatchNonPositiveTimesAlwaysFails(t *testing.T) {
	for _, times := range []int{0, -1} {
		c := invalidPatchClient(t, InvalidPatch(matchName("a"), times), cm("a"))
		for i := range 5 {
			if err := labelPatch(t, c, "a"); !apierrors.IsInvalid(err) {
				t.Fatalf("times=%d patch %d: want IsInvalid, got %v", times, i, err)
			}
		}
	}
}

func TestInvalidPatchCountsConcurrentCalls(t *testing.T) {
	c := invalidPatchClient(t, InvalidPatch(matchName("a"), 10), cm("a"))
	var failures atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if apierrors.IsInvalid(labelPatch(t, c, "a")) {
				failures.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := failures.Load(); got != 10 {
		t.Fatalf("want exactly 10 failures, got %d", got)
	}
}
