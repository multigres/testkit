// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"errors"
	"fmt"
	"iter"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// counterKeys lists the n of each step of a trace.
func counterKeys(steps []Step[counter]) []int {
	var ns []int
	for _, s := range steps {
		ns = append(ns, s.State.n)
	}
	return ns
}

func TestReplayedTraceAndExitsHoldTheExploredStates(t *testing.T) {
	// The invariant violation at 5 is reached by 1+2+2, and the recoverable
	// trap at 7 (past the limit, so the goal 'n is 6' is unreachable) has
	// exits that must carry their own states.
	res := New(counter{}).
		Actor(adder{name: "one", step: 1, limit: 8}).
		Actor(adder{name: "two", step: 2, limit: 8}).
		Invariant("n != 5", notFive).
		Recoverable("six", func(c counter) bool { return c.n == 6 }, "two").
		Explore(Options{Workers: 1})

	if len(res.Violations) != 2 {
		t.Fatalf("violations = %d, want 2", len(res.Violations))
	}
	inv, trap := res.Violations[0], res.Violations[1]
	if got := counterKeys(inv.Trace); !reflect.DeepEqual(got, []int{0, 1, 3, 5}) {
		t.Errorf("invariant trace = %v, want [0 1 3 5]", got)
	}
	if inv.Trace[3].Actor != "two" || inv.Trace[3].Move != "add 2" {
		t.Errorf("last step = %+v, want two: add 2", inv.Trace[3])
	}

	// With only "two" allowed, odd states can never reach 6, so the
	// shallowest trap is 1, whose two exits lead to 2 and 3.
	if trap.Kind != "recoverable" {
		t.Fatalf("second violation = %+v", trap)
	}
	if got := counterKeys(trap.Trace); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("trap trace = %v, want [0 1]", got)
	}
	var exits []string
	for _, e := range trap.Exits {
		exits = append(exits, fmt.Sprintf("%s:%s:%d", e.Actor, e.Move, e.State.n))
	}
	want := []string{"one:add 1:2", "two:add 2:3"}
	if !reflect.DeepEqual(exits, want) {
		t.Errorf("exits = %v, want %v", exits, want)
	}
}

// duplicateNames yields two different successors under one move name.
type duplicateNames struct{}

func (duplicateNames) Name() string { return "dup" }
func (duplicateNames) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n > 0 {
			return
		}
		for _, d := range []int{1, 2} {
			c.n = d
			if !yield("pick", c) {
				return
			}
		}
	}
}

func TestReplayTellsApartMovesSharingAName(t *testing.T) {
	res := New(counter{}).Actor(duplicateNames{}).
		Invariant("n != 2", func(c counter) error {
			if c.n == 2 {
				return errors.New("n is 2")
			}
			return nil
		}).Explore(Options{})
	if len(res.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(res.Violations))
	}
	if got := counterKeys(res.Violations[0].Trace); !reflect.DeepEqual(got, []int{0, 2}) {
		t.Errorf("trace = %v, want [0 2]: the second 'pick' must replay to 2, not 1", got)
	}
}

// drifting behaves as an ordinary adder until armed, then yields a different
// state under the same move name, as a model reading a clock would.
type drifting struct{ armed *atomic.Bool }

func (drifting) Name() string { return "drift" }
func (d drifting) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n >= 4 {
			return
		}
		if d.armed.Load() {
			c.n += 10
		} else {
			c.n++
		}
		yield("add", c)
	}
}

// vanishing yields its move only until armed.
type vanishing struct{ armed *atomic.Bool }

func (vanishing) Name() string { return "vanish" }
func (v vanishing) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n < 4 && !v.armed.Load() {
			c.n++
			yield("add", c)
		}
	}
}

func expectPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatal("Explore did not panic on a nondeterministic Moves")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, want) {
			t.Errorf("panic = %q, want it to contain %q", msg, want)
		}
	}()
	f()
}

func TestNondeterministicMovesFailLoudly(t *testing.T) {
	// The invariant arms the actor when it fires, so exploration is
	// consistent and only the replay that builds the trace diverges.
	t.Run("different key", func(t *testing.T) {
		armed := &atomic.Bool{}
		sys := New(counter{}).Actor(drifting{armed}).Invariant("n != 3", func(c counter) error {
			if c.n == 3 {
				armed.Store(true)
				return errors.New("n is 3")
			}
			return nil
		})
		expectPanic(t, "Key differs", func() { sys.Explore(Options{Workers: 1}) })
	})
	t.Run("missing move", func(t *testing.T) {
		armed := &atomic.Bool{}
		sys := New(counter{}).Actor(vanishing{armed}).Invariant("n != 3", func(c counter) error {
			if c.n == 3 {
				armed.Store(true)
				return errors.New("n is 3")
			}
			return nil
		})
		expectPanic(t, "no longer yields that move", func() { sys.Explore(Options{Workers: 1}) })
	})
}

type panicky struct{}

func (panicky) Name() string { return "panicky" }
func (panicky) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n == 7 {
			panic("model bug at 7")
		}
		c.n++
		yield("add", c)
	}
}

func TestWorkerPanicReachesTheCaller(t *testing.T) {
	expectPanic(t, "model bug at 7", func() {
		New(counter{}).Actor(panicky{}).Explore(Options{Workers: 4, MaxStates: 100})
	})
}

func forcedChunks(t *testing.T) {
	old := chunkNodesPerWorker
	chunkNodesPerWorker = 1
	t.Cleanup(func() { chunkNodesPerWorker = old })
}

// richSystem has invariant and recoverable violations, duplicate edges and
// several actors, on a graph with levels wider than one chunk.
func richSystem() *System[counter] {
	return New(counter{}).
		Actor(adder{name: "one", step: 1, limit: 40}).
		Actor(adder{name: "two", step: 2, limit: 40}).
		Actor(adder{name: "three", step: 3, limit: 40}).
		Actor(multiMover{}).
		Invariant("n != 17", func(c counter) error {
			if c.n == 17 {
				return errors.New("n is 17")
			}
			return nil
		}).
		Invariant("n != 29", func(c counter) error {
			if c.n == 29 {
				return errors.New("n is 29")
			}
			return nil
		}).
		Recoverable("reach 100", func(c counter) bool { return c.n >= 100 }, "one").
		Recoverable("even", func(c counter) bool { return c.n%2 == 0 })
}

func TestResultsAreIndependentOfWorkersAndKeyMode(t *testing.T) {
	forcedChunks(t)
	// 23 and 37 cut the exploration off in the middle of a level; 0 is the
	// default bound, and a depth bound is checked as well.
	for _, opts := range []Options{
		{MaxStates: 23},
		{MaxStates: 37},
		{MaxStates: 37, MaxDepth: 6},
		{},
	} {
		opts.Workers = 1
		want := richSystem().Explore(opts)
		if len(want.Violations) == 0 {
			t.Fatalf("%+v: the fixture must violate something", opts)
		}
		if opts.MaxStates != 0 && !want.Truncated {
			t.Fatalf("%+v: the fixture must be truncated", opts)
		}
		for _, workers := range []int{1, 2, 3, 7, 0} {
			for _, exact := range []bool{false, true} {
				o := opts
				o.Workers, o.ExactKeys = workers, exact
				got := richSystem().Explore(o)
				if !reflect.DeepEqual(got, want) {
					t.Errorf(
						"%+v differs from the sequential result:\n got %+v\nwant %+v",
						o,
						got,
						want,
					)
				}
			}
		}
	}
}

func TestParallelPhilosophersMatchSequential(t *testing.T) {
	forcedChunks(t)
	build := func() *System[table] {
		sys := New(newTable()).Recoverable("someone eats", table.anyAte)
		for i := range 3 {
			sys.Actor(philosopher{id: i})
		}
		return sys
	}
	want := build().Explore(Options{Workers: 1})
	got := build().Explore(Options{Workers: 4})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parallel result differs:\n got %+v\nwant %+v", got, want)
	}
}

func TestKeyIndexGrowsAndFindsEveryKey(t *testing.T) {
	// Past several doublings of the initial table, every node must still be
	// found and a hash never inserted must still be absent.
	const n = 5000
	ix := newKeyIndex(false)
	var hashes []hash128
	var buf []byte
	for i := range n {
		h := hashKey(fmt.Sprint("k", i), &buf)
		hashes = append(hashes, h)
		ix.insert(int32(i), h, "", hashes)
	}
	for i := range n {
		if got, ok := ix.lookup(hashes[i], "", hashes); !ok || int(got) != i {
			t.Fatalf("lookup %d = %d, %v", i, got, ok)
		}
	}
	if _, ok := ix.lookup(hashKey("absent", &buf), "", hashes); ok {
		t.Error("found a key that was never inserted")
	}
}

// goexiter ends its goroutine the way t.FailNow does, at one state.
type goexiter struct{ at int }

func (goexiter) Name() string { return "goexit" }
func (g goexiter) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n == g.at {
			runtime.Goexit()
		}
		if c.n < 20 {
			c.n++
			yield("add", c)
		}
		if c.n < 20 {
			c.n++
			yield("add", c)
		}
	}
}

func TestGoexitOnAWorkerFailsLoudly(t *testing.T) {
	forcedChunks(t)
	// Level 5 of the two-step counter holds several states, so the exit
	// happens on a worker and not inline.
	sys := New(counter{}).Actor(adder{name: "one", step: 1, limit: 20}).
		Actor(adder{name: "two", step: 2, limit: 20}).Actor(goexiter{at: 10})
	expectPanic(t, "runtime.Goexit", func() { sys.Explore(Options{Workers: 4}) })
}

// panicAbove panics on every state at or above a threshold.
type panicAbove struct{ n int }

func (panicAbove) Name() string { return "panicAbove" }
func (p panicAbove) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n >= p.n {
			panic(fmt.Errorf("boom at %d", c.n))
		}
		c.n++
		yield("add", c)
		c.n++
		yield("add2", c)
	}
}

func TestWorkerPanicIsDeterministicAndUnwraps(t *testing.T) {
	forcedChunks(t)
	var sentinel *workerPanic
	for range 20 {
		func() {
			defer func() {
				r := recover()
				err, ok := r.(error)
				if !ok || !errors.As(err, &sentinel) {
					t.Fatalf("panic = %#v, want a *workerPanic", r)
				}
				if got := errors.Unwrap(
					err,
				); got == nil ||
					!strings.HasPrefix(got.Error(), "boom at") {
					t.Errorf("Unwrap = %v, want the original error", got)
				}
			}()
			New(counter{}).Actor(panicAbove{n: 5}).Explore(Options{Workers: 6, MaxStates: 1000})
		}()
		// Several states of one level panic; the reported one must always be
		// the first of them in node order.
		if !strings.Contains(sentinel.Error(), "boom at 5") {
			t.Fatalf(
				"reported %v, want the lowest panicking node (state 5; 6 panics too)",
				sentinel,
			)
		}
	}
}

// unchecked ignores yield's result, as iterators written without early exit
// in mind do.
type unchecked struct{}

func (unchecked) Name() string { return "unchecked" }
func (unchecked) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n > 0 {
			return
		}
		for _, d := range []int{1, 2} {
			c.n = d
			yield("set", c)
		}
	}
}

func TestReplayDrainsActorsThatIgnoreYield(t *testing.T) {
	res := New(counter{}).Actor(unchecked{}).Invariant("n != 1", func(c counter) error {
		if c.n == 1 {
			return errors.New("n is 1")
		}
		return nil
	}).Explore(Options{Workers: 1})
	if res.States != 3 || len(res.Violations) != 1 {
		t.Fatalf("states=%d violations=%d, want 3 and 1", res.States, len(res.Violations))
	}
}

func TestReplayMismatchNamesTheStep(t *testing.T) {
	for _, exact := range []bool{false, true} {
		armed := &atomic.Bool{}
		sys := New(counter{}).Actor(drifting{armed}).Invariant("n != 3", func(c counter) error {
			if c.n == 3 {
				armed.Store(true)
				return errors.New("n is 3")
			}
			return nil
		})
		var msg string
		func() {
			defer func() { msg = fmt.Sprint(recover()) }()
			sys.Explore(Options{Workers: 1, ExactKeys: exact})
		}()
		for _, want := range []string{`actor "drift"`, `move "add"`, "step 1", "occurrence 0"} {
			if !strings.Contains(msg, want) {
				t.Errorf("exact=%v: message lacks %q:\n%s", exact, want, msg)
			}
		}
		if exact && !strings.Contains(msg, `line 1: "n=1" vs "n=10"`) {
			t.Errorf("exact message lacks the key difference:\n%s", msg)
		}
	}
}

// lossy keeps a field Key leaves out, which an armed actor sets differently.
type lossy struct{ n, hidden int }

func (l lossy) Clone() lossy { return l }
func (l lossy) Key() string  { return fmt.Sprintf("n=%d", l.n) }

type hiddenWriter struct{ armed *atomic.Bool }

func (hiddenWriter) Name() string { return "hidden" }
func (h hiddenWriter) Moves(l lossy) iter.Seq2[string, lossy] {
	return func(yield func(string, lossy) bool) {
		if l.n < 3 {
			l.n++
			if h.armed.Load() {
				l.hidden = 0
			} else {
				l.hidden = 1
			}
			yield("add", l)
		}
	}
}

func TestReplayRechecksTheViolatedProperty(t *testing.T) {
	armed := &atomic.Bool{}
	sys := New(lossy{}).Actor(hiddenWriter{armed}).Invariant("hidden unset", func(l lossy) error {
		if l.hidden == 1 && l.n == 3 {
			armed.Store(true)
			return errors.New("hidden is set")
		}
		return nil
	})
	expectPanic(t, "no longer violates", func() { sys.Explore(Options{Workers: 1}) })
}
