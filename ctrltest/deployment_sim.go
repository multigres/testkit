// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tickDeploymentPods acts as the Deployment controller's pod half: create and
// delete pods so each Deployment's active pods match spec, one scale or
// update step per sweep. Run only when Options.SimWorkloads is set.
//
// A Deployment under deletion is skipped: the real controller does no more
// work on one either, and if garbage collection is also on it is the one
// that clears the pods behind it, same as a real cluster's foreground delete.
func (s *DataPlaneSim) tickDeploymentPods(ctx context.Context) {
	deps := &appsv1.DeploymentList{}
	if err := s.c.List(ctx, deps); err != nil {
		return
	}
	pods := &corev1.PodList{}
	if err := s.c.List(ctx, pods); err != nil {
		return
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		if !d.DeletionTimestamp.IsZero() {
			continue
		}
		s.reconcileDeploymentPods(ctx, d, pods.Items)
	}
}

// reconcileDeploymentPods is tickDeploymentPods for one Deployment. It takes
// exactly one step toward spec: a scale changes every pod it needs to in one
// sweep, but an update changes at most one, which is what "no surge or
// unavailability pacing beyond one-at-a-time" means in the DataPlaneSim doc
// comment.
func (s *DataPlaneSim) reconcileDeploymentPods(
	ctx context.Context,
	d *appsv1.Deployment,
	allPods []corev1.Pod,
) {
	hash := podTemplateHash(&d.Spec.Template)

	var mine, active []*corev1.Pod
	for i := range allPods {
		p := &allPods[i]
		if p.Namespace != d.Namespace ||
			!isControlledBy(p, d, deploymentAPIVersion, deploymentKind) {
			continue
		}
		mine = append(mine, p)
		if p.DeletionTimestamp.IsZero() {
			active = append(active, p)
		}
	}

	want := deploymentReplicas(d)
	total := int32Clamped(len(active))

	switch {
	case total > want:
		victims := append([]*corev1.Pod(nil), active...)
		deletionPriority(victims, func(p *corev1.Pod) bool { return podHash(p) != hash })
		for _, p := range victims[:total-want] {
			_ = s.c.Delete(ctx, p)
		}

	case total < want:
		s.createDeploymentPods(ctx, d, hash, mine, int(want-total))

	default:
		var old []*corev1.Pod
		for _, p := range active {
			if podHash(p) != hash {
				old = append(old, p)
			}
		}
		if len(old) == 0 {
			return
		}
		if d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
			for _, p := range old {
				_ = s.c.Delete(ctx, p)
			}
			return
		}
		// RollingUpdate, which is also what an unset Strategy.Type defaults
		// to on a real apiserver.
		if !everyPodReady(active) {
			return
		}
		deletionPriority(old, func(*corev1.Pod) bool { return true })
		_ = s.c.Delete(ctx, old[0])
	}
}

// createDeploymentPods creates n pods for d at hash, named
// "<deployment>-<hash>-<n>" for the smallest indices not already used by one
// of mine at that hash. mine is every pod the Deployment owns, active or
// terminating, so a name is never reused while its old holder is still being
// deleted.
func (s *DataPlaneSim) createDeploymentPods(
	ctx context.Context,
	d *appsv1.Deployment,
	hash string,
	mine []*corev1.Pod,
	n int,
) {
	prefix := fmt.Sprintf("%s-%s-", d.Name, hash)
	used := map[int]bool{}
	for _, p := range mine {
		if podHash(p) != hash {
			continue
		}
		if idx, ok := parseOrdinalSuffix(p.Name, prefix); ok {
			used[idx] = true
		}
	}

	for _, idx := range nextOrdinals(used, n) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s%d", prefix, idx),
				Namespace: d.Namespace,
				Labels: mergeLabels(
					d.Spec.Template.Labels,
					map[string]string{podTemplateHashLabel: hash},
				),
				Annotations: d.Spec.Template.Annotations,
				OwnerReferences: []metav1.OwnerReference{
					controllerRef(d, deploymentAPIVersion, deploymentKind),
				},
			},
			Spec: *d.Spec.Template.Spec.DeepCopy(),
		}
		s.createPod(ctx, pod)
	}
}

// defaultMaxUnavailable is what a Deployment's RollingUpdate strategy uses
// when Spec.Strategy.RollingUpdate or its MaxUnavailable is nil, matching the
// apiserver's own default.
var defaultMaxUnavailable = intstr.FromString("25%")

// tickDeploymentStatus computes every Deployment's status from the pods it
// currently owns. Run only when Options.SimWorkloads is set, and after
// tickPods in one sweep, so a pod fault that just changed Ready is already
// reflected here rather than a sweep behind.
func (s *DataPlaneSim) tickDeploymentStatus(ctx context.Context) {
	deps := &appsv1.DeploymentList{}
	if err := s.c.List(ctx, deps); err != nil {
		return
	}
	pods := &corev1.PodList{}
	if err := s.c.List(ctx, pods); err != nil {
		return
	}
	now := metav1.Now()
	for i := range deps.Items {
		s.updateDeploymentStatus(ctx, &deps.Items[i], pods.Items, now)
	}
}

func (s *DataPlaneSim) updateDeploymentStatus(
	ctx context.Context,
	d *appsv1.Deployment,
	allPods []corev1.Pod,
	now metav1.Time,
) {
	hash := podTemplateHash(&d.Spec.Template)

	var active []*corev1.Pod
	for i := range allPods {
		p := &allPods[i]
		if p.Namespace != d.Namespace ||
			!isControlledBy(p, d, deploymentAPIVersion, deploymentKind) {
			continue
		}
		if p.DeletionTimestamp.IsZero() {
			active = append(active, p)
		}
	}

	var updated, ready int32
	for _, p := range active {
		if podHash(p) == hash {
			updated++
		}
		if isPodReady(p) {
			ready++
		}
	}
	want := deploymentReplicas(d)
	available := ready // minReadySeconds not modelled: Available tracks Ready exactly.

	maxUnavailable := int32(0)
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		mu := defaultMaxUnavailable
		if ru := d.Spec.Strategy.RollingUpdate; ru != nil && ru.MaxUnavailable != nil {
			mu = *ru.MaxUnavailable
		}
		// Error only on a malformed percent string, which the Deployment API
		// validates against at admission; on a real cluster this path never
		// sees one.
		v, err := intstr.GetScaledValueFromIntOrPercent(&mu, int(want), false)
		if err == nil {
			maxUnavailable = int32Clamped(v)
		}
	}
	unavailable := want - available
	if unavailable < 0 {
		unavailable = 0
	}

	base := d.DeepCopy()
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = int32Clamped(len(active))
	d.Status.UpdatedReplicas = updated
	d.Status.ReadyReplicas = ready
	d.Status.AvailableReplicas = available
	d.Status.UnavailableReplicas = unavailable

	setDeploymentCondition(d, appsv1.DeploymentAvailable, available >= want-maxUnavailable,
		"MinimumReplicasAvailable", "MinimumReplicasUnavailable", now)

	rolloutComplete := updated == want && ready >= want
	progressingReason := "ReplicaSetUpdated"
	if rolloutComplete {
		progressingReason = "NewReplicaSetAvailable"
	}
	// Always True: no deadline is modelled, so Progressing never reports
	// ProgressDeadlineExceeded the way a real cluster's Deployment controller
	// would after spec.progressDeadlineSeconds elapses.
	setDeploymentCondition(
		d,
		appsv1.DeploymentProgressing,
		true,
		progressingReason,
		progressingReason,
		now,
	)

	if equality.Semantic.DeepEqual(base.Status, d.Status) {
		return
	}
	_ = s.c.Status().Patch(ctx, d, client.StrategicMergeFrom(base))
}

// setDeploymentCondition sets t's status and reason on d, moving
// LastTransitionTime only when the status changes and LastUpdateTime whenever
// anything about the condition changes, matching the Deployment controller's
// own convention for the two timestamps.
func setDeploymentCondition(
	d *appsv1.Deployment,
	t appsv1.DeploymentConditionType,
	ok bool,
	trueReason, falseReason string,
	now metav1.Time,
) {
	status, reason := corev1.ConditionFalse, falseReason
	if ok {
		status, reason = corev1.ConditionTrue, trueReason
	}
	for i := range d.Status.Conditions {
		c := &d.Status.Conditions[i]
		if c.Type != t {
			continue
		}
		if c.Status != status {
			c.LastTransitionTime = now
		}
		if c.Status != status || c.Reason != reason {
			c.LastUpdateTime = now
		}
		c.Status, c.Reason = status, reason
		return
	}
	d.Status.Conditions = append(d.Status.Conditions, appsv1.DeploymentCondition{
		Type:               t,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: now,
		LastUpdateTime:     now,
	})
}
