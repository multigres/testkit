// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"fmt"
	"strings"
)

// replayer rebuilds states the exploration did not keep. A state is rebuilt by
// walking its parent chain from the initial state and, at each step, running
// the recorded actor's Moves on the parent and taking the successor yielded
// under the recorded move name (a name shared by several successors is told
// apart by yield order). That is only sound if Moves is deterministic, so every
// step is checked against the key hash stored at discovery, and a mismatch
// panics: a trace that silently showed a different state would be worse than
// none.
type replayer[S State[S]] struct {
	sys   *System[S]
	g     *stateGraph
	names []string
	exact map[string]int32 // the key index in exact mode, else nil
	memo  map[int32]S
}

func newReplayer[S State[S]](
	sys *System[S], g *stateGraph, names []string, exact map[string]int32,
) *replayer[S] {
	return &replayer[S]{sys: sys, g: g, names: names, exact: exact, memo: map[int32]S{}}
}

// describe names node i for a failure message: its depth in the trace and the
// move that reached it.
func (rp *replayer[S]) describe(i int32) string {
	depth := 0
	for j := i; rp.g.parent[j] >= 0; j = rp.g.parent[j] {
		depth++
	}
	if l := rp.g.label[i]; l >= 0 {
		return fmt.Sprintf("step %d of the trace to node %d (actor %q, move %q, occurrence %d)",
			depth, i, rp.names[rp.g.labels[l].actor], rp.g.labels[l].move, rp.g.ordinal[i])
	}
	return fmt.Sprintf("initial state %d (node %d)", rp.g.roots[i], i)
}

// state returns the state of node idx. Replayed states are shared between
// calls, so callers must treat them as read-only.
func (rp *replayer[S]) state(idx int32) S {
	if s, ok := rp.memo[idx]; ok {
		return s
	}
	var chain []int32
	for i := idx; ; i = rp.g.parent[i] {
		if _, ok := rp.memo[i]; ok {
			break
		}
		chain = append(chain, i)
		if rp.g.parent[i] < 0 {
			break
		}
	}
	for k := len(chain) - 1; k >= 0; k-- {
		i := chain[k]
		var s S
		if p := rp.g.parent[i]; p < 0 {
			s = rp.sys.initial[rp.g.roots[i]].Clone()
		} else {
			s = rp.step(p, i)
		}
		if key := s.Key(); hashKey(key, new([]byte)) != rp.g.hash[i] {
			panic(rp.mismatch(i, key))
		}
		rp.memo[i] = s
	}
	return rp.memo[idx]
}

func (rp *replayer[S]) mismatch(i int32, got string) string {
	msg := fmt.Sprintf(
		"modelcheck: replaying %s reached a state whose Key differs from the one explored, "+
			"so Moves is not deterministic (it may read the clock, a random source or a map "+
			"in iteration order, or depend on something outside the state)", rp.describe(i))
	switch {
	case rp.exact != nil:
		for k, v := range rp.exact { // lookup of one value, order irrelevant
			if v == i {
				msg += "\n  explored vs replayed Key, first difference: " + firstDiff(k, got)
				break
			}
		}
	case rp.g.parent[i] >= 0:
		// Only hashes were kept, so compare two replays of the step with each
		// other: if they disagree the nondeterminism is shown directly.
		again := rp.step(rp.g.parent[i], i).Key()
		if again != got {
			msg += "\n  two replays of the step differ, first difference: " + firstDiff(got, again)
		} else {
			msg += "\n  repeated replays agree with each other; set Options.ExactKeys to see the explored Key"
		}
	}
	return msg
}

// firstDiff reports the first Key line at which a and b differ.
func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for j := range max(len(la), len(lb)) {
		var x, y string
		if j < len(la) {
			x = la[j]
		}
		if j < len(lb) {
			y = lb[j]
		}
		if x != y {
			return fmt.Sprintf("line %d: %q vs %q", j+1, x, y)
		}
	}
	return "none"
}

// step re-runs the move that discovered node i from its parent p. It drains
// Moves rather than stopping at the match, because an iterator that ignores
// yield's result would otherwise panic in the runtime.
func (rp *replayer[S]) step(p, i int32) S {
	l := rp.g.labels[rp.g.label[i]]
	a := rp.sys.actors[l.actor]
	want := int(rp.g.ordinal[i])
	var (
		found S
		ok    bool
		n     int
	)
	for move, next := range a.Moves(rp.memo[p].Clone()) {
		if move != l.move {
			continue
		}
		if n == want {
			found, ok = next, true
		}
		n++
	}
	if !ok {
		panic(fmt.Sprintf(
			"modelcheck: replaying %s: the actor no longer yields that move "+
				"(it yielded the name %d times, occurrence %d was needed): Moves must be deterministic",
			rp.describe(i), n, want))
	}
	return found
}

// trace returns the steps from an initial state to node idx.
func (rp *replayer[S]) trace(idx int32) []Step[S] {
	var rev []int32
	for i := idx; i >= 0; i = rp.g.parent[i] {
		rev = append(rev, i)
	}
	steps := make([]Step[S], 0, len(rev))
	for k := len(rev) - 1; k >= 0; k-- {
		i := rev[k]
		st := Step[S]{State: rp.state(i)}
		if l := rp.g.label[i]; l >= 0 {
			st.Actor, st.Move = rp.names[rp.g.labels[l].actor], rp.g.labels[l].move
		}
		steps = append(steps, st)
	}
	return steps
}

// confirm re-checks, on the replayed final state of a violation's trace, the
// fact the violation rests on. Key is deliberately lossy in models, so a
// replay can reproduce the Key and still differ in a field the property reads;
// that would show a trace that does not demonstrate the violation.
func (rp *replayer[S]) confirm(
	trace []Step[S], holds func(S) bool, property, outcome string,
) []Step[S] {
	last := trace[len(trace)-1].State
	if !holds(last) {
		panic(fmt.Sprintf(
			"modelcheck: the replayed final state of the trace for %q %s, although its Key "+
				"matches the explored one: Moves is not deterministic in a field Key leaves out",
			property, outcome))
	}
	return trace
}
