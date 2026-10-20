// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"fmt"
	"iter"
	"testing"
)

// State is a model state. A state is an ordinary Go value that may hold
// protos or other mutable data; the engine never mutates one it has stored.
type State[S any] interface {
	// Clone returns a deep copy that shares no mutable data with the receiver.
	Clone() S

	// Key identifies the state: two states with the same Key are the same
	// state. It must be deterministic (sort map keys, never range over a map
	// into the output). Make it multi-line, one fact per line, with the same
	// facts on the same line numbers in every state: violation traces show
	// only the lines that changed between steps, so a readable Key is what
	// makes a trace readable.
	Key() string
}

// Actor is one participant in the model. Moves yields the states reachable
// from s by a single step of this actor, each under a name of the form
// "<verb> <target>", for example "failover p1". Coverage groups by verb.
//
// The engine passes Moves a clone of the stored state, but an actor that yields
// several successors from one call must still build each from its own copy.
type Actor[S any] interface {
	Name() string
	Moves(s S) iter.Seq2[string, S]
}

type invariant[S any] struct {
	name string
	ok   func(S) error
}

type recoverable[S any] struct {
	name   string
	goal   func(S) bool
	actors []string
}

// System is a model under construction: initial states, actors and
// properties. Build one with New and the chained methods, then Explore it.
type System[S State[S]] struct {
	initial      []S
	actors       []Actor[S]
	names        map[string]bool
	invariants   []invariant[S]
	recoverables []recoverable[S]
}

// New starts a system from one or more initial states.
func New[S State[S]](initial ...S) *System[S] {
	return &System[S]{initial: initial, names: map[string]bool{}}
}

// Actor registers a. Registering two actors with the same name panics.
func (sys *System[S]) Actor(a Actor[S]) *System[S] {
	if sys.names[a.Name()] {
		panic(fmt.Sprintf("modelcheck: duplicate actor %q", a.Name()))
	}
	sys.names[a.Name()] = true
	sys.actors = append(sys.actors, a)
	return sys
}

// Attach registers an actor over a component C of the system state, so a
// component model can be written once against its own state type. get
// projects the component out of a system state. set receives a fresh clone of
// the system state and the component the actor produced, and returns the
// system state with the component replaced; it may mutate and return that
// clone. The attached actor keeps a's name.
func (sys *System[S]) Attach[C State[C]](get func(S) C, set func(S, C) S, a Actor[C]) *System[S] {
	return sys.Actor(attached[S, C]{get: get, set: set, inner: a})
}

type attached[S State[S], C State[C]] struct {
	get   func(S) C
	set   func(S, C) S
	inner Actor[C]
}

func (at attached[S, C]) Name() string { return at.inner.Name() }

func (at attached[S, C]) Moves(s S) iter.Seq2[string, S] {
	return func(yield func(string, S) bool) {
		for name, c := range at.inner.Moves(at.get(s)) {
			if !yield(name, at.set(s.Clone(), c)) {
				return
			}
		}
	}
}

// Invariant requires ok to return nil in every reachable state. Only the
// shallowest violating state is reported.
func (sys *System[S]) Invariant(name string, ok func(S) error) *System[S] {
	sys.invariants = append(sys.invariants, invariant[S]{name: name, ok: ok})
	return sys
}

// Recoverable requires that from every reachable state some path made only of
// moves by the named actors reaches a state where goal holds. With no actors
// named, all registered actors count. A state with no such path is a trap, so
// naming only the actors that are expected to do the recovering checks that the
// system does not depend on anything else going right. Explore panics if a
// named actor is not registered.
func (sys *System[S]) Recoverable(name string, goal func(S) bool, actors ...string) *System[S] {
	sys.recoverables = append(
		sys.recoverables,
		recoverable[S]{name: name, goal: goal, actors: actors},
	)
	return sys
}

// Options bounds an exploration.
type Options struct {
	MaxStates int // 0 means 1_000_000
	MaxDepth  int // 0 means unbounded
}

// Step is one position in a trace: the move that produced State.
type Step[S any] struct {
	Actor, Move string
	State       S
}

// Violation is a failed property with the evidence for it.
type Violation[S any] struct {
	Property string
	Kind     string    // "invariant" or "recoverable"
	Err      error     // the invariant's error; nil for recoverable
	Trace    []Step[S] // shortest path from an initial state; Trace[0] has empty Actor and Move
	Exits    []Step[S] // recoverable only: every move available in the trap state
}

// Result summarises an exploration.
type Result[S any] struct {
	States, Edges int
	Truncated     bool           // MaxStates or MaxDepth stopped exploration
	Violations    []Violation[S] // invariants in registration order, then recoverables
	Coverage      map[string]int // "actor/verb" -> times the move was taken; verb is the move name up to its first space
}

// Check explores the system and reports every violation through t.Errorf as a
// formatted trace.
func (sys *System[S]) Check(t testing.TB, opts Options) Result[S] {
	t.Helper()
	res := sys.Explore(opts)
	for _, v := range res.Violations {
		t.Errorf("%s", FormatViolation(v))
	}
	t.Logf("modelcheck: %d states, %d edges, truncated=%v", res.States, res.Edges, res.Truncated)
	return res
}
