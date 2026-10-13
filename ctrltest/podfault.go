// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// PodFault describes how DataPlaneSim should report a Pod as broken. The zero
// value, like a nil *PodFault, is a healthy pod.
//
// Container names cover init containers as well as regular ones, and what a
// fault means depends on which kind it names, following kubelet:
//
//   - A regular container or a sidecar (an init container with restartPolicy
//     Always) named in NotReady runs but fails its readiness probe; in
//     CrashLoop it sits in CrashLoopBackOff. Either way ContainersReady, and
//     so Ready, is False.
//   - A regular init container named in NotReady never completes; in
//     CrashLoop it sits in Init:CrashLoopBackOff. Either way the pod stays
//     Pending with Initialized False, every later init container and every
//     regular container waits in PodInitializing, and nothing is Ready.
//   - A crashlooping sidecar blocks the init containers after it the same way,
//     because kubelet starts the next one only once a sidecar has started. A
//     NotReady sidecar has started, so it does not.
//
// Restarts are modelled only as status: RestartCount climbs on kubelet's
// backoff (10s doubling to 5m), and nothing actually restarts.
type PodFault struct {
	// Unscheduled reports the pod as the scheduler leaves one it cannot
	// place: Pending, PodScheduled False with reason Unschedulable, no podIP
	// and no container statuses. Envtest has no binding to undo, so this
	// applies even to a pod the simulator already started.
	Unscheduled bool

	// NotReady names containers that run but never pass readiness.
	NotReady []string

	// CrashLoop names containers stuck in CrashLoopBackOff.
	CrashLoop []string
}

// PodFaultFunc decides, on every sweep, which fault a pod has. It returns nil
// for a healthy pod.
//
// A predicate rather than a list of pod names, because the pods a test wants
// broken are usually defined by what they run, not what they are called: a
// rollout replaces them with new UIDs and often the same names, and "every pod
// on image X" has to keep matching across that.
type PodFaultFunc func(*corev1.Pod) *PodFault

// podFaultRule is one namespace's installed PodFaultFunc, with where to report
// a fault that names a container the pod does not have.
type podFaultRule struct {
	fn     PodFaultFunc
	report func(format string, args ...any)

	// reported holds "<pod uid>/<container>" for every unknown name already
	// reported, so a sweep every quarter second reports each one once.
	reported map[string]bool
}

// SetPodFaults installs fn for every pod in ns, replacing any earlier one. A
// nil fn heals the namespace. report receives one message per pod and
// unknown container name a fault mentions, since such a fault does nothing
// and a test relying on it would otherwise pass for the wrong reason.
//
// Once SetPodFaults returns, the simulator will not call the previous report
// again, which is what lets C.FaultPods hand it a test's Errorf and clear it in
// Cleanup without a late sweep logging into a finished test.
func (s *DataPlaneSim) SetPodFaults(
	ns string,
	fn PodFaultFunc,
	report func(format string, args ...any),
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn == nil {
		delete(s.faults, ns)
		return
	}
	if s.faults == nil {
		s.faults = map[string]*podFaultRule{}
	}
	s.faults[ns] = &podFaultRule{fn: fn, report: report, reported: map[string]bool{}}
}

// podFault evaluates the namespace's rule for p, reporting unknown container
// names. The rule's fn runs outside the lock, so a slow predicate cannot stall
// SetPodFaults, and the report is re-checked under it, so a rule replaced in
// the meantime is never reported through.
func (s *DataPlaneSim) podFault(p *corev1.Pod) *PodFault {
	s.mu.Lock()
	rule := s.faults[p.Namespace]
	s.mu.Unlock()
	if rule == nil {
		return nil
	}
	f := rule.fn(p)
	if f == nil {
		return nil
	}

	var unknown []string
	for _, name := range slices.Concat(f.NotReady, f.CrashLoop) {
		if !podHasContainer(p, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return f
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.faults[p.Namespace] != rule || rule.report == nil {
		return f
	}
	for _, name := range unknown {
		key := string(p.UID) + "/" + name
		if rule.reported[key] {
			continue
		}
		rule.reported[key] = true
		rule.report("PodFault for pod %s/%s names container %q, which the pod does not have",
			p.Namespace, p.Name, name)
	}
	return f
}

func podHasContainer(p *corev1.Pod, name string) bool {
	for _, c := range slices.Concat(p.Spec.InitContainers, p.Spec.Containers) {
		if c.Name == name {
			return true
		}
	}
	return false
}

// containerHealth is what a fault makes of one container.
type containerHealth int

const (
	healthy containerHealth = iota
	notReady
	crashLoop
)

func (f *PodFault) health(name string) containerHealth {
	switch {
	case f == nil:
		return healthy
	case slices.Contains(f.CrashLoop, name):
		return crashLoop
	case slices.Contains(f.NotReady, name):
		return notReady
	default:
		return healthy
	}
}

// crashLoopBackoff is kubelet's restart backoff: 10s, doubling, capped at 5m.
func crashLoopBackoff(restarts int32) time.Duration {
	d := 10 * time.Second
	for i := int32(1); i < restarts && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// podStatus computes the status kubelet and the scheduler would report for p
// under f, from p's current status. Timestamps are carried over wherever the
// state they date has not changed, so a pod that already matches comes back
// equal under podStatusShape and the sweep writes nothing.
func (s *DataPlaneSim) podStatus(p *corev1.Pod, f *PodFault, now metav1.Time) corev1.PodStatus {
	st := *p.Status.DeepCopy()

	if f != nil && f.Unscheduled {
		st.Phase = corev1.PodPending
		st.PodIP, st.PodIPs, st.HostIP, st.HostIPs = "", nil, "", nil
		st.StartTime = nil
		st.ContainerStatuses, st.InitContainerStatuses = nil, nil
		setCondition(&st, corev1.PodScheduled, false, "Unschedulable", now)
		setCondition(&st, corev1.PodInitialized, false, "", now)
		setCondition(&st, corev1.ContainersReady, false, "ContainersNotReady", now)
		setCondition(&st, corev1.PodReady, false, "ContainersNotReady", now)
		return st
	}

	if st.PodIP == "" {
		s.podIPSeq++
		st.PodIP = fmt.Sprintf("10.244.%d.%d", s.podIPSeq/250, s.podIPSeq%250+1)
		st.PodIPs = []corev1.PodIP{{IP: st.PodIP}}
		st.HostIP = "10.0.0.1"
	}
	if st.StartTime == nil {
		st.StartTime = &now
	}

	// Init containers start in order, and one that has not finished (or, for a
	// sidecar, not started) holds back everything after it.
	blocked := false
	allReady := true
	inits := make([]corev1.ContainerStatus, 0, len(p.Spec.InitContainers))
	for _, c := range p.Spec.InitContainers {
		prev := findContainerStatus(p.Status.InitContainerStatuses, c.Name)
		sidecar := c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		var cs corev1.ContainerStatus
		switch {
		case blocked:
			cs = waitingStatus(c, prev, "PodInitializing")
		case sidecar:
			cs = runningContainerStatus(c, prev, f.health(c.Name), now)
			blocked = cs.Started == nil || !*cs.Started
			allReady = allReady && cs.Ready
		default:
			switch f.health(c.Name) {
			case healthy:
				cs = completedStatus(c, prev, now)
			case notReady:
				cs = runningContainerStatus(c, prev, notReady, now)
				blocked = true
			case crashLoop:
				cs = runningContainerStatus(c, prev, crashLoop, now)
				blocked = true
			}
		}
		inits = append(inits, cs)
	}

	mains := make([]corev1.ContainerStatus, 0, len(p.Spec.Containers))
	for _, c := range p.Spec.Containers {
		prev := findContainerStatus(p.Status.ContainerStatuses, c.Name)
		var cs corev1.ContainerStatus
		if blocked {
			cs = waitingStatus(c, prev, "PodInitializing")
		} else {
			cs = runningContainerStatus(c, prev, f.health(c.Name), now)
		}
		allReady = allReady && cs.Ready
		mains = append(mains, cs)
	}
	st.InitContainerStatuses, st.ContainerStatuses = inits, mains

	st.Phase = corev1.PodRunning
	if blocked {
		st.Phase = corev1.PodPending
	}
	containersReady := allReady && !blocked
	gatesReady := true
	for _, g := range p.Spec.ReadinessGates {
		gatesReady = gatesReady && conditionTrue(st.Conditions, g.ConditionType)
	}
	readyReason := ""
	switch {
	case !containersReady:
		readyReason = "ContainersNotReady"
	case !gatesReady:
		readyReason = "ReadinessGatesNotReady"
	}

	setCondition(&st, corev1.PodScheduled, true, "", now)
	setCondition(
		&st,
		corev1.PodInitialized,
		!blocked,
		notReason(blocked, "ContainersNotInitialized"),
		now,
	)
	setCondition(&st, corev1.ContainersReady, containersReady,
		notReason(!containersReady, "ContainersNotReady"), now)
	setCondition(&st, corev1.PodReady, containersReady && gatesReady, readyReason, now)
	return st
}

func notReason(failing bool, reason string) string {
	if failing {
		return reason
	}
	return ""
}

func conditionTrue(conds []corev1.PodCondition, t corev1.PodConditionType) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// setCondition sets t's status and reason, moving LastTransitionTime only when
// the status changes, as kubelet does.
func setCondition(
	st *corev1.PodStatus,
	t corev1.PodConditionType,
	ok bool,
	reason string,
	now metav1.Time,
) {
	status := corev1.ConditionFalse
	if ok {
		status = corev1.ConditionTrue
	}
	for i := range st.Conditions {
		c := &st.Conditions[i]
		if c.Type != t {
			continue
		}
		if c.Status != status {
			c.LastTransitionTime = now
		}
		c.Status, c.Reason = status, reason
		return
	}
	st.Conditions = append(st.Conditions, corev1.PodCondition{
		Type:               t,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: now,
	})
}

func findContainerStatus(list []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func baseContainerStatus(c corev1.Container, prev *corev1.ContainerStatus) corev1.ContainerStatus {
	cs := corev1.ContainerStatus{
		Name:        c.Name,
		Image:       c.Image,
		ImageID:     "sha256:" + c.Name,
		ContainerID: "containerd://" + c.Name,
	}
	if prev != nil {
		cs.RestartCount = prev.RestartCount
		cs.LastTerminationState = prev.LastTerminationState
	}
	return cs
}

// runningContainerStatus covers a container kubelet has started: Running and
// Ready when healthy, Running and not Ready when failing readiness, and
// Waiting in CrashLoopBackOff, with RestartCount climbing on kubelet's backoff,
// when crashlooping.
func runningContainerStatus(
	c corev1.Container,
	prev *corev1.ContainerStatus,
	h containerHealth,
	now metav1.Time,
) corev1.ContainerStatus {
	cs := baseContainerStatus(c, prev)
	if h == crashLoop {
		cs.Started = ptr.To(false)
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
		var last *corev1.ContainerStateTerminated
		if prev != nil {
			last = prev.LastTerminationState.Terminated
		}
		if last == nil || now.Sub(last.FinishedAt.Time) >= crashLoopBackoff(cs.RestartCount) {
			cs.RestartCount++
			cs.LastTerminationState = corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   1,
					Reason:     "Error",
					FinishedAt: now,
				},
			}
		}
		return cs
	}
	cs.Started = ptr.To(true)
	cs.Ready = h == healthy
	if prev != nil && prev.State.Running != nil {
		cs.State.Running = prev.State.Running
	} else {
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: now}
	}
	return cs
}

func completedStatus(
	c corev1.Container,
	prev *corev1.ContainerStatus,
	now metav1.Time,
) corev1.ContainerStatus {
	cs := baseContainerStatus(c, prev)
	cs.Ready = true
	cs.Started = ptr.To(false)
	if prev != nil && prev.State.Terminated != nil && prev.State.Terminated.ExitCode == 0 {
		cs.State.Terminated = prev.State.Terminated
	} else {
		cs.State.Terminated = &corev1.ContainerStateTerminated{
			ExitCode:   0,
			Reason:     "Completed",
			FinishedAt: now,
		}
	}
	return cs
}

func waitingStatus(
	c corev1.Container,
	prev *corev1.ContainerStatus,
	reason string,
) corev1.ContainerStatus {
	cs := baseContainerStatus(c, prev)
	cs.Started = ptr.To(false)
	cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: reason}
	return cs
}

// FaultPods breaks pods in this case's namespace: on every sweep, the data
// plane simulator reports each pod as fn says, and as healthy where fn
// returns nil. A later call replaces fn; FaultPods(nil) heals the namespace.
// The fault is lifted when the test ends.
//
// A fault naming a container its pod does not have fails the test, once per
// pod and name, because it would otherwise silently do nothing.
func (c *C) FaultPods(fn PodFaultFunc) {
	c.Helper()
	sim := c.Suite.sim
	if sim == nil {
		c.Fatalf(
			"FaultPods needs the data plane simulator, and Options.SimInterval is not positive",
		)
	}
	if fn == nil {
		sim.SetPodFaults(c.NS, nil, nil)
		return
	}
	sim.SetPodFaults(c.NS, fn, c.Errorf)
	c.Cleanup(func() { sim.SetPodFaults(c.NS, nil, nil) })
}
