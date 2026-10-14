// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (f *workloadFixture) pvcs(namespace string) []corev1.PersistentVolumeClaim {
	f.c.Helper()
	list := &corev1.PersistentVolumeClaimList{}
	f.c.NoError(f.cl.List(context.Background(), list, client.InNamespace(namespace)), "list pvcs")
	return list.Items
}

func (f *workloadFixture) getStatefulSet(name string) *appsv1.StatefulSet {
	f.c.Helper()
	ss := &appsv1.StatefulSet{}
	f.c.NoError(f.cl.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, ss),
		"get statefulset %s", name)
	return ss
}

// statefulSetFixture is a StatefulSet with want replicas, one volume claim
// template, and a trivial pod template.
func statefulSetFixture(name string, want int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "ns",
			UID:        types.UID(name + "-uid"),
			Generation: 1,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(want),
			ServiceName: name + "-svc",
			Template:    podTemplate("v1"),
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "data"}},
			},
		},
	}
}

func TestStatefulSetOrderedReadyCreatesOnlyTheLowestMissingOrdinal(t *testing.T) {
	ss := statefulSetFixture("web", 3)
	f := newWorkloadFixture(t, ss)

	f.sim.tickStatefulSetPods(context.Background())

	pods := podNames(f.pods("ns"))
	f.c.Len(pods, 1, "OrderedReady should create one pod before waiting on it")
	f.c.Contains(pods, "web-0", "the lowest ordinal should be created first")
}

func TestStatefulSetOrderedReadyWaitsOnAnUnreadyLowerOrdinal(t *testing.T) {
	ss := statefulSetFixture("web", 3)
	pod0 := statefulSetPodFixture(ss, 0) // not Ready
	f := newWorkloadFixture(t, ss, pod0)

	f.sim.tickStatefulSetPods(context.Background())

	f.c.Len(f.pods("ns"), 1, "a not-Ready ordinal 0 should block ordinal 1 from being created")
}

func TestStatefulSetOrderedReadyCreatesTheNextOrdinalOnceReady(t *testing.T) {
	ss := statefulSetFixture("web", 3)
	pod0 := readyPod(statefulSetPodFixture(ss, 0))
	f := newWorkloadFixture(t, ss, pod0)

	f.sim.tickStatefulSetPods(context.Background())

	pods := podNames(f.pods("ns"))
	f.c.Len(pods, 2, "ordinal 1 should be created now that ordinal 0 is Ready")
	f.c.Contains(pods, "web-1", "ordinal 1 should exist")
}

func TestStatefulSetParallelCreatesEveryMissingOrdinalAtOnce(t *testing.T) {
	ss := statefulSetFixture("web", 3)
	ss.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	f := newWorkloadFixture(t, ss)

	f.sim.tickStatefulSetPods(context.Background())

	f.c.Len(f.pods("ns"), 3, "Parallel should create every missing ordinal in one sweep")
}

func TestStatefulSetOrderedReadyScalesDownHighestOrdinalOneAtATime(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	pods := []client.Object{
		readyPod(statefulSetPodFixture(ss, 0)),
		readyPod(statefulSetPodFixture(ss, 1)),
		readyPod(statefulSetPodFixture(ss, 2)),
	}
	f := newWorkloadFixture(t, append([]client.Object{ss}, pods...)...)

	f.sim.tickStatefulSetPods(context.Background())

	names := podNames(f.pods("ns"))
	f.c.Len(names, 2, "OrderedReady should delete one pod per sweep")
	f.c.NotContains(names, "web-2", "the highest ordinal should be deleted first")
}

func TestStatefulSetParallelScalesDownEveryExtraOrdinalAtOnce(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	pods := []client.Object{
		readyPod(statefulSetPodFixture(ss, 0)),
		readyPod(statefulSetPodFixture(ss, 1)),
		readyPod(statefulSetPodFixture(ss, 2)),
	}
	f := newWorkloadFixture(t, append([]client.Object{ss}, pods...)...)

	f.sim.tickStatefulSetPods(context.Background())

	f.c.Len(f.pods("ns"), 1, "Parallel should delete every extra ordinal in one sweep")
}

func TestStatefulSetRollingUpdateRetiresHighestOrdinalFirst(t *testing.T) {
	ss := statefulSetFixture("web", 2)
	oldTemplate := podTemplate("v1")
	ss.Spec.Template = podTemplate("v2")
	old0 := readyPod(statefulSetPodFixtureAt(ss, 0, &oldTemplate))
	old1 := readyPod(statefulSetPodFixtureAt(ss, 1, &oldTemplate))
	f := newWorkloadFixture(t, ss, old0, old1)

	f.sim.tickStatefulSetPods(context.Background())

	names := podNames(f.pods("ns"))
	f.c.Len(names, 1, "exactly one old pod should be retired per sweep")
	f.c.NotContains(names, "web-1", "the highest ordinal should be retired first")
}

func TestStatefulSetRollingUpdateWaitsForEveryPodReady(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	oldTemplate := podTemplate("v1")
	ss.Spec.Template = podTemplate("v2")
	old0 := statefulSetPodFixtureAt(ss, 0, &oldTemplate) // not Ready
	f := newWorkloadFixture(t, ss, old0)

	f.sim.tickStatefulSetPods(context.Background())

	names := podNames(f.pods("ns"))
	f.c.Len(names, 1, "a not-Ready pod should block the rolling update")
	f.c.Contains(names, "web-0", "the old pod should not have been retired yet")
}

func TestStatefulSetRollingUpdateRespectsPartition(t *testing.T) {
	ss := statefulSetFixture("web", 3)
	ss.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{
		Partition: ptr.To(int32(2)),
	}
	oldTemplate := podTemplate("v1")
	ss.Spec.Template = podTemplate("v2")
	pods := []client.Object{
		readyPod(statefulSetPodFixtureAt(ss, 0, &oldTemplate)),
		readyPod(statefulSetPodFixtureAt(ss, 1, &oldTemplate)),
		readyPod(statefulSetPodFixtureAt(ss, 2, &oldTemplate)),
	}
	f := newWorkloadFixture(t, append([]client.Object{ss}, pods...)...)

	f.sim.tickStatefulSetPods(context.Background())

	names := podNames(f.pods("ns"))
	f.c.Len(names, 2, "only ordinals >= partition are eligible for update")
	f.c.NotContains(names, "web-2", "ordinal 2 is >= partition and should be retired")
	f.c.Contains(names, "web-0", "ordinal 0 is below partition and must survive")
	f.c.Contains(names, "web-1", "ordinal 1 is below partition and must survive")
}

func TestStatefulSetOnDeleteNeverRetiresAPod(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
	oldTemplate := podTemplate("v1")
	ss.Spec.Template = podTemplate("v2")
	old0 := readyPod(statefulSetPodFixtureAt(ss, 0, &oldTemplate))
	f := newWorkloadFixture(t, ss, old0)

	f.sim.tickStatefulSetPods(context.Background())

	names := podNames(f.pods("ns"))
	f.c.Len(names, 1, "OnDelete must never delete a pod on its own")
	f.c.Contains(names, "web-0", "the old pod should survive untouched")
}

func TestStatefulSetClaimsAreCreatedUpFront(t *testing.T) {
	ss := statefulSetFixture("web", 2)
	f := newWorkloadFixture(t, ss)

	f.sim.tickStatefulSetPods(context.Background())

	claims := f.pvcs("ns")
	f.c.Len(
		claims,
		2,
		"claims should be created for every desired ordinal regardless of podManagementPolicy",
	)
	names := make([]string, len(claims))
	for i, c := range claims {
		names[i] = c.Name
	}
	f.c.Contains(names, "data-web-0", "claim for ordinal 0")
	f.c.Contains(names, "data-web-1", "claim for ordinal 1")
}

// TestStatefulSetClaimLabelsCarryTheSetSelector pins the claim labels the
// StatefulSet controller writes: the claim template's own labels plus the
// set's pod selector, not the claim template's volume-binding selector.
func TestStatefulSetClaimLabelsCarryTheSetSelector(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	ss.Spec.VolumeClaimTemplates[0].Labels = map[string]string{"tier": "data"}
	ss.Spec.VolumeClaimTemplates[0].Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"volume": "fast"},
	}
	f := newWorkloadFixture(t, ss)

	f.sim.tickStatefulSetPods(context.Background())

	claims := f.pvcs("ns")
	f.c.Len(claims, 1, "one claim for one replica")
	f.c.Eq("web", claims[0].Labels["app"], "the set's selector label")
	f.c.Eq("data", claims[0].Labels["tier"], "the claim template's label")
	f.c.Eq("", claims[0].Labels["volume"], "the binding selector must not become a label")
}

func TestStatefulSetExistingClaimIsNeverRecreatedOrModified(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	existing := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-web-0",
			Namespace: "ns",
			Labels:    map[string]string{"custom": "label"},
		},
	}
	f := newWorkloadFixture(t, ss, existing)

	f.sim.tickStatefulSetPods(context.Background())

	claims := f.pvcs("ns")
	f.c.Len(claims, 1, "no second claim should be created")
	f.c.Eq("label", claims[0].Labels["custom"], "an existing claim's labels were overwritten")
}

func TestStatefulSetWhenDeletedRetainKeepsClaimsUnowned(t *testing.T) {
	ss := statefulSetFixture("web", 1) // default policy: Retain/Retain
	f := newWorkloadFixture(t, ss)

	f.sim.tickStatefulSetPods(context.Background())

	claims := f.pvcs("ns")
	f.c.Len(claims, 1, "want one claim")
	f.c.Len(claims[0].OwnerReferences, 0, "a Retain claim should carry no owner reference")
}

func TestStatefulSetWhenDeletedDeleteOwnsTheClaim(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	f := newWorkloadFixture(t, ss)

	f.sim.tickStatefulSetPods(context.Background())

	claims := f.pvcs("ns")
	f.c.Len(claims, 1, "want one claim")
	f.c.Len(
		claims[0].OwnerReferences,
		1,
		"a whenDeleted:Delete claim should be owned by the StatefulSet",
	)
	f.c.Eq(ss.UID, claims[0].OwnerReferences[0].UID, "the claim's owner reference")
}

func TestStatefulSetWhenScaledDeleteRemovesClaimOnceThePodIsGone(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
	}
	extraClaim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-web-1", Namespace: "ns"},
	}
	f := newWorkloadFixture(
		t,
		ss,
		extraClaim,
	) // ordinal 1's pod is already gone; only its claim remains

	f.sim.tickStatefulSetPods(context.Background())

	claims := f.pvcs("ns")
	names := make([]string, len(claims))
	for i, c := range claims {
		names[i] = c.Name
	}
	f.c.NotContains(
		names,
		"data-web-1",
		"a scaled-away ordinal's claim should be deleted once its pod is gone",
	)
}

func TestStatefulSetWhenScaledDeleteWaitsForThePodToBeGone(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	ss.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
	}
	extraPod := readyPod(statefulSetPodFixture(ss, 1))
	extraClaim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-web-1", Namespace: "ns"},
	}
	f := newWorkloadFixture(t, ss, extraPod, extraClaim)

	// Claims are reconciled before pods within one sweep, so the claim for
	// ordinal 1 must survive this very sweep even though the same sweep is
	// about to delete its pod.
	f.sim.tickStatefulSetPods(context.Background())

	claims := f.pvcs("ns")
	names := make([]string, len(claims))
	for i, c := range claims {
		names[i] = c.Name
	}
	f.c.Contains(names, "data-web-1", "the claim should survive while its pod still exists")
}

func TestStatefulSetUnderDeletionIsNotReconciled(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Finalizers = []string{"example.com/keep"}
	f := newWorkloadFixture(t, ss)
	f.c.NoError(f.cl.Delete(context.Background(), ss), "delete statefulset")

	f.sim.tickStatefulSetPods(context.Background())

	f.c.Len(f.pods("ns"), 0, "a statefulset under deletion should not have pods created for it")
	f.c.Len(f.pvcs("ns"), 0, "a statefulset under deletion should not have claims created for it")
}

func TestStatefulSetStatusTracksRevisionsAndReadiness(t *testing.T) {
	ss := statefulSetFixture("web", 2)
	ready := readyPod(statefulSetPodFixture(ss, 0))
	notReady := statefulSetPodFixture(ss, 1)
	f := newWorkloadFixture(t, ss, ready, notReady)

	f.sim.tickStatefulSetStatus(context.Background())

	got := f.getStatefulSet("web")
	f.c.Eq(int64(1), int64(got.Status.ObservedGeneration), "observedGeneration")
	f.c.Eq(int32(2), got.Status.Replicas, "replicas")
	f.c.Eq(int32(1), got.Status.ReadyReplicas, "readyReplicas")
	f.c.Eq(int32(1), got.Status.AvailableReplicas, "availableReplicas")
	f.c.Eq(int32(2), got.Status.CurrentReplicas, "currentReplicas")
	f.c.Eq(int32(2), got.Status.UpdatedReplicas, "updatedReplicas")
	f.c.Eq(got.Status.UpdateRevision, got.Status.CurrentRevision,
		"a freshly created StatefulSet should already be at its own update revision")
}

// TestStatefulSetStatusCurrentRevisionLagsUntilEveryPodIsUpdated pins "current
// becomes update when every pod is at the update revision": a status with a
// stale CurrentRevision must keep reporting it while an old-revision pod
// remains, even though a fresh computation would otherwise be tempted to call
// it done early.
func TestStatefulSetStatusCurrentRevisionLagsUntilEveryPodIsUpdated(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	oldTemplate := podTemplate("v1")
	oldRevision := "web-" + podTemplateHash(&oldTemplate)
	ss.Status.CurrentRevision = oldRevision
	ss.Spec.Template = podTemplate("v2")
	old0 := readyPod(statefulSetPodFixtureAt(ss, 0, &oldTemplate))
	f := newWorkloadFixture(t, ss, old0)

	f.sim.tickStatefulSetStatus(context.Background())

	got := f.getStatefulSet("web")
	f.c.Eq(
		oldRevision,
		got.Status.CurrentRevision,
		"currentRevision moved before every pod updated",
	)
	f.c.NotEq(
		got.Status.CurrentRevision,
		got.Status.UpdateRevision,
		"updateRevision should reflect the new template",
	)
}

func TestStatefulSetStatusSteadyStateWritesNothing(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ready := readyPod(statefulSetPodFixture(ss, 0))
	f := newWorkloadFixture(t, ss, ready)
	ctx := context.Background()

	f.sim.tickStatefulSetStatus(ctx)
	first := f.getStatefulSet("web").ResourceVersion

	f.sim.tickStatefulSetStatus(ctx)
	f.c.Eq(
		first,
		f.getStatefulSet("web").ResourceVersion,
		"a second sweep patched an unchanged status",
	)
}

// statefulSetPodFixture builds the pod createStatefulSetPod would create for
// ordinal, at ss's current template, not yet Ready.
func statefulSetPodFixture(ss *appsv1.StatefulSet, ordinal int) *corev1.Pod {
	return statefulSetPodFixtureAt(ss, ordinal, &ss.Spec.Template)
}

// statefulSetPodFixtureAt is statefulSetPodFixture pinned to an explicit
// template, for a pod that predates a later change to ss.Spec.Template.
func statefulSetPodFixtureAt(
	ss *appsv1.StatefulSet,
	ordinal int,
	tpl *corev1.PodTemplateSpec,
) *corev1.Pod {
	name := statefulSetPodName(ss.Name, ordinal)
	revision := ss.Name + "-" + podTemplateHash(tpl)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ss.Namespace,
			Labels: map[string]string{
				statefulSetPodNameLabel:  name,
				statefulSetRevisionLabel: revision,
				statefulSetPodIndexLabel: strconv.Itoa(ordinal),
			},
			OwnerReferences: []metav1.OwnerReference{
				controllerRef(ss, statefulSetAPIVersion, statefulSetKind),
			},
		},
	}
}

func statefulSetPodName(name string, ordinal int) string {
	return name + "-" + strconv.Itoa(ordinal)
}

// TestSweepHoldsANewPodPendingForOneSweep pins that a workload's new pod is
// not Ready in the sweep that created it. The workload's status therefore
// always shows the intermediate state, pods but none ready, for a whole
// interval, so a controller watching it observes the same sequence on every
// run instead of one that depends on where its reconcile lands in a sweep.
func TestSweepHoldsANewPodPendingForOneSweep(t *testing.T) {
	ss := statefulSetFixture("web", 1)
	ss.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	f := newWorkloadFixture(t, ss)
	ctx := context.Background()

	f.sim.sweep(ctx)
	pods := f.pods("ns")
	f.c.Len(pods, 1, "the sweep creates the pod")
	f.c.Eq(
		corev1.PodPhase(""),
		pods[0].Status.Phase,
		"a pod is not given status in the sweep that created it",
	)
	f.c.Eq(int32(1), f.getStatefulSet("web").Status.Replicas, "replicas counts the new pod")
	f.c.Eq(int32(0), f.getStatefulSet("web").Status.ReadyReplicas, "nothing is ready yet")

	f.sim.sweep(ctx)
	pods = f.pods("ns")
	f.c.Eq(corev1.PodRunning, pods[0].Status.Phase, "the next sweep runs it")
	f.c.Eq(
		int32(1),
		f.getStatefulSet("web").Status.ReadyReplicas,
		"and the workload reports it ready",
	)
}
