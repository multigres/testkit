// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DataPlaneSim stands in for everything envtest does not run: kubelet, the
// Deployment/StatefulSet controllers, and the volume provisioner.
//
// Modelled:
//   - Pods reach Running with a podIP and the conditions and container
//     statuses kubelet reports. Sidecars (init containers with restartPolicy
//     Always) run alongside the regular containers and count toward
//     ContainersReady; other init containers complete first.
//   - Ready follows kubelet: ContainersReady and every readiness gate
//     condition True. The simulator never writes a gate condition itself, so
//     a pod with a gate nobody sets stays not Ready, as it would on a cluster.
//   - Pods made unschedulable, not ready or crashlooping by a PodFault
//     (SetPodFaults, C.FaultPods).
//   - Deployments and StatefulSets report status matching spec.replicas.
//   - PVCs reach Bound.
//
// Not modelled: eviction, node pressure, real volume attachment, actual
// restarts, or any ordering between them. Deployment and StatefulSet status is
// synthetic and does not follow pod faults: envtest runs no ReplicaSet or
// StatefulSet controller, so the pods behind them do not exist to fault.
type DataPlaneSim struct {
	c        client.Client
	interval time.Duration

	// podIPSeq is only touched from the sweep goroutine.
	podIPSeq int

	// mu guards faults, which tests install from their own goroutines.
	mu     sync.Mutex
	faults map[string]*podFaultRule
}

// NewDataPlaneSim returns a simulator that patches status on every object it
// models, once per interval, through c.
func NewDataPlaneSim(c client.Client, interval time.Duration) *DataPlaneSim {
	return &DataPlaneSim{c: c, interval: interval}
}

// Run drives the simulator until ctx is cancelled.
func (s *DataPlaneSim) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tickPods(ctx)
			s.tickDeployments(ctx)
			s.tickStatefulSets(ctx)
			s.tickPVCs(ctx)
		}
	}
}

func (s *DataPlaneSim) tickPods(ctx context.Context) {
	list := &corev1.PodList{}
	if err := s.c.List(ctx, list); err != nil {
		return
	}
	now := metav1.Now()
	for i := range list.Items {
		p := &list.Items[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		want := s.podStatus(p, s.podFault(p), now)
		if equality.Semantic.DeepEqual(want, p.Status) {
			continue
		}
		base := p.DeepCopy()
		p.Status = want
		// Strategic merge, not JSON merge: a JSON merge patch replaces the
		// whole conditions array with this tick's snapshot, deleting any
		// condition another writer (a controller's readiness gate) added
		// since the List. Real kubelet merges non-kubelet conditions from a
		// fresh read, so the sim must not clobber them either. The other
		// status patches below use it for the same reason.
		_ = s.c.Status().Patch(ctx, p, client.StrategicMergeFrom(base))
	}
}

func (s *DataPlaneSim) tickDeployments(ctx context.Context) {
	list := &appsv1.DeploymentList{}
	if err := s.c.List(ctx, list); err != nil {
		return
	}
	for i := range list.Items {
		d := &list.Items[i]
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if d.Status.ObservedGeneration == d.Generation &&
			d.Status.ReadyReplicas == want &&
			d.Status.AvailableReplicas == want &&
			d.Status.UpdatedReplicas == want &&
			d.Status.Replicas == want {
			continue
		}
		base := d.DeepCopy()
		d.Status.ObservedGeneration = d.Generation
		d.Status.Replicas = want
		d.Status.ReadyReplicas = want
		d.Status.AvailableReplicas = want
		d.Status.UpdatedReplicas = want
		d.Status.UnavailableReplicas = 0
		d.Status.Conditions = []appsv1.DeploymentCondition{
			{
				Type:               appsv1.DeploymentAvailable,
				Status:             corev1.ConditionTrue,
				Reason:             "MinimumReplicasAvailable",
				LastTransitionTime: metav1.Now(),
				LastUpdateTime:     metav1.Now(),
			},
			{
				Type:               appsv1.DeploymentProgressing,
				Status:             corev1.ConditionTrue,
				Reason:             "NewReplicaSetAvailable",
				LastTransitionTime: metav1.Now(),
				LastUpdateTime:     metav1.Now(),
			},
		}
		_ = s.c.Status().Patch(ctx, d, client.StrategicMergeFrom(base))
	}
}

func (s *DataPlaneSim) tickStatefulSets(ctx context.Context) {
	list := &appsv1.StatefulSetList{}
	if err := s.c.List(ctx, list); err != nil {
		return
	}
	for i := range list.Items {
		ss := &list.Items[i]
		want := int32(1)
		if ss.Spec.Replicas != nil {
			want = *ss.Spec.Replicas
		}
		if ss.Status.ObservedGeneration == ss.Generation &&
			ss.Status.ReadyReplicas == want &&
			ss.Status.AvailableReplicas == want &&
			ss.Status.CurrentReplicas == want &&
			ss.Status.UpdatedReplicas == want &&
			ss.Status.Replicas == want {
			continue
		}
		base := ss.DeepCopy()
		ss.Status.ObservedGeneration = ss.Generation
		ss.Status.Replicas = want
		ss.Status.ReadyReplicas = want
		ss.Status.AvailableReplicas = want
		ss.Status.CurrentReplicas = want
		ss.Status.UpdatedReplicas = want
		ss.Status.CurrentRevision = ss.Name + "-rev"
		ss.Status.UpdateRevision = ss.Name + "-rev"
		_ = s.c.Status().Patch(ctx, ss, client.StrategicMergeFrom(base))
	}
}

func (s *DataPlaneSim) tickPVCs(ctx context.Context) {
	list := &corev1.PersistentVolumeClaimList{}
	if err := s.c.List(ctx, list); err != nil {
		return
	}
	for i := range list.Items {
		pvc := &list.Items[i]
		if pvc.Status.Phase == corev1.ClaimBound {
			continue
		}
		base := pvc.DeepCopy()
		pvc.Status.Phase = corev1.ClaimBound
		pvc.Status.AccessModes = pvc.Spec.AccessModes
		if pvc.Spec.Resources.Requests != nil {
			pvc.Status.Capacity = pvc.Spec.Resources.Requests
		}
		_ = s.c.Status().Patch(ctx, pvc, client.StrategicMergeFrom(base))
	}
}
