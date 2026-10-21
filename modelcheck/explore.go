// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"fmt"
	"math"
	"runtime"
	"strings"
)

const defaultMaxStates = 1_000_000

// chunkNodesPerWorker bounds how many frontier nodes are expanded before their
// successors are merged, which bounds the successor states buffered at once to
// a small multiple of the worker count instead of a whole level. It is a
// variable so tests can force chunk boundaries onto tiny graphs.
var chunkNodesPerWorker = 64

// label is one (actor, move) pair. Interning them lets a node and an edge each
// carry one int32 instead of two strings.
type label struct {
	actor int
	move  string
}

// stateGraph is the explored state graph, kept column-wise. It deliberately holds no
// state values: those are rebuilt by replay for the few violations that need
// them, so memory per state is a handful of integers.
type stateGraph struct {
	// Per node, in discovery order.
	parent  []int32  // -1 for an initial state
	label   []int32  // the move that discovered the node; -1 for an initial state
	ordinal []uint16 // among same-named moves of that label from the parent, which one
	hash    []hash128
	unknown []bool   // some successor was not explored, so the future is unknown
	goal    [][]bool // per recoverable, the goal evaluated once at discovery

	// Edges in CSR form: node i owns edgeTo[edgeStart[i]:edgeStart[i+1]].
	edgeStart []uint32
	edgeLabel []int32
	edgeTo    []int32

	labels []label
	// roots[i] is the index in System.initial of node i, for the first
	// len(roots) nodes, which are exactly the initial states.
	roots []int32
}

// Explore searches breadth-first from the initial states and evaluates every
// property.
//
// A bound never produces a recoverable violation by itself: a state that was
// not expanded (at MaxDepth) and a state with a successor refused by
// MaxStates both have an unknown future, so they count as able to reach the
// goal and are never reported as traps.
//
// Results do not depend on Options.Workers: the merge visits successors in the
// order a sequential search would, so node indices, truncation, coverage,
// edges and violations are the same for any worker count.
func (sys *System[S]) Explore(opts Options) Result[S] {
	allowed := sys.resolveRecoverables()
	maxStates := opts.MaxStates
	if maxStates == 0 {
		maxStates = defaultMaxStates
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	e := &explorer[S]{
		sys:       sys,
		maxStates: maxStates,
		workers:   workers,
		g:         &stateGraph{edgeStart: []uint32{0}, goal: make([][]bool, len(sys.recoverables))},
		index:     newKeyIndex(opts.ExactKeys),
		labelIDs:  map[label]int32{},
		bufs:      make([][]byte, workers),
		invViol:   make([]*invViolation, len(sys.invariants)),
	}
	for _, a := range sys.actors {
		e.names = append(e.names, a.Name())
	}
	e.run(opts.MaxDepth)

	res := Result[S]{
		States:    len(e.g.parent),
		Edges:     len(e.g.edgeTo),
		Truncated: e.truncated,
		Coverage:  map[string]int{},
	}
	for id, n := range e.labelCount {
		l := e.g.labels[id]
		res.Coverage[e.names[l.actor]+"/"+verb(l.move)] += n
	}

	rp := newReplayer(sys, e.g, e.names)
	for i, v := range e.invViol {
		if v != nil {
			res.Violations = append(res.Violations, Violation[S]{
				Property: sys.invariants[i].name,
				Kind:     "invariant",
				Err:      v.err,
				Trace:    rp.trace(v.node),
			})
		}
	}
	for i, r := range sys.recoverables {
		if v := findTrap(e.g, e.names, rp, r.name, allowed[i], i); v != nil {
			res.Violations = append(res.Violations, *v)
		}
	}
	return res
}

type invViolation struct {
	node int32
	err  error
}

type explorer[S State[S]] struct {
	sys       *System[S]
	maxStates int
	workers   int
	names     []string
	g         *stateGraph
	index     *keyIndex
	labelIDs  map[label]int32
	bufs      [][]byte // per-worker scratch for hashing keys
	truncated bool
	invViol   []*invViolation
	// labelCount counts every move yielded under a label, including moves to
	// states already seen or refused by MaxStates, as coverage does.
	labelCount []int
	// seenStamp[label] == stamp marks a label already yielded by the current
	// (node, actor); seenN then counts how many times, to number moves that
	// share a name.
	seenStamp []uint64
	seenN     []uint16
	stamp     uint64
}

// succ is a successor produced by a worker, awaiting the ordered merge.
type succ[S any] struct {
	actor int
	move  string
	state S
	hash  hash128
	key   string // only kept in exact-key mode
}

func (e *explorer[S]) run(maxDepth int) {
	sys := e.sys
	var cur []S
	for k, s := range sys.initial {
		s = s.Clone()
		key := s.Key()
		h := hashKey(key, &e.bufs[0])
		if _, seen := e.index.lookup(h, key, e.g.hash); seen {
			continue
		}
		if len(e.g.parent) >= e.maxStates {
			e.truncated = true
			continue
		}
		e.addNode(-1, -1, 0, h, key)
		e.g.roots = append(e.g.roots, int32(k))
		cur = append(cur, s)
	}
	e.evaluate(cur, 0)

	levelStart := 0
	for depth := 0; len(cur) > 0; depth++ {
		var next []S
		if maxDepth > 0 && depth >= maxDepth {
			for range cur {
				e.g.unknown[levelStart] = true
				e.g.edgeStart = append(e.g.edgeStart, offset32(len(e.g.edgeTo)))
				levelStart++
			}
			e.truncated = true
			return
		}
		chunk := max(1, e.workers*chunkNodesPerWorker)
		for lo := 0; lo < len(cur); lo += chunk {
			hi := min(lo+chunk, len(cur))
			next = e.expandChunk(cur[lo:hi], levelStart+lo, next)
			// Release the expanded states now rather than at the end of the
			// level: they are the bulk of the live heap.
			clear(cur[lo:hi])
		}
		levelStart += len(cur)
		cur = next
	}
}

// expandChunk expands states, whose node indices start at base, in parallel,
// then merges their successors in node, actor, move order. New states are
// appended to next, which is returned.
func (e *explorer[S]) expandChunk(states []S, base int, next []S) []S {
	out := make([][]succ[S], len(states))
	parallelFor(e.workers, len(states), func(w, i int) {
		out[i] = e.expand(states[i], &e.bufs[w])
	})

	nextBase := len(next)
	for i := range out {
		e.merge(base+i, out[i], &next)
		out[i] = nil
		e.g.edgeStart = append(e.g.edgeStart, offset32(len(e.g.edgeTo)))
	}
	e.evaluate(next[nextBase:], len(e.g.parent)-(len(next)-nextBase))
	return next
}

// expand yields every successor of s. It must not touch shared mutable
// explorer state.
func (e *explorer[S]) expand(s S, buf *[]byte) []succ[S] {
	var out []succ[S]
	for ai, a := range e.sys.actors {
		in := s
		// The last actor may take the frontier state itself: it is dropped
		// right after expansion and every property was evaluated when it was
		// discovered, so nobody reads it again.
		if ai < len(e.sys.actors)-1 {
			in = s.Clone()
		}
		for move, next := range a.Moves(in) {
			key := next.Key()
			sc := succ[S]{actor: ai, move: move, state: next, hash: hashKey(key, buf)}
			if e.index.exact != nil {
				sc.key = key
			}
			out = append(out, sc)
		}
	}
	return out
}

// merge applies one node's successors in the order the moves were yielded.
func (e *explorer[S]) merge(from int, succs []succ[S], next *[]S) {
	g := e.g
	e.stamp++
	prevActor := -1
	for i := range succs {
		sc := &succs[i]
		if sc.actor != prevActor {
			e.stamp++
			prevActor = sc.actor
		}
		id := e.intern(label{sc.actor, sc.move})
		if e.seenStamp[id] == e.stamp {
			if e.seenN[id] == math.MaxUint16 {
				panic(fmt.Sprintf(
					"modelcheck: actor %q yields move %q more than %d times from one state",
					e.names[sc.actor], sc.move, math.MaxUint16,
				))
			}
			e.seenN[id]++
		} else {
			e.seenStamp[id] = e.stamp
			e.seenN[id] = 0
		}
		e.labelCount[id]++

		to, seen := e.index.lookup(sc.hash, sc.key, g.hash)
		if !seen {
			if len(g.parent) >= e.maxStates {
				e.truncated = true
				g.unknown[from] = true
				continue
			}
			to = e.addNode(index32(from), id, e.seenN[id], sc.hash, sc.key)
			*next = append(*next, sc.state)
		}
		g.edgeLabel = append(g.edgeLabel, id)
		g.edgeTo = append(g.edgeTo, to)
	}
}

func (e *explorer[S]) intern(l label) int32 {
	if id, ok := e.labelIDs[l]; ok {
		return id
	}
	id := index32(len(e.g.labels))
	e.g.labels = append(e.g.labels, l)
	e.labelIDs[l] = id
	e.labelCount = append(e.labelCount, 0)
	e.seenStamp = append(e.seenStamp, 0)
	e.seenN = append(e.seenN, 0)
	return id
}

func (e *explorer[S]) addNode(parent, lbl int32, ord uint16, h hash128, key string) int32 {
	g := e.g
	idx := index32(len(g.parent))
	g.parent = append(g.parent, parent)
	g.label = append(g.label, lbl)
	g.ordinal = append(g.ordinal, ord)
	g.hash = append(g.hash, h)
	g.unknown = append(g.unknown, false)
	e.index.insert(idx, h, key, g.hash)
	return idx
}

// evaluate runs the invariants and recoverable goals over newly discovered
// states, whose nodes are base, base+1, ... in order. Properties are pure, so
// the work runs in parallel; results are recorded in node order so the first
// violation of each invariant is the one a sequential search finds.
func (e *explorer[S]) evaluate(states []S, base int) {
	sys := e.sys
	if len(states) == 0 {
		return
	}
	var live []int
	for i := range sys.invariants {
		if e.invViol[i] == nil {
			live = append(live, i)
		}
	}
	goals := make([][]bool, len(sys.recoverables))
	for i := range goals {
		goals[i] = make([]bool, len(states))
	}
	errs := make([]error, len(states)*len(live))
	parallelFor(e.workers, len(states), func(_, j int) {
		for k, i := range live {
			errs[j*len(live)+k] = sys.invariants[i].ok(states[j])
		}
		for i, r := range sys.recoverables {
			goals[i][j] = r.goal(states[j])
		}
	})
	for i := range goals {
		e.g.goal[i] = append(e.g.goal[i], goals[i]...)
	}
	for j := range states {
		for k, i := range live {
			if err := errs[j*len(live)+k]; err != nil && e.invViol[i] == nil {
				e.invViol[i] = &invViolation{node: index32(base + j), err: err}
			}
		}
	}
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
	g *stateGraph,
	names []string,
	rp *replayer[S],
	name string,
	allowed map[string]bool,
	rec int,
) *Violation[S] {
	n := len(g.parent)
	ok := make([]bool, len(g.labels))
	for id, l := range g.labels {
		ok[id] = allowed[names[l.actor]]
	}

	// Reverse adjacency in CSR form, over allowed edges only.
	revStart := make([]uint32, n+2)
	for from := range n {
		for k := g.edgeStart[from]; k < g.edgeStart[from+1]; k++ {
			if ok[g.edgeLabel[k]] {
				revStart[g.edgeTo[k]+2]++
			}
		}
	}
	for i := 2; i < len(revStart); i++ {
		revStart[i] += revStart[i-1]
	}
	rev := make([]int32, revStart[n+1])
	for from := range n {
		for k := g.edgeStart[from]; k < g.edgeStart[from+1]; k++ {
			if ok[g.edgeLabel[k]] {
				to := g.edgeTo[k]
				rev[revStart[to+1]] = index32(from)
				revStart[to+1]++
			}
		}
	}

	reach := make([]bool, n)
	var queue []int32
	for i := range n {
		if g.unknown[i] || g.goal[rec][i] {
			reach[i] = true
			queue = append(queue, int32(i))
		}
	}
	for head := 0; head < len(queue); head++ {
		i := queue[head]
		for _, from := range rev[revStart[i]:revStart[i+1]] {
			if !reach[from] {
				reach[from] = true
				queue = append(queue, from)
			}
		}
	}

	// Discovery order is non-decreasing in depth, so the first trap is a
	// shallowest one.
	for i := range n {
		if reach[i] {
			continue
		}
		v := &Violation[S]{Property: name, Kind: "recoverable", Trace: rp.trace(int32(i))}
		for k := g.edgeStart[i]; k < g.edgeStart[i+1]; k++ {
			l := g.labels[g.edgeLabel[k]]
			v.Exits = append(
				v.Exits,
				Step[S]{Actor: names[l.actor], Move: l.move, State: rp.state(g.edgeTo[k])},
			)
		}
		return v
	}
	return nil
}

// index32 and offset32 narrow node, label and edge counts. The engine would
// need tens of gigabytes of graph before either overflows, so overflow is a
// loud failure rather than a case to handle.
func index32(n int) int32 {
	if n > math.MaxInt32 {
		panic("modelcheck: more than 2^31 states or labels")
	}
	return int32(n) //nolint:gosec // bounded by the check above
}

func offset32(n int) uint32 {
	if n > math.MaxUint32 {
		panic("modelcheck: more than 2^32 edges")
	}
	return uint32(n) //nolint:gosec // bounded by the check above
}

func verb(move string) string {
	v, _, _ := strings.Cut(move, " ")
	return v
}
