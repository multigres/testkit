// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"fmt"
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestWorkloadSimulationEndToEnd drives SimWorkloads and SimGarbageCollection
// through the real envtest suite rather than a fake client: a Deployment and
// a StatefulSet reach Ready through pods and a claim the sim itself creates,
// a fault on one pod reaches their status the way a real cluster's would, and
// deleting each owner, one in the background and one in the foreground,
// proves the cascade actually runs. None of this is reachable from a fake
// client, since it depends on the API server's own finalizer and
// deletionTimestamp handling around Delete.
func TestWorkloadSimulationEndToEnd(t *testing.T) {
	c := shared.Case(t)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: c.NS},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "web:v1"}},
				},
			},
		},
	}
	c.NoError(c.Create(dep), "create deployment")

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: c.NS},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(int32(1)),
			ServiceName: "db",
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "db:v1"}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			}},
		},
	}
	c.NoError(c.Create(ss), "create statefulset")

	c.Eventually(10*time.Second, "deployment reaches 2 ready replicas", func() error {
		got := &appsv1.Deployment{}
		if err := c.Get(client.ObjectKeyFromObject(dep), got); err != nil {
			return err
		}
		if got.Status.ReadyReplicas != 2 {
			return fmt.Errorf("readyReplicas=%d", got.Status.ReadyReplicas)
		}
		return nil
	})
	c.Eventually(10*time.Second, "statefulset reaches 1 ready replica", func() error {
		got := &appsv1.StatefulSet{}
		if err := c.Get(client.ObjectKeyFromObject(ss), got); err != nil {
			return err
		}
		if got.Status.ReadyReplicas != 1 {
			return fmt.Errorf("readyReplicas=%d", got.Status.ReadyReplicas)
		}
		return nil
	})
	claim := &corev1.PersistentVolumeClaim{}
	c.Eventually(10*time.Second, "the statefulset's claim is created and bound", func() error {
		if err := c.Get(client.ObjectKey{Namespace: c.NS, Name: "data-db-0"}, claim); err != nil {
			return err
		}
		if claim.Status.Phase != corev1.ClaimBound {
			return fmt.Errorf("claim phase=%s", claim.Status.Phase)
		}
		return nil
	})

	// Fault exactly one Deployment pod, named deterministically so the test
	// does not depend on sweep timing choosing which one exists first.
	webPods := &corev1.PodList{}
	c.NoError(c.List(webPods, client.MatchingLabels{"app": "web"}), "list web pods")
	c.Require().Len(webPods.Items, 2, "want 2 deployment pods before faulting one")
	names := make([]string, len(webPods.Items))
	for i, p := range webPods.Items {
		names[i] = p.Name
	}
	sort.Strings(names)
	broken := names[0]

	c.FaultPods(func(p *corev1.Pod) *PodFault {
		if p.Name == broken {
			return &PodFault{CrashLoop: []string{"app"}}
		}
		return nil
	})

	c.Eventually(10*time.Second, "deployment status follows the faulted pod", func() error {
		got := &appsv1.Deployment{}
		if err := c.Get(client.ObjectKeyFromObject(dep), got); err != nil {
			return err
		}
		if got.Status.ReadyReplicas != 1 {
			return fmt.Errorf(
				"readyReplicas=%d, want 1 with one pod crashlooping",
				got.Status.ReadyReplicas,
			)
		}
		for _, cond := range got.Status.Conditions {
			if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionFalse {
				return nil
			}
		}
		return fmt.Errorf("Available condition did not go False: %+v", got.Status.Conditions)
	})

	c.FaultPods(nil)
	c.Eventually(10*time.Second, "deployment status recovers once the pod heals", func() error {
		got := &appsv1.Deployment{}
		if err := c.Get(client.ObjectKeyFromObject(dep), got); err != nil {
			return err
		}
		if got.Status.ReadyReplicas != 2 {
			return fmt.Errorf("readyReplicas=%d", got.Status.ReadyReplicas)
		}
		return nil
	})

	// Background delete (the default): the apiserver removes the Deployment
	// immediately since it has no finalizers, and only garbage collection
	// ever notices its pods are now dangling and removes them.
	c.NoError(c.Delete(dep), "delete deployment")
	c.Eventually(10*time.Second, "the deployment's pods are cascade-deleted", func() error {
		list := &corev1.PodList{}
		if err := c.List(list, client.MatchingLabels{"app": "web"}); err != nil {
			return err
		}
		if len(list.Items) != 0 {
			return fmt.Errorf("%d pods remain", len(list.Items))
		}
		return nil
	})

	// Foreground delete: the apiserver sets a deletionTimestamp and the
	// foregroundDeletion finalizer and otherwise does nothing further.
	// Without garbage collection this hangs forever, since nothing deletes
	// the pod behind it or clears the finalizer.
	c.NoError(c.Delete(ss, client.PropagationPolicy(metav1.DeletePropagationForeground)),
		"foreground-delete statefulset")
	c.Eventually(
		10*time.Second,
		"the statefulset finishes deleting in the foreground",
		func() error {
			if err := c.Get(client.ObjectKeyFromObject(ss), &appsv1.StatefulSet{}); err == nil {
				return fmt.Errorf("statefulset still exists")
			}
			return nil
		},
	)
}
