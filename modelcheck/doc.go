// SPDX-License-Identifier: Apache-2.0

// Package modelcheck is an explicit-state model checker whose models call real
// implementation code.
//
// A model is a state type, a set of actors that each yield the successor
// states reachable by one move, and properties over states. The actors are
// ordinary Go: they may call the same functions production does, so the
// property is checked against the implementation's decisions rather than a
// restatement of them. Only what cannot be called (I/O, remote handlers) is
// hand-modelled.
//
// Exploration is breadth-first and deduplicated by State.Key, so the trace
// reported for a violation is a shortest one. Two kinds of property are
// checked: an Invariant must hold in every reachable state, and a Recoverable
// goal must stay reachable from every reachable state using only the named
// actors, so a state from which it can no longer be reached is reported as a
// trap.
//
// Only the current and next breadth-first level hold state values. A visited
// state is kept as a few integers (parent, move, edges, the goal results),
// and the states a violation shows are rebuilt by replaying the path from the
// initial state: for each step the recorded actor's Moves is run again on the
// parent and the successor yielded under the recorded move name is taken. That
// needs Moves to be deterministic: the same state must yield the same moves
// in the same order, each leading to a state with the same Key, so it may not
// read the clock, a random source or a Go map in iteration order. Move names
// should be unique per actor per state, since a trace is read by name; moves
// that share one are told apart by yield order. A replay that cannot find the
// move, or reaches a state with a different Key, panics rather than reporting a
// trace that did not happen.
//
// States are expanded concurrently (Options.Workers). Successors are merged in
// the order a sequential search would visit them, so every result, including
// which states a MaxStates bound refuses, is independent of the worker count.
// Moves, Key, Clone and the properties are therefore called from several
// goroutines at once, and must not share mutable data between states.
//
// The search is bounded (Options). It finds bugs and proves nothing: a run
// that reports no violation says only that none exists within the explored
// states, and Result.Truncated says when the bound was hit.
//
// The API is experimental and will change until a second model uses it.
package modelcheck
