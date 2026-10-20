// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// recorderTB stands in for *testing.T so Check's reporting can be observed
// without failing the real test.
type recorderTB struct {
	testing.TB
	errors []string
	logs   []string
}

func (r *recorderTB) Helper() {}
func (r *recorderTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorderTB) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func notFive(c counter) error {
	if c.n == 5 {
		return errors.New("n is 5")
	}
	return nil
}

func counterSystem() *System[counter] {
	return New(counter{}).
		Actor(adder{name: "one", step: 1}).
		Actor(adder{name: "two", step: 2}).
		Invariant("n != 5", notFive)
}

func TestInvariantViolationShortestTrace(t *testing.T) {
	rec := &recorderTB{}
	res := counterSystem().Check(rec, Options{MaxStates: 100})

	if len(res.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(res.Violations))
	}
	v := res.Violations[0]
	if v.Kind != "invariant" || v.Property != "n != 5" || v.Err == nil {
		t.Fatalf("violation = %+v", v)
	}
	// 2+2+1 is the shortest way to 5: initial plus three moves.
	if len(v.Trace) != 4 {
		t.Fatalf("trace length = %d, want 4", len(v.Trace))
	}
	if v.Trace[0].Actor != "" || v.Trace[0].Move != "" {
		t.Errorf("Trace[0] = %+v, want empty actor and move", v.Trace[0])
	}
	if got := v.Trace[3].State.n; got != 5 {
		t.Errorf("final state n = %d, want 5", got)
	}
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], "invariant n != 5: n is 5") {
		t.Errorf("Check errors = %q", rec.errors)
	}
	if len(rec.logs) != 1 || !strings.HasPrefix(rec.logs[0], "modelcheck: 100 states") {
		t.Errorf("Check logs = %q", rec.logs)
	}
	if !res.Truncated {
		t.Error("an unbounded counter capped at 100 states must be truncated")
	}
}

func TestInvariantCheckedOnInitialState(t *testing.T) {
	res := New(counter{n: 5}).Actor(adder{name: "one", step: 1, limit: 6}).
		Invariant("n != 5", notFive).Explore(Options{})
	if len(res.Violations) != 1 || len(res.Violations[0].Trace) != 1 {
		t.Fatalf("violations = %+v, want one with a one-step trace", res.Violations)
	}
}

func TestMaxStatesTruncates(t *testing.T) {
	res := New(counter{}).Actor(adder{name: "one", step: 1}).Explore(Options{MaxStates: 10})
	if res.States != 10 || !res.Truncated {
		t.Errorf("States=%d Truncated=%v, want 10 and true", res.States, res.Truncated)
	}
}

func TestDiningPhilosophersDeadlock(t *testing.T) {
	sys := New(newTable()).Recoverable("someone eats", table.anyAte)
	for i := range 3 {
		sys.Actor(philosopher{id: i})
	}
	res := sys.Explore(Options{})

	if len(res.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(res.Violations))
	}
	v := res.Violations[0]
	if v.Kind != "recoverable" || v.Err != nil {
		t.Fatalf("violation = %+v", v)
	}
	trap := v.Trace[len(v.Trace)-1].State
	for i := range 3 {
		if trap.forks[i] != i {
			t.Errorf(
				"trap fork%d held by %d, want %d (everyone holds their left fork)",
				i,
				trap.forks[i],
				i,
			)
		}
	}
	if len(v.Exits) != 0 {
		t.Errorf("exits = %+v, want none", v.Exits)
	}
	if len(v.Trace) != 4 {
		t.Errorf("trace length = %d, want 4 (initial plus three pickups)", len(v.Trace))
	}
}

func TestDiningPhilosophersNoDeadlockWithOrdering(t *testing.T) {
	sys := New(newTable()).Recoverable("someone eats", table.anyAte)
	for i := range 3 {
		sys.Actor(philosopher{id: i, rightFirst: i == 2})
	}
	res := sys.Explore(Options{})
	if len(res.Violations) != 0 {
		t.Fatalf("unexpected violations: %s", formatAll(res.Violations))
	}
	if res.Truncated {
		t.Error("the table is finite and must be fully explored")
	}
}

func stuckSystem(actors ...string) *System[vertex] {
	return New(vertex("init")).
		Actor(graph{name: "system", moves: []graphMove{
			{"init", "go", "stuck"},
			{"init", "go", "done"},
		}}).
		Actor(graph{name: "rescuer", moves: []graphMove{{"stuck", "rescue", "done"}}}).
		Recoverable("done", func(n vertex) bool { return n == "done" }, actors...)
}

func TestRecoverableActorSetExcludesRescuer(t *testing.T) {
	res := stuckSystem("system").Explore(Options{})
	if len(res.Violations) != 1 {
		t.Fatalf("with {system}: violations = %d, want 1", len(res.Violations))
	}
	v := res.Violations[0]
	if got := v.Trace[len(v.Trace)-1].State; got != "stuck" {
		t.Errorf("trap = %q, want stuck", got)
	}
	// Exits lists every actor's moves, including ones outside the property's set.
	if len(v.Exits) != 1 || v.Exits[0].Actor != "rescuer" || v.Exits[0].State != "done" {
		t.Errorf("exits = %+v, want the rescuer's move to done", v.Exits)
	}

	if res := stuckSystem("system", "rescuer").Explore(Options{}); len(res.Violations) != 0 {
		t.Errorf("with {system, rescuer}: violations = %d, want 0", len(res.Violations))
	}
	if res := stuckSystem().Explore(Options{}); len(res.Violations) != 0 {
		t.Errorf("with no actors named (all): violations = %d, want 0", len(res.Violations))
	}
}

func TestTruncatedStatesAreNotTraps(t *testing.T) {
	res := New(counter{}).
		Actor(adder{name: "one", step: 1}).
		Recoverable("never", func(counter) bool { return false }).
		Explore(Options{MaxDepth: 3})
	if !res.Truncated {
		t.Error("MaxDepth 3 on an unbounded counter must set Truncated")
	}
	if len(res.Violations) != 0 {
		t.Errorf(
			"violations = %d, want 0: every state reaches the unexplored frontier",
			len(res.Violations),
		)
	}
}

func TestTrapBehindExploredStates(t *testing.T) {
	res := New(vertex("root")).
		Actor(graph{name: "a", moves: []graphMove{
			{"root", "go", "good"},
			{"root", "go", "c1"},
			{"c1", "go", "c2"},
			{"c2", "go", "c1"},
		}}).
		Recoverable("good", func(n vertex) bool { return n == "good" }).
		Explore(Options{MaxDepth: 50})
	if res.Truncated {
		t.Fatal("the graph is small and must be fully explored")
	}
	if len(res.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(res.Violations))
	}
	v := res.Violations[0]
	if got := v.Trace[len(v.Trace)-1].State; got != "c1" {
		t.Errorf("trap = %q, want c1 (the cycle's first state)", got)
	}
	if len(v.Exits) != 1 || v.Exits[0].State != "c2" {
		t.Errorf("exits = %+v, want one move to c2", v.Exits)
	}
}

func TestAttachLens(t *testing.T) {
	sys := New(outer{tag: "x"}).
		Attach(
			func(o outer) counter { return o.inner },
			func(o outer, c counter) outer { o.inner = c; return o },
			adder{name: "inner", step: 1, limit: 3},
		).
		Invariant("outer untouched", func(o outer) error {
			if o.tag != "x" {
				return fmt.Errorf("tag = %q", o.tag)
			}
			return nil
		})
	res := sys.Explore(Options{})
	if res.States != 4 {
		t.Errorf("states = %d, want 4 (inner n in 0..3)", res.States)
	}
	if len(res.Violations) != 0 {
		t.Errorf("violations: %s", formatAll(res.Violations))
	}
	if _, ok := res.Coverage["inner/add"]; !ok {
		t.Errorf("coverage = %v, want the attached actor under its own name", res.Coverage)
	}
}

func panicValue(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

func TestDuplicateActorPanics(t *testing.T) {
	got := panicValue(func() {
		New(counter{}).Actor(adder{name: "a", step: 1}).Actor(adder{name: "a", step: 2})
	})
	if want := `modelcheck: duplicate actor "a"`; got != want {
		t.Errorf("panic = %v, want %q", got, want)
	}
}

func TestUnknownRecoverableActorPanics(t *testing.T) {
	sys := New(counter{}).
		Actor(adder{name: "a", step: 1, limit: 2}).
		Recoverable("p", func(counter) bool { return true }, "a", "ghost")
	got := panicValue(func() { sys.Explore(Options{}) })
	if want := `modelcheck: Recoverable "p" names unknown actor "ghost"`; got != want {
		t.Errorf("panic = %v, want %q", got, want)
	}
}

func TestCoverageByVerb(t *testing.T) {
	res := New(counter{}).Actor(multiMover{}).Explore(Options{})
	// n=0,1,2 each offer both moves; n=3,4 offer none.
	if got := res.Coverage["mover/inc"]; got != 6 {
		t.Errorf(`Coverage["mover/inc"] = %d, want 6`, got)
	}
	if len(res.Coverage) != 1 {
		t.Errorf("coverage = %v, want only the verb key", res.Coverage)
	}
}

func TestFormatViolation(t *testing.T) {
	res := counterSystem().Explore(Options{MaxStates: 100})
	if len(res.Violations) == 0 {
		t.Fatal("no violation to format")
	}
	want := `invariant n != 5: n is 5
  0. initial
     + n=0
     + parity=even
  1. one: add 1
     - n=0
     + n=1
     - parity=even
     + parity=odd
  2. two: add 2
     - n=1
     + n=3
  3. two: add 2
     - n=3
     + n=5
`
	if got := FormatViolation(res.Violations[0]); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	rec := FormatViolation(Violation[vertex]{
		Property: "p",
		Kind:     "recoverable",
		Trace:    []Step[vertex]{{State: "a"}, {Actor: "x", Move: "go b", State: "b"}},
		Exits:    []Step[vertex]{{Actor: "y", Move: "go c", State: "c"}},
	})
	wantRec := `recoverable p: no path to goal
  0. initial
     + a
  1. x: go b
     - a
     + b
  exits from the trap:
    y: go c
`
	if rec != wantRec {
		t.Errorf("got:\n%s\nwant:\n%s", rec, wantRec)
	}
}

// formatAll renders every violation so a failing test shows what was found
// rather than indexing into a slice that may be empty.
func formatAll[S State[S]](vs []Violation[S]) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(FormatViolation(v))
	}
	return b.String()
}
