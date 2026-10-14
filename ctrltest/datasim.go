// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DataPlaneSim stands in for everything envtest does not run: kubelet, the
// Deployment/StatefulSet/garbage-collector controllers, and the volume
// provisioner.
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
//   - Deployments and StatefulSets report status matching spec.replicas by
//     default. With Options.SimWorkloads (DataPlaneSimOptions.Workloads),
//     instead act as their controllers: create and delete the pods and
//     claims spec calls for, including scale up and down, rolling and
//     Recreate updates (one pod at a time, no surge pacing), partitioned
//     StatefulSet updates, OnDelete, and PVC retention policies, and drive
//     status from those pods, so a pod fault reaches Deployment and
//     StatefulSet status the way it reaches a real cluster's.
//   - With Options.SimGarbageCollection (DataPlaneSimOptions.GarbageCollection),
//     act as the garbage collector over Options.WatchedKinds: background
//     cascade once every owner reference on a dependent is dangling, and the
//     foregroundDeletion and orphan finalizers.
//   - PVCs reach Bound.
//
// Not modelled: eviction, node pressure, real volume attachment, actual
// restarts, or any ordering between them. ReplicaSets and ControllerRevisions
// are not created as objects; a Deployment's pods are owned by the Deployment
// directly, and a StatefulSet's revision bookkeeping lives only in its status
// and its pods' labels. DaemonSets and Jobs are not modelled at all.
// minReadySeconds, surge, and PVC resize are not modelled. Garbage collection
// only sees dependents of a watched kind; a dependent of an unwatched kind is
// invisible to it, and an owner reference naming a kind the scheme or
// RESTMapper does not know is treated as live rather than deleted on a guess.
//
// With both options off, Deployment and StatefulSet status is synthetic and
// does not follow pod faults, and nothing is garbage collected: envtest runs
// none of these controllers, so without the sim the pods behind a workload do
// not exist to fault and a cascade delete leaves every dependent behind.
type DataPlaneSim struct {
	c        client.Client
	interval time.Duration
	opts     DataPlaneSimOptions

	// podIPSeq is only touched from the sweep goroutine.
	podIPSeq int

	// born holds the pods a workload created in the current sweep, which
	// tickPods leaves Pending until the next one. Reset at the start of every
	// sweep; only touched from the sweep goroutine.
	born map[types.NamespacedName]bool

	// mu guards faults, which tests install from their own goroutines.
	mu     sync.Mutex
	faults map[string]*podFaultRule
}

// DataPlaneSimOptions turns on DataPlaneSim's opt-in features. The zero value
// matches NewDataPlaneSim: neither feature runs.
type DataPlaneSimOptions struct {
	// Workloads makes the simulator act as the Deployment and StatefulSet
	// controllers. See the DataPlaneSim doc comment for what that models.
	Workloads bool

	// GarbageCollection makes the simulator act as Kubernetes' garbage
	// collector over WatchedKinds.
	GarbageCollection bool

	// WatchedKinds is every kind garbage collection lists to find dependents.
	// A dependent of a kind missing here is invisible to it. Ignored unless
	// GarbageCollection is set.
	WatchedKinds []client.ObjectList

	// RESTMapper resolves whether an owner reference names a namespaced or a
	// cluster-scoped kind, which decides whether the sim looks it up in the
	// dependent's namespace or outside every namespace, and whether the sim
	// recognizes the kind at all: a reference the mapper cannot map is
	// treated as live rather than deleted on a guess. Required when
	// GarbageCollection is set; ignored otherwise.
	RESTMapper meta.RESTMapper
}

// NewDataPlaneSim returns a simulator that patches status on every object it
// models, once per interval, through c. Neither opt-in feature runs; use
// NewDataPlaneSimWithOptions for those.
func NewDataPlaneSim(c client.Client, interval time.Duration) *DataPlaneSim {
	return &DataPlaneSim{c: c, interval: interval}
}

// NewDataPlaneSimWithOptions is NewDataPlaneSim with Workloads and
// GarbageCollection available to turn on.
func NewDataPlaneSimWithOptions(
	c client.Client,
	interval time.Duration,
	opts DataPlaneSimOptions,
) *DataPlaneSim {
	return &DataPlaneSim{c: c, interval: interval, opts: opts}
}

// Run drives the simulator until ctx is cancelled, one sweep per interval.
func (s *DataPlaneSim) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep is one pass over everything the simulator models.
//
// The order matters. Garbage collection runs first so a cascade delete lands
// before anything below tries to act on what it just removed. Workload pods
// and claims come next, then pod status, then workload status, so a pod
// fault reaches Deployment and StatefulSet status in the same sweep it
// reaches the pod. PVC binding runs last so a claim a StatefulSet created
// this sweep is bound without waiting.
//
// A pod a workload creates in this sweep is left Pending, with no status,
// until the next one. Kubelet never makes a pod Ready the instant it is
// created, and compressing the two into one sweep made the intermediate
// state, a workload with pods but none ready, exist only for the moment
// between two writes inside a sweep. A controller watching the workload then
// saw it on some runs and not others, so the number of status writes it made
// depended on timing. Held back a full interval, the intermediate state is
// always observed, and a script can name every write.
func (s *DataPlaneSim) sweep(ctx context.Context) {
	s.born = map[types.NamespacedName]bool{}
	if s.opts.GarbageCollection {
		s.tickGC(ctx)
	}
	if s.opts.Workloads {
		s.tickDeploymentPods(ctx)
		s.tickStatefulSetPods(ctx)
	}
	s.tickPods(ctx)
	if s.opts.Workloads {
		s.tickDeploymentStatus(ctx)
		s.tickStatefulSetStatus(ctx)
	} else {
		s.tickDeployments(ctx)
		s.tickStatefulSets(ctx)
	}
	s.tickPVCs(ctx)
}

// createPod creates a pod on a workload's behalf and records it as born this
// sweep, so tickPods leaves it Pending until the next one.
func (s *DataPlaneSim) createPod(ctx context.Context, pod *corev1.Pod) {
	if err := s.c.Create(ctx, pod); err != nil {
		return
	}
	if s.born == nil {
		s.born = map[types.NamespacedName]bool{}
	}
	s.born[client.ObjectKeyFromObject(pod)] = true
}

func (s *DataPlaneSim) tickPods(ctx context.Context) {
	list := &corev1.PodList{}
	if err := s.c.List(ctx, list); err != nil {
		return
	}
	now := metav1.Now()
	for i := range list.Items {
		p := &list.Items[i]
		if !p.DeletionTimestamp.IsZero() || s.born[client.ObjectKeyFromObject(p)] {
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
