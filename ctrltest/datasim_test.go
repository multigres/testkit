// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/multigres/testkit/assert"
)

const foreignCondition corev1.PodConditionType = "example.com/foreign-ready"

// TestDataPlaneSimPreservesForeignPodConditions pins the sim to kubelet's
// behaviour: a condition another writer adds between the sim's List and its
// patch survives the patch. The hook lands that write in exactly that window,
// so the test is deterministic rather than a race it usually wins.
func TestDataPlaneSimPreservesForeignPodConditions(t *testing.T) {
	c := assert.NewAborting(t)

	scheme := runtime.NewScheme()
	c.NoError(clientgoscheme.AddToScheme(scheme), "add to scheme")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithStatusSubresource(&corev1.Pod{}).
		Build()

	var injected atomic.Bool
	cl := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := w.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, ok := list.(*corev1.PodList); !ok || injected.Swap(true) {
				return nil
			}
			fresh := &corev1.Pod{}
			if err := w.Get(ctx, client.ObjectKeyFromObject(pod), fresh); err != nil {
				return err
			}
			fresh.Status.Conditions = append(fresh.Status.Conditions, corev1.PodCondition{
				Type:   foreignCondition,
				Status: corev1.ConditionTrue,
			})
			return w.Status().Update(ctx, fresh)
		},
	})

	NewDataPlaneSim(cl, time.Hour).tickPods(t.Context())

	c.True(injected.Load(), "the List hook never ran, so nothing was raced")
	got := &corev1.Pod{}
	c.NoError(base.Get(t.Context(), client.ObjectKeyFromObject(pod), got), "get pod")

	var foreign, ready bool
	for _, cond := range got.Status.Conditions {
		switch cond.Type {
		case foreignCondition:
			foreign = cond.Status == corev1.ConditionTrue
		case corev1.PodReady:
			ready = cond.Status == corev1.ConditionTrue
		}
	}
	c.True(ready, "the sim's own Ready condition did not land: %+v", got.Status.Conditions)
	c.True(
		foreign,
		"the sim's patch deleted a condition it does not own: %+v",
		got.Status.Conditions,
	)
}
