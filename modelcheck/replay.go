// SPDX-License-Identifier: Apache-2.0

package modelcheck

import "fmt"

// replayer rebuilds states the exploration did not keep. A state is rebuilt by
// walking its parent chain from the initial state and, at each step, running
// the recorded actor's Moves on the parent and taking the successor yielded
// under the recorded move name. That is only sound if Moves is deterministic
// (a move name shared by several successors is told apart by yield order), so every step is checked
// against the key hash stored at discovery, and a mismatch panics: a trace
// that silently showed a different state would be worse than none.
type replayer[S State[S]] struct {
	sys   *System[S]
	g     *stateGraph
	names []string
	memo  map[int32]S
}

func newReplayer[S State[S]](sys *System[S], g *stateGraph, names []string) *replayer[S] {
	return &replayer[S]{sys: sys, g: g, names: names, memo: map[int32]S{}}
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
		if h := hashKey(s.Key(), new([]byte)); h != rp.g.hash[i] {
			panic(fmt.Sprintf(
				"modelcheck: replaying node %d produced a state whose Key differs from the one explored: "+
					"Moves must be deterministic (and must not depend on anything outside the state)",
				i,
			))
		}
		rp.memo[i] = s
	}
	return rp.memo[idx]
}

// step re-runs the move that discovered node i from its parent p. Moves that
// share a name are told apart by the order they were yielded in, which is
// stable because Moves is deterministic.
func (rp *replayer[S]) step(p, i int32) S {
	l := rp.g.labels[rp.g.label[i]]
	a := rp.sys.actors[l.actor]
	want := int(rp.g.ordinal[i])
	n := 0
	for move, next := range a.Moves(rp.memo[p].Clone()) {
		if move != l.move {
			continue
		}
		if n == want {
			return next
		}
		n++
	}
	panic(fmt.Sprintf(
		"modelcheck: replaying node %d: actor %q no longer yields move %q (occurrence %d): "+
			"Moves must be deterministic",
		i, rp.names[l.actor], l.move, want,
	))
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
