// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/multigres/testkit/assert"
)

const gateCondition corev1.PodConditionType = "example.com/gate"

// faultPod is a pod with every container kind the simulator distinguishes: a
// regular init container ("setup"), a sidecar ("db", the shape of a Postgres
// run as a native sidecar), a regular init container after it ("migrate"),
// and a regular container ("app").
func faultPod(name string, gates ...corev1.PodConditionType) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "setup", Image: "i"},
				{
					Name:          "db",
					Image:         "i",
					RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
				},
				{Name: "migrate", Image: "i"},
			},
			Containers: []corev1.Container{{Name: "app", Image: "i"}},
		},
	}
	for _, g := range gates {
		p.Spec.ReadinessGates = append(
			p.Spec.ReadinessGates,
			corev1.PodReadinessGate{ConditionType: g},
		)
	}
	return p
}

type simFixture struct {
	c       *assert.C
	sim     *DataPlaneSim
	base    client.Client
	patches atomic.Int32
}

func newSimFixture(t *testing.T, pods ...*corev1.Pod) *simFixture {
	t.Helper()
	c := assert.NewAborting(t)
	scheme := runtime.NewScheme()
	c.NoError(clientgoscheme.AddToScheme(scheme), "add to scheme")
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{})
	for _, p := range pods {
		builder = builder.WithObjects(p)
	}
	f := &simFixture{c: c, base: builder.Build()}
	cl := interceptor.NewClient(f.base.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, w client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			f.patches.Add(1)
			return w.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	f.sim = NewDataPlaneSim(cl, time.Hour)
	return f
}

func (f *simFixture) tick() { f.sim.tickPods(context.Background()) }

func (f *simFixture) get(name string) *corev1.Pod {
	f.c.Helper()
	p := &corev1.Pod{}
	f.c.NoError(f.base.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, p),
		"get pod %s", name)
	return p
}

func condition(p *corev1.Pod, t corev1.PodConditionType) (corev1.ConditionStatus, string) {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return c.Status, c.Reason
		}
	}
	return "", ""
}

func status(p *corev1.Pod, name string) corev1.ContainerStatus {
	for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if cs.Name == name {
			return cs
		}
	}
	return corev1.ContainerStatus{}
}

func stateOf(cs corev1.ContainerStatus) string {
	switch {
	case cs.State.Running != nil:
		return "Running"
	case cs.State.Waiting != nil:
		return "Waiting/" + cs.State.Waiting.Reason
	case cs.State.Terminated != nil:
		return "Terminated/" + cs.State.Terminated.Reason
	}
	return "none"
}

// summary is the part of a pod's status these tests assert on, in one
// comparable string, so a failure shows the whole shape at once.
func summary(p *corev1.Pod) string {
	s := string(p.Status.Phase)
	for _, t := range []corev1.PodConditionType{
		corev1.PodScheduled, corev1.PodInitialized, corev1.ContainersReady, corev1.PodReady,
	} {
		st, reason := condition(p, t)
		s += fmt.Sprintf(" %s=%s", t, st)
		if reason != "" {
			s += "(" + reason + ")"
		}
	}
	for _, name := range []string{"setup", "db", "migrate", "app"} {
		cs := status(p, name)
		s += fmt.Sprintf(" %s:%s", name, stateOf(cs))
		if cs.Ready {
			s += "+ready"
		}
	}
	return s
}

func TestDataPlaneSimPodStatus(t *testing.T) {
	cases := []struct {
		name  string
		fault *PodFault
		want  string
	}{
		{
			name: "healthy: the sidecar runs and counts toward readiness",
			want: "Running PodScheduled=True Initialized=True ContainersReady=True Ready=True" +
				" setup:Terminated/Completed+ready db:Running+ready" +
				" migrate:Terminated/Completed+ready app:Running+ready",
		},
		{
			name:  "a regular container failing readiness",
			fault: &PodFault{NotReady: []string{"app"}},
			want: "Running PodScheduled=True Initialized=True" +
				" ContainersReady=False(ContainersNotReady) Ready=False(ContainersNotReady)" +
				" setup:Terminated/Completed+ready db:Running+ready" +
				" migrate:Terminated/Completed+ready app:Running",
		},
		{
			name:  "a sidecar failing readiness has started, so it blocks nothing",
			fault: &PodFault{NotReady: []string{"db"}},
			want: "Running PodScheduled=True Initialized=True" +
				" ContainersReady=False(ContainersNotReady) Ready=False(ContainersNotReady)" +
				" setup:Terminated/Completed+ready db:Running" +
				" migrate:Terminated/Completed+ready app:Running+ready",
		},
		{
			name:  "a crashlooping sidecar holds back everything after it",
			fault: &PodFault{CrashLoop: []string{"db"}},
			want: "Pending PodScheduled=True Initialized=False(ContainersNotInitialized)" +
				" ContainersReady=False(ContainersNotReady) Ready=False(ContainersNotReady)" +
				" setup:Terminated/Completed+ready db:Waiting/CrashLoopBackOff" +
				" migrate:Waiting/PodInitializing app:Waiting/PodInitializing",
		},
		{
			name:  "a crashlooping regular init container",
			fault: &PodFault{CrashLoop: []string{"setup"}},
			want: "Pending PodScheduled=True Initialized=False(ContainersNotInitialized)" +
				" ContainersReady=False(ContainersNotReady) Ready=False(ContainersNotReady)" +
				" setup:Waiting/CrashLoopBackOff db:Waiting/PodInitializing" +
				" migrate:Waiting/PodInitializing app:Waiting/PodInitializing",
		},
		{
			name:  "a regular init container that never completes",
			fault: &PodFault{NotReady: []string{"migrate"}},
			want: "Pending PodScheduled=True Initialized=False(ContainersNotInitialized)" +
				" ContainersReady=False(ContainersNotReady) Ready=False(ContainersNotReady)" +
				" setup:Terminated/Completed+ready db:Running+ready" +
				" migrate:Running app:Waiting/PodInitializing",
		},
		{
			name:  "unscheduled",
			fault: &PodFault{Unscheduled: true},
			want: "Pending PodScheduled=False(Unschedulable) Initialized=False" +
				" ContainersReady=False(ContainersNotReady) Ready=False(ContainersNotReady)" +
				" setup:none db:none migrate:none app:none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSimFixture(t, faultPod("p"))
			f.sim.SetPodFaults("ns", func(*corev1.Pod) *PodFault { return tc.fault }, t.Errorf)
			f.tick()
			f.c.Eq(tc.want, summary(f.get("p")), "pod status")
		})
	}
}

// TestDataPlaneSimFaultHeals pins that a fault is re-evaluated every sweep, so
// lifting it heals the pod: the old sim skipped any pod it had already made
// Ready, which would have made a fault a one-way door.
func TestDataPlaneSimFaultHeals(t *testing.T) {
	f := newSimFixture(t, faultPod("p"))
	f.tick()
	f.c.Eq(corev1.ConditionTrue, condReady(f.get("p")), "healthy first")

	f.sim.SetPodFaults("ns", func(*corev1.Pod) *PodFault {
		return &PodFault{CrashLoop: []string{"app"}}
	}, t.Errorf)
	f.tick()
	f.c.Eq(corev1.ConditionFalse, condReady(f.get("p")), "broken")

	f.sim.SetPodFaults("ns", nil, nil)
	f.tick()
	p := f.get("p")
	f.c.Eq(corev1.ConditionTrue, condReady(p), "healed")
	f.c.Eq(int32(1), status(p, "app").RestartCount, "restarts survive healing, as on kubelet")
}

func condReady(p *corev1.Pod) corev1.ConditionStatus {
	st, _ := condition(p, corev1.PodReady)
	return st
}

// TestDataPlaneSimReadinessGates pins kubelet's Ready rule: containers ready is
// not enough while a readiness gate condition is not True, and the simulator
// never sets a gate itself.
func TestDataPlaneSimReadinessGates(t *testing.T) {
	f := newSimFixture(t, faultPod("p", gateCondition))
	f.tick()
	p := f.get("p")
	st, reason := condition(p, corev1.PodReady)
	f.c.Eq(corev1.ConditionFalse, st, "Ready with the gate unset")
	f.c.Eq("ReadinessGatesNotReady", reason, "Ready reason with the gate unset")
	st, _ = condition(p, corev1.ContainersReady)
	f.c.Eq(corev1.ConditionTrue, st, "ContainersReady with the gate unset")
	st, _ = condition(p, gateCondition)
	f.c.Eq(corev1.ConditionStatus(""), st, "the sim wrote the gate condition itself")

	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
		Type:   gateCondition,
		Status: corev1.ConditionTrue,
	})
	f.c.NoError(f.base.Status().Update(context.Background(), p), "set gate")
	f.tick()
	f.c.Eq(corev1.ConditionTrue, condReady(f.get("p")), "Ready once the gate is True")
}

// TestDataPlaneSimSteadyStateWritesNothing pins that a pod already matching
// what the simulator would report is not patched again, healthy or faulted,
// so the sim adds no churn for RequireQuiescent to trip over.
func TestDataPlaneSimSteadyStateWritesNothing(t *testing.T) {
	for _, fault := range []*PodFault{nil, {NotReady: []string{"db"}}, {Unscheduled: true}} {
		f := newSimFixture(t, faultPod("p"))
		f.sim.SetPodFaults("ns", func(*corev1.Pod) *PodFault { return fault }, t.Errorf)
		f.tick()
		before := f.patches.Load()
		f.c.Eq(int32(1), before, "first sweep patches once (fault %+v)", fault)
		f.tick()
		f.tick()
		f.c.Eq(before, f.patches.Load(), "later sweeps patch nothing (fault %+v)", fault)
	}
}

// TestDataPlaneSimCrashLoopBackoff pins that restarts climb on kubelet's
// backoff rather than once per sweep, which at a quarter-second sweep would
// make a crashlooping pod a write storm.
func TestDataPlaneSimCrashLoopBackoff(t *testing.T) {
	f := newSimFixture(t, faultPod("p"))
	fault := &PodFault{CrashLoop: []string{"app"}}

	start := metav1.NewTime(time.Unix(1_000_000, 0))
	p := f.get("p")
	at := func(d time.Duration) int32 {
		p.Status = f.sim.podStatus(p, fault, metav1.NewTime(start.Add(d)))
		return status(p, "app").RestartCount
	}
	f.c.Eq(int32(1), at(0), "entering the crash loop is one restart")
	f.c.Eq(int32(1), at(5*time.Second), "inside the first 10s backoff")
	f.c.Eq(int32(2), at(10*time.Second), "after the first backoff")
	f.c.Eq(int32(2), at(25*time.Second), "inside the second, 20s backoff")
	f.c.Eq(int32(3), at(30*time.Second), "after the second backoff")

	for restarts, want := range map[int32]time.Duration{
		1: 10 * time.Second, 2: 20 * time.Second, 5: 160 * time.Second, 6: 5 * time.Minute, 30: 5 * time.Minute,
	} {
		f.c.Eq(want, crashLoopBackoff(restarts), "backoff after %d restarts", restarts)
	}
}

type recordedReports struct{ msgs []string }

func (r *recordedReports) report(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

// TestDataPlaneSimReportsUnknownContainers pins the guard against a fault that
// does nothing because it names a container the pod does not have: reported,
// once per pod and name however many sweeps see it, and never through a rule
// that has since been replaced.
func TestDataPlaneSimReportsUnknownContainers(t *testing.T) {
	f := newSimFixture(t, faultPod("p"), faultPod("q"))
	var r recordedReports
	f.sim.SetPodFaults("ns", func(*corev1.Pod) *PodFault {
		return &PodFault{CrashLoop: []string{"app", "postgress"}}
	}, r.report)
	f.tick()
	f.tick()
	f.c.Len(r.msgs, 2, "one report per pod: %q", r.msgs)

	var after recordedReports
	f.sim.SetPodFaults("ns", nil, nil)
	f.sim.SetPodFaults("ns", func(*corev1.Pod) *PodFault { return nil }, after.report)
	f.tick()
	f.c.Len(r.msgs, 2, "the replaced rule reported again: %q", r.msgs)
	f.c.Len(after.msgs, 0, "a healthy rule reported: %q", after.msgs)
}

// TestFaultPods drives the whole path through the shared suite: a fault
// installed by C.FaultPods reaches the running simulator, matches by what the
// pod runs rather than its name, and heals when replaced.
func TestFaultPods(t *testing.T) {
	c := shared.Case(t)
	broken := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: c.NS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "bad"}}},
	}
	fine := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fine", Namespace: c.NS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "good"}}},
	}
	c.FaultPods(func(p *corev1.Pod) *PodFault {
		if p.Spec.Containers[0].Image == "bad" {
			return &PodFault{CrashLoop: []string{"app"}}
		}
		return nil
	})
	c.NoError(c.Create(broken), "create broken pod")
	c.NoError(c.Create(fine), "create fine pod")

	readyIs := func(name string, want corev1.ConditionStatus) func() error {
		return func() error {
			p := &corev1.Pod{}
			if err := c.Get(client.ObjectKey{Namespace: c.NS, Name: name}, p); err != nil {
				return err
			}
			if got := condReady(p); got != want {
				return fmt.Errorf("pod %s Ready=%q: %s", name, got, summary(p))
			}
			return nil
		}
	}
	c.Eventually(10*time.Second, "fine pod Ready", readyIs("fine", corev1.ConditionTrue))
	c.Eventually(10*time.Second, "broken pod not Ready", readyIs("broken", corev1.ConditionFalse))

	c.FaultPods(nil)
	c.Eventually(10*time.Second, "broken pod healed", readyIs("broken", corev1.ConditionTrue))
}
