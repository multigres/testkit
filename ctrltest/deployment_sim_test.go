// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/multigres/testkit/assert"
)

type workloadFixture struct {
	c   *assert.C
	cl  client.WithWatch
	sim *DataPlaneSim
}

// newWorkloadFixture builds a sim with Workloads on (GarbageCollection off,
// tests here are not about GC) seeded with objs.
func newWorkloadFixture(t *testing.T, objs ...client.Object) *workloadFixture {
	t.Helper()
	c := assert.NewAborting(t)
	scheme := runtime.NewScheme()
	c.NoError(clientgoscheme.AddToScheme(scheme), "add to scheme")
	c.NoError(appsv1.AddToScheme(scheme), "add apps/v1 to scheme")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&appsv1.Deployment{}, &appsv1.StatefulSet{}).
		Build()
	sim := NewDataPlaneSimWithOptions(cl, time.Hour, DataPlaneSimOptions{Workloads: true})
	return &workloadFixture{c: c, cl: cl, sim: sim}
}

func (f *workloadFixture) pods(namespace string) []corev1.Pod {
	f.c.Helper()
	list := &corev1.PodList{}
	f.c.NoError(f.cl.List(context.Background(), list, client.InNamespace(namespace)), "list pods")
	return list.Items
}

func (f *workloadFixture) getDeployment(name string) *appsv1.Deployment {
	f.c.Helper()
	d := &appsv1.Deployment{}
	f.c.NoError(f.cl.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, d),
		"get deployment %s", name)
	return d
}

// deploymentFixture is a Deployment with want replicas and a trivial pod
// template, the shape every test below starts from and mutates.
func deploymentFixture(name string, want int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "ns",
			UID:        types.UID(name + "-uid"),
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(want),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "v1"}}},
			},
		},
	}
}

// readyPod marks p Ready, as if tickPods had already run a sweep over it.
func readyPod(p *corev1.Pod) *corev1.Pod {
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	return p
}

func TestDeploymentCreatesPodsUpToReplicas(t *testing.T) {
	d := deploymentFixture("d", 3)
	f := newWorkloadFixture(t, d)

	f.sim.tickDeploymentPods(context.Background())

	pods := f.pods("ns")
	f.c.Len(pods, 3, "want 3 pods created")
	hash := podTemplateHash(&d.Spec.Template)
	for _, p := range pods {
		f.c.Eq(hash, podHash(&p), "pod %s has the wrong template hash label", p.Name)
		f.c.True(isControlledBy(&p, d, deploymentAPIVersion, deploymentKind),
			"pod %s is not controlled by the deployment", p.Name)
		f.c.Eq("app", p.Spec.Containers[0].Name, "pod %s does not carry the template spec", p.Name)
	}
}

func TestDeploymentScalesUpWithoutTouchingExistingPods(t *testing.T) {
	d := deploymentFixture("d", 3)
	hash := podTemplateHash(&d.Spec.Template)
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "d-" + hash + "-0",
			Namespace: "ns",
			Labels:    map[string]string{podTemplateHashLabel: hash},
			OwnerReferences: []metav1.OwnerReference{
				controllerRef(d, deploymentAPIVersion, deploymentKind),
			},
		},
	}
	f := newWorkloadFixture(t, d, existing)

	f.sim.tickDeploymentPods(context.Background())

	pods := f.pods("ns")
	f.c.Len(pods, 3, "want 2 new pods alongside the existing one")
	f.c.Contains(podNames(pods), "d-"+hash+"-0", "the existing pod was replaced rather than kept")
}

func TestDeploymentScaleDownPrefersOldHashThenUnreadyThenHighestName(t *testing.T) {
	d := deploymentFixture("d", 1)
	hash := podTemplateHash(&d.Spec.Template)
	ownerRefs := []metav1.OwnerReference{controllerRef(d, deploymentAPIVersion, deploymentKind)}

	currentReady := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-" + hash + "-0", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: hash}, OwnerReferences: ownerRefs,
	}})
	currentUnready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-" + hash + "-1", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: hash}, OwnerReferences: ownerRefs,
	}}
	oldHash := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-stale-0", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: "stale"}, OwnerReferences: ownerRefs,
	}})
	f := newWorkloadFixture(t, d, currentReady, currentUnready, oldHash)

	f.sim.tickDeploymentPods(context.Background())

	pods := podNames(f.pods("ns"))
	f.c.Len(pods, 1, "want 2 of the 3 pods deleted to reach replicas=1")
	f.c.NotContains(pods, "d-stale-0", "the old-hash pod should be the first victim")
	f.c.NotContains(pods, "d-"+hash+"-1", "the unready pod should go before the ready one")
	f.c.Contains(pods, "d-"+hash+"-0", "the ready current-hash pod should survive")
}

// podTemplate is a trivial pod template running one container at image.
func podTemplate(image string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}},
	}
}

func TestDeploymentRollingUpdateWaitsForEveryPodReady(t *testing.T) {
	d := deploymentFixture("d", 1)
	d.Spec.Template.Spec.Containers[0].Image = "v2"
	oldTemplate := podTemplate("v1")
	oldHash := podTemplateHash(&oldTemplate)
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ // not Ready
		Name:      "d-" + oldHash + "-0",
		Namespace: "ns",
		Labels:    map[string]string{podTemplateHashLabel: oldHash},
		OwnerReferences: []metav1.OwnerReference{
			controllerRef(d, deploymentAPIVersion, deploymentKind),
		},
	}}
	f := newWorkloadFixture(t, d, old)

	f.sim.tickDeploymentPods(context.Background())

	pods := f.pods("ns")
	f.c.Len(pods, 1, "a not-Ready pod should block the rolling update entirely")
	f.c.Eq("d-"+oldHash+"-0", pods[0].Name, "the old pod should not have been touched")
}

func TestDeploymentRollingUpdateRetiresOneOldPodWhenReady(t *testing.T) {
	d := deploymentFixture("d", 2)
	d.Spec.Template.Spec.Containers[0].Image = "v2"
	oldTemplate := podTemplate("v1")
	oldHash := podTemplateHash(&oldTemplate)
	ownerRefs := []metav1.OwnerReference{controllerRef(d, deploymentAPIVersion, deploymentKind)}
	old0 := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-" + oldHash + "-0", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: oldHash}, OwnerReferences: ownerRefs,
	}})
	old1 := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-" + oldHash + "-1", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: oldHash}, OwnerReferences: ownerRefs,
	}})
	f := newWorkloadFixture(t, d, old0, old1)

	f.sim.tickDeploymentPods(context.Background())

	pods := podNames(f.pods("ns"))
	f.c.Len(pods, 1, "exactly one old pod should be retired per sweep")
	f.c.NotContains(pods, "d-"+oldHash+"-1", "the higher-named old pod should be retired first")
}

func TestDeploymentRecreateDeletesEveryOldPodAtOnce(t *testing.T) {
	d := deploymentFixture("d", 2)
	d.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
	d.Spec.Template.Spec.Containers[0].Image = "v2"
	oldTemplate := podTemplate("v1")
	oldHash := podTemplateHash(&oldTemplate)
	ownerRefs := []metav1.OwnerReference{controllerRef(d, deploymentAPIVersion, deploymentKind)}
	old0 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ // not Ready: Recreate must not care
		Name: "d-" + oldHash + "-0", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: oldHash}, OwnerReferences: ownerRefs,
	}}
	old1 := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-" + oldHash + "-1", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: oldHash}, OwnerReferences: ownerRefs,
	}})
	f := newWorkloadFixture(t, d, old0, old1)

	f.sim.tickDeploymentPods(context.Background())

	f.c.Len(f.pods("ns"), 0, "Recreate should delete every old pod before creating any new one")
}

func TestDeploymentUnderDeletionIsNotReconciled(t *testing.T) {
	d := deploymentFixture("d", 1)
	d.Finalizers = []string{
		"example.com/keep",
	} // so the fake client stages deletion rather than removing it
	f := newWorkloadFixture(t, d)
	f.c.NoError(f.cl.Delete(context.Background(), d), "delete deployment")

	f.sim.tickDeploymentPods(context.Background())

	f.c.Len(f.pods("ns"), 0, "a deployment under deletion should not have pods created for it")
}

func TestDeploymentStatusTracksPodsAndConditions(t *testing.T) {
	d := deploymentFixture("d", 2)
	hash := podTemplateHash(&d.Spec.Template)
	ownerRefs := []metav1.OwnerReference{controllerRef(d, deploymentAPIVersion, deploymentKind)}
	ready := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-" + hash + "-0", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: hash}, OwnerReferences: ownerRefs,
	}})
	notReady := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "d-" + hash + "-1", Namespace: "ns",
		Labels: map[string]string{podTemplateHashLabel: hash}, OwnerReferences: ownerRefs,
	}}
	f := newWorkloadFixture(t, d, ready, notReady)

	f.sim.tickDeploymentStatus(context.Background())

	got := f.getDeployment("d")
	f.c.Eq(int64(1), int64(got.Status.ObservedGeneration), "observedGeneration")
	f.c.Eq(int32(2), got.Status.Replicas, "replicas")
	f.c.Eq(int32(2), got.Status.UpdatedReplicas, "updatedReplicas")
	f.c.Eq(int32(1), got.Status.ReadyReplicas, "readyReplicas")
	f.c.Eq(int32(1), got.Status.AvailableReplicas, "availableReplicas")

	// 25% of 2, rounded down, is 0: available (1) >= want(2) - maxUnavailable(0)
	// is false, so Available should read False.
	avail, foundAvail := deploymentCondition(got, appsv1.DeploymentAvailable)
	f.c.True(foundAvail, "no Available condition was written")
	f.c.Eq(corev1.ConditionFalse, avail.Status, "Available status")

	prog, foundProg := deploymentCondition(got, appsv1.DeploymentProgressing)
	f.c.True(foundProg, "no Progressing condition was written")
	f.c.Eq(corev1.ConditionTrue, prog.Status, "Progressing status")
	f.c.Eq("ReplicaSetUpdated", prog.Reason, "Progressing reason while rollout is incomplete")
}

func TestDeploymentStatusReportsRolloutComplete(t *testing.T) {
	d := deploymentFixture("d", 1)
	hash := podTemplateHash(&d.Spec.Template)
	ready := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "d-" + hash + "-0",
		Namespace: "ns",
		Labels:    map[string]string{podTemplateHashLabel: hash},
		OwnerReferences: []metav1.OwnerReference{
			controllerRef(d, deploymentAPIVersion, deploymentKind),
		},
	}})
	f := newWorkloadFixture(t, d, ready)

	f.sim.tickDeploymentStatus(context.Background())

	got := f.getDeployment("d")
	avail, _ := deploymentCondition(got, appsv1.DeploymentAvailable)
	f.c.Eq(corev1.ConditionTrue, avail.Status, "Available once every replica is ready")
	prog, _ := deploymentCondition(got, appsv1.DeploymentProgressing)
	f.c.Eq("NewReplicaSetAvailable", prog.Reason, "Progressing reason once the rollout is complete")
}

// TestDeploymentStatusAvailableToleratesMaxUnavailable pins that Available
// compares against want - maxUnavailable, not against want itself: with 4
// replicas and the default 25% maxUnavailable (rounded down to 1), 3 ready
// pods is still Available, even though one of the 4 is not Ready.
func TestDeploymentStatusAvailableToleratesMaxUnavailable(t *testing.T) {
	d := deploymentFixture("d", 4)
	hash := podTemplateHash(&d.Spec.Template)
	ownerRefs := []metav1.OwnerReference{controllerRef(d, deploymentAPIVersion, deploymentKind)}
	pods := []client.Object{d}
	for i, name := range []string{"d-" + hash + "-0", "d-" + hash + "-1", "d-" + hash + "-2", "d-" + hash + "-3"} {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns",
			Labels: map[string]string{podTemplateHashLabel: hash}, OwnerReferences: ownerRefs,
		}}
		if i < 3 {
			p = readyPod(p)
		}
		pods = append(pods, p)
	}
	f := newWorkloadFixture(t, pods...)

	f.sim.tickDeploymentStatus(context.Background())

	got := f.getDeployment("d")
	f.c.Eq(int32(3), got.Status.AvailableReplicas, "availableReplicas")
	avail, _ := deploymentCondition(got, appsv1.DeploymentAvailable)
	f.c.Eq(corev1.ConditionTrue, avail.Status,
		"Available should tolerate one unready pod within the default 25% maxUnavailable of 4")
}

// TestDeploymentStatusSteadyStateWritesNothing mirrors
// TestDataPlaneSimSteadyStateWritesNothing: a status matching what the sim
// would compute anyway must not be patched again, or RequireQuiescent would
// never see a Deployment settle.
func TestDeploymentStatusSteadyStateWritesNothing(t *testing.T) {
	d := deploymentFixture("d", 1)
	hash := podTemplateHash(&d.Spec.Template)
	ready := readyPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "d-" + hash + "-0",
		Namespace: "ns",
		Labels:    map[string]string{podTemplateHashLabel: hash},
		OwnerReferences: []metav1.OwnerReference{
			controllerRef(d, deploymentAPIVersion, deploymentKind),
		},
	}})
	f := newWorkloadFixture(t, d, ready)
	ctx := context.Background()

	f.sim.tickDeploymentStatus(ctx)
	first := f.getDeployment("d").ResourceVersion

	f.sim.tickDeploymentStatus(ctx)
	f.c.Eq(
		first,
		f.getDeployment("d").ResourceVersion,
		"a second sweep patched an unchanged status",
	)
}

func podNames(pods []corev1.Pod) []string {
	out := make([]string, len(pods))
	for i, p := range pods {
		out[i] = p.Name
	}
	return out
}

func deploymentCondition(
	d *appsv1.Deployment,
	t appsv1.DeploymentConditionType,
) (appsv1.DeploymentCondition, bool) {
	for _, c := range d.Status.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return appsv1.DeploymentCondition{}, false
}
