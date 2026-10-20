// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"fmt"
	"strings"
)

const defaultMaxStates = 1_000_000

type edge struct {
	actor, move string
	to          int
}

type node[S any] struct {
	state        S
	depth        int
	parent       int // -1 for an initial state
	actor, move  string
	edges        []edge
	unknownAfter bool // some successor was not explored, so the future is unknown
}

// Explore searches breadth-first from the initial states and evaluates every
// property.
//
// A bound never produces a recoverable violation by itself: a state that was
// not expanded (at MaxDepth) and a state with a successor refused by
// MaxStates both have an unknown future, so they count as able to reach the
// goal and are never reported as traps.
func (sys *System[S]) Explore(opts Options) Result[S] {
	allowed := sys.resolveRecoverables()
	maxStates := opts.MaxStates
	if maxStates == 0 {
		maxStates = defaultMaxStates
	}

	res := Result[S]{Coverage: map[string]int{}}
	var nodes []*node[S]
	index := map[string]int{}
	invViolations := make([]*Violation[S], len(sys.invariants))

	// add returns the index of the state, discovering it if new. ok is false
	// when it is new but the MaxStates bound refuses it.
	add := func(s S, depth, parent int, actor, move string) (idx int, ok bool) {
		key := s.Key()
		if i, seen := index[key]; seen {
			return i, true
		}
		if len(nodes) >= maxStates {
			res.Truncated = true
			return 0, false
		}
		idx = len(nodes)
		nodes = append(
			nodes,
			&node[S]{state: s, depth: depth, parent: parent, actor: actor, move: move},
		)
		index[key] = idx
		for i, inv := range sys.invariants {
			if invViolations[i] != nil {
				continue
			}
			if err := inv.ok(s); err != nil {
				invViolations[i] = &Violation[S]{
					Property: inv.name,
					Kind:     "invariant",
					Err:      err,
					Trace:    traceTo(nodes, idx),
				}
			}
		}
		return idx, true
	}

	for _, s := range sys.initial {
		add(s.Clone(), 0, -1, "", "")
	}

	// nodes grows while it is walked; discovery order is breadth-first.
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		if opts.MaxDepth > 0 && n.depth >= opts.MaxDepth {
			n.unknownAfter = true
			res.Truncated = true
			continue
		}
		for _, a := range sys.actors {
			for move, next := range a.Moves(n.state.Clone()) {
				res.Coverage[a.Name()+"/"+verb(move)]++
				to, ok := add(next, n.depth+1, i, a.Name(), move)
				if !ok {
					n.unknownAfter = true
					continue
				}
				n.edges = append(n.edges, edge{actor: a.Name(), move: move, to: to})
				res.Edges++
			}
		}
	}
	res.States = len(nodes)

	for _, v := range invViolations {
		if v != nil {
			res.Violations = append(res.Violations, *v)
		}
	}
	for i, r := range sys.recoverables {
		if v := findTrap(nodes, r, allowed[i]); v != nil {
			res.Violations = append(res.Violations, *v)
		}
	}
	return res
}

// resolveRecoverables returns, per recoverable property, the set of actor
// names whose moves count towards reaching the goal.
func (sys *System[S]) resolveRecoverables() []map[string]bool {
	sets := make([]map[string]bool, len(sys.recoverables))
	for i, r := range sys.recoverables {
		set := map[string]bool{}
		if len(r.actors) == 0 {
			for name := range sys.names {
				set[name] = true
			}
		}
		for _, name := range r.actors {
			if !sys.names[name] {
				panic(
					fmt.Sprintf("modelcheck: Recoverable %q names unknown actor %q", r.name, name),
				)
			}
			set[name] = true
		}
		sets[i] = set
	}
	return sets
}

// findTrap returns the shallowest explored state from which the goal cannot be
// reached using only the allowed actors. States whose futures were cut off by
// a bound count as able to reach the goal: they are unknown, not trapped.
func findTrap[S State[S]](
	nodes []*node[S],
	r recoverable[S],
	allowed map[string]bool,
) *Violation[S] {
	rev := make([][]int, len(nodes))
	for from, n := range nodes {
		for _, e := range n.edges {
			if allowed[e.actor] {
				rev[e.to] = append(rev[e.to], from)
			}
		}
	}

	reach := make([]bool, len(nodes))
	var queue []int
	for i, n := range nodes {
		if n.unknownAfter || r.goal(n.state) {
			reach[i] = true
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		for _, from := range rev[i] {
			if !reach[from] {
				reach[from] = true
				queue = append(queue, from)
			}
		}
	}

	// Discovery order is non-decreasing in depth, so the first trap is a
	// shallowest one.
	for i, n := range nodes {
		if reach[i] {
			continue
		}
		v := &Violation[S]{Property: r.name, Kind: "recoverable", Trace: traceTo(nodes, i)}
		for _, e := range n.edges {
			v.Exits = append(
				v.Exits,
				Step[S]{Actor: e.actor, Move: e.move, State: nodes[e.to].state},
			)
		}
		return v
	}
	return nil
}

func traceTo[S any](nodes []*node[S], idx int) []Step[S] {
	var rev []Step[S]
	for i := idx; i >= 0; i = nodes[i].parent {
		n := nodes[i]
		rev = append(rev, Step[S]{Actor: n.actor, Move: n.move, State: n.state})
	}
	steps := make([]Step[S], len(rev))
	for i, s := range rev {
		steps[len(rev)-1-i] = s
	}
	return steps
}

func verb(move string) string {
	v, _, _ := strings.Cut(move, " ")
	return v
}
