// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	statefulSetPodNameLabel  = "statefulset.kubernetes.io/pod-name"
	statefulSetRevisionLabel = "controller-revision-hash"
	statefulSetPodIndexLabel = "apps.kubernetes.io/pod-index"
)

// tickStatefulSetPods acts as the StatefulSet controller's pod and claim
// half. Run only when Options.SimWorkloads is set.
//
// A StatefulSet under deletion is skipped for the reason tickDeploymentPods
// gives: there is nothing left for the controller to do, and garbage
// collection, if also on, is what clears its pods and claims.
func (s *DataPlaneSim) tickStatefulSetPods(ctx context.Context) {
	stss := &appsv1.StatefulSetList{}
	if err := s.c.List(ctx, stss); err != nil {
		return
	}
	pods := &corev1.PodList{}
	if err := s.c.List(ctx, pods); err != nil {
		return
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := s.c.List(ctx, pvcs); err != nil {
		return
	}
	for i := range stss.Items {
		ss := &stss.Items[i]
		if !ss.DeletionTimestamp.IsZero() {
			continue
		}
		// Claims first: the real controller provisions a claim before the
		// pod that mounts it, and ensuring every ordinal's claims exist is
		// not gated by podManagementPolicy the way pod creation is.
		s.reconcileStatefulSetClaims(ctx, ss, pods.Items, pvcs.Items)
		s.reconcileStatefulSetPods(ctx, ss, pods.Items)
	}
}

// statefulSetPodOrdinals maps every pod ss controls, active or terminating,
// to the ordinal its name encodes.
func statefulSetPodOrdinals(ss *appsv1.StatefulSet, allPods []corev1.Pod) map[int]*corev1.Pod {
	prefix := ss.Name + "-"
	out := map[int]*corev1.Pod{}
	for i := range allPods {
		p := &allPods[i]
		if p.Namespace != ss.Namespace ||
			!isControlledBy(p, ss, statefulSetAPIVersion, statefulSetKind) {
			continue
		}
		if ord, ok := parseOrdinalSuffix(p.Name, prefix); ok {
			out[ord] = p
		}
	}
	return out
}

func statefulSetPodRevision(p *corev1.Pod) string {
	return p.Labels[statefulSetRevisionLabel]
}

// reconcileStatefulSetPods takes exactly one step toward ss's desired pods:
// a scale change acts on every ordinal podManagementPolicy allows in one
// sweep, but an update retires at most one old-revision pod, matching
// tickDeploymentPods's "one step per sweep" rule.
func (s *DataPlaneSim) reconcileStatefulSetPods(
	ctx context.Context,
	ss *appsv1.StatefulSet,
	allPods []corev1.Pod,
) {
	want := int(statefulSetReplicas(ss))
	parallel := ss.Spec.PodManagementPolicy == appsv1.ParallelPodManagement
	byOrdinal := statefulSetPodOrdinals(ss, allPods)

	if s.scaleDownStatefulSet(ctx, byOrdinal, want, parallel) {
		return
	}
	if s.createStatefulSetPods(ctx, ss, byOrdinal, want, parallel) {
		return
	}
	s.rollStatefulSetPod(ctx, ss, byOrdinal, want)
}

// scaleDownStatefulSet deletes active pods at ordinals >= want, reporting
// whether it deleted anything. Parallel deletes all of them in one sweep;
// OrderedReady (the zero value) deletes only the highest ordinal, one sweep
// at a time.
func (s *DataPlaneSim) scaleDownStatefulSet(
	ctx context.Context,
	byOrdinal map[int]*corev1.Pod,
	want int,
	parallel bool,
) bool {
	var extras []int
	for ord, p := range byOrdinal {
		if ord >= want && p.DeletionTimestamp.IsZero() {
			extras = append(extras, ord)
		}
	}
	if len(extras) == 0 {
		return false
	}
	sort.Sort(sort.Reverse(sort.IntSlice(extras)))
	if !parallel {
		extras = extras[:1]
	}
	for _, ord := range extras {
		_ = s.c.Delete(ctx, byOrdinal[ord])
	}
	return true
}

// createStatefulSetPods creates missing pods for ordinals in [0, want),
// reporting whether the caller should take no further action this sweep:
// either it created one, or OrderedReady is waiting on an existing ordinal
// that is not yet Ready. Parallel ignores readiness and fills every missing
// ordinal in one sweep; OrderedReady creates at most the lowest missing one.
func (s *DataPlaneSim) createStatefulSetPods(
	ctx context.Context,
	ss *appsv1.StatefulSet,
	byOrdinal map[int]*corev1.Pod,
	want int,
	parallel bool,
) bool {
	created := false
	for ord := 0; ord < want; ord++ {
		p := byOrdinal[ord]
		if p == nil {
			s.createStatefulSetPod(ctx, ss, ord)
			created = true
			if !parallel {
				return true
			}
			continue
		}
		if parallel {
			continue
		}
		if !p.DeletionTimestamp.IsZero() || !isPodReady(p) {
			return true
		}
	}
	return created
}

// rollStatefulSetPod deletes the one pod a RollingUpdate should retire this
// sweep: the highest ordinal at or above partition that is not yet at the
// current template's revision, once every pod in [0, want) is Ready. OnDelete
// never deletes anything here; a pod it should replace only goes when
// something else deletes it, and is then recreated at the new revision by
// createStatefulSetPods.
func (s *DataPlaneSim) rollStatefulSetPod(
	ctx context.Context,
	ss *appsv1.StatefulSet,
	byOrdinal map[int]*corev1.Pod,
	want int,
) {
	if ss.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return
	}
	updateRevision := ss.Name + "-" + podTemplateHash(&ss.Spec.Template)
	partition := int32(0)
	if ru := ss.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		partition = *ru.Partition
	}

	active := make([]*corev1.Pod, 0, want)
	for ord := 0; ord < want; ord++ {
		if p := byOrdinal[ord]; p != nil {
			active = append(active, p)
		}
	}
	if !everyPodReady(active) {
		return
	}

	for ord := want - 1; ord >= int(partition); ord-- {
		p := byOrdinal[ord]
		if p != nil && statefulSetPodRevision(p) != updateRevision {
			_ = s.c.Delete(ctx, p)
			return
		}
	}
}

// createStatefulSetPod creates the pod for ordinal: named "<sts>-<ordinal>",
// at the current template's revision, with a volume per claim template
// pointing at that ordinal's claim.
func (s *DataPlaneSim) createStatefulSetPod(
	ctx context.Context,
	ss *appsv1.StatefulSet,
	ordinal int,
) {
	name := fmt.Sprintf("%s-%d", ss.Name, ordinal)
	revision := ss.Name + "-" + podTemplateHash(&ss.Spec.Template)

	labels := mergeLabels(ss.Spec.Template.Labels, map[string]string{
		statefulSetPodNameLabel:  name,
		statefulSetRevisionLabel: revision,
		statefulSetPodIndexLabel: strconv.Itoa(ordinal),
	})

	spec := *ss.Spec.Template.Spec.DeepCopy()
	spec.Hostname = name
	spec.Subdomain = ss.Spec.ServiceName
	for _, vct := range ss.Spec.VolumeClaimTemplates {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: vct.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: statefulSetClaimName(vct.Name, ss.Name, ordinal),
				},
			},
		})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ss.Namespace,
			Labels:      labels,
			Annotations: ss.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				controllerRef(ss, statefulSetAPIVersion, statefulSetKind),
			},
		},
		Spec: spec,
	}
	s.createPod(ctx, pod)
}

func statefulSetClaimName(template, statefulSet string, ordinal int) string {
	return fmt.Sprintf("%s-%s-%d", template, statefulSet, ordinal)
}

// statefulSetRetentionPolicy returns ss's PVC retention policy, defaulting
// both fields to Retain: a StatefulSet that does not set the field at all,
// which is most of them, keeps every claim regardless of what happens to the
// StatefulSet or to one ordinal.
func statefulSetRetentionPolicy(
	ss *appsv1.StatefulSet,
) (whenDeleted, whenScaled appsv1.PersistentVolumeClaimRetentionPolicyType) {
	whenDeleted, whenScaled = appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	if r := ss.Spec.PersistentVolumeClaimRetentionPolicy; r != nil {
		if r.WhenDeleted != "" {
			whenDeleted = r.WhenDeleted
		}
		if r.WhenScaled != "" {
			whenScaled = r.WhenScaled
		}
	}
	return whenDeleted, whenScaled
}

// reconcileStatefulSetClaims ensures a claim exists for every volume claim
// template at every ordinal in [0, want), and applies the retention policy:
// whenDeleted:Delete gives a newly created claim an owner reference to ss, so
// garbage collection (if on) removes it when ss itself goes; whenScaled:Delete
// removes the claims of an ordinal that has been scaled away once its pod is
// gone. An existing claim is never recreated or modified, which is also why
// whenDeleted is only applied at creation: retroactively adding an owner
// reference to a claim that predates the policy would reach into something
// the policy explicitly leaves alone.
func (s *DataPlaneSim) reconcileStatefulSetClaims(
	ctx context.Context,
	ss *appsv1.StatefulSet,
	allPods []corev1.Pod,
	allPVCs []corev1.PersistentVolumeClaim,
) {
	want := int(statefulSetReplicas(ss))
	whenDeleted, whenScaled := statefulSetRetentionPolicy(ss)
	byOrdinal := statefulSetPodOrdinals(ss, allPods)

	for _, vct := range ss.Spec.VolumeClaimTemplates {
		prefix := vct.Name + "-" + ss.Name + "-"
		existing := map[int]*corev1.PersistentVolumeClaim{}
		for i := range allPVCs {
			pvc := &allPVCs[i]
			if pvc.Namespace != ss.Namespace {
				continue
			}
			if ord, ok := parseOrdinalSuffix(pvc.Name, prefix); ok {
				existing[ord] = pvc
			}
		}

		for ord := 0; ord < want; ord++ {
			if existing[ord] != nil {
				continue
			}
			claim := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      statefulSetClaimName(vct.Name, ss.Name, ord),
					Namespace: ss.Namespace,
					// The StatefulSet controller labels each claim with the claim
					// template's labels plus the StatefulSet's own pod selector,
					// which is how a claim can be found from its set; the
					// template's spec.selector is a volume-binding selector and
					// has nothing to do with it.
					Labels: mergeLabels(vct.Labels, selectorMatchLabels(ss.Spec.Selector)),
				},
				Spec: *vct.Spec.DeepCopy(),
			}
			if whenDeleted == appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
				claim.OwnerReferences = []metav1.OwnerReference{
					ownerRef(ss, statefulSetAPIVersion, statefulSetKind),
				}
			}
			_ = s.c.Create(ctx, claim)
		}

		if whenScaled != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
			continue
		}
		scaledAway := make([]int, 0)
		for ord := range existing {
			if ord >= want && byOrdinal[ord] == nil {
				scaledAway = append(scaledAway, ord)
			}
		}
		sort.Ints(scaledAway)
		for _, ord := range scaledAway {
			_ = s.c.Delete(ctx, existing[ord])
		}
	}
}

func selectorMatchLabels(sel *metav1.LabelSelector) map[string]string {
	if sel == nil {
		return nil
	}
	return sel.MatchLabels
}

// tickStatefulSetStatus computes every StatefulSet's status from the pods it
// currently owns. Run only when Options.SimWorkloads is set, and after
// tickPods in one sweep, for the reason tickDeploymentStatus gives.
func (s *DataPlaneSim) tickStatefulSetStatus(ctx context.Context) {
	stss := &appsv1.StatefulSetList{}
	if err := s.c.List(ctx, stss); err != nil {
		return
	}
	pods := &corev1.PodList{}
	if err := s.c.List(ctx, pods); err != nil {
		return
	}
	for i := range stss.Items {
		s.updateStatefulSetStatus(ctx, &stss.Items[i], pods.Items)
	}
}

func (s *DataPlaneSim) updateStatefulSetStatus(
	ctx context.Context,
	ss *appsv1.StatefulSet,
	allPods []corev1.Pod,
) {
	updateRevision := ss.Name + "-" + podTemplateHash(&ss.Spec.Template)
	byOrdinal := statefulSetPodOrdinals(ss, allPods)

	ordinals := make([]int, 0, len(byOrdinal))
	for ord := range byOrdinal {
		ordinals = append(ordinals, ord)
	}
	sort.Ints(ordinals)

	active := make([]*corev1.Pod, 0, len(ordinals))
	for _, ord := range ordinals {
		if p := byOrdinal[ord]; p.DeletionTimestamp.IsZero() {
			active = append(active, p)
		}
	}

	currentRevision := ss.Status.CurrentRevision
	if currentRevision == "" {
		currentRevision = updateRevision
	}
	allAtUpdate := true
	for _, p := range active {
		if statefulSetPodRevision(p) != updateRevision {
			allAtUpdate = false
			break
		}
	}
	if allAtUpdate {
		currentRevision = updateRevision
	}

	var ready, current, updated int32
	for _, p := range active {
		if isPodReady(p) {
			ready++
		}
		rev := statefulSetPodRevision(p)
		if rev == currentRevision {
			current++
		}
		if rev == updateRevision {
			updated++
		}
	}

	base := ss.DeepCopy()
	ss.Status.ObservedGeneration = ss.Generation
	ss.Status.Replicas = int32Clamped(len(active))
	ss.Status.ReadyReplicas = ready
	ss.Status.AvailableReplicas = ready // minReadySeconds not modelled
	ss.Status.CurrentReplicas = current
	ss.Status.UpdatedReplicas = updated
	ss.Status.CurrentRevision = currentRevision
	ss.Status.UpdateRevision = updateRevision

	if equality.Semantic.DeepEqual(base.Status, ss.Status) {
		return
	}
	_ = s.c.Status().Patch(ctx, ss, client.StrategicMergeFrom(base))
}
