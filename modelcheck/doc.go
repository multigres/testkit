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
// The search is bounded (Options). It finds bugs and proves nothing: a run
// that reports no violation says only that none exists within the explored
// states, and Result.Truncated says when the bound was hit.
//
// The API is experimental and will change until a second model uses it.
package modelcheck
