// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// The additions below were driven by counting what these projects actually
// assert, so the tests are shaped the same way as the originals: one table proving each assertion passes on good input, one
// proving it fails on bad, and a dedicated test wherever the interesting part
// is a semantic rather than a comparison.

func TestAddedAssertionsPassOnGoodInput(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	c := NewAborting(r)

	c.Zero(0)
	c.NotZero(1)
	c.Greater(1, 2)
	c.GreaterOrEqual(2, 2)
	c.Less(2, 1)
	c.LessOrEqual(2, 2)
	c.InDelta(1.0, 1.05, 0.1)
	c.StrContains("hello world", "lo w")
	c.NotStrContains("hello", "z")
	c.ElementsMatch([]int{1, 2, 2}, []int{2, 1, 2})
	c.EqualError(errors.New("boom"), "boom")
	c.NotPanics(func() {})
	c.Panics(func() { panic("x") })

	p := &struct{ n int }{}
	c.Same(p, p)
	c.NotSame(p, &struct{ n int }{})

	c.NotNil(p)
	c.Nil(nil)

	if r.failed {
		t.Fatalf("good input should not fail: fatals=%v errors=%v", r.fatals, r.errors)
	}
}

func TestAddedAssertionsFailOnBadInput(t *testing.T) {
	t.Parallel()

	p := &struct{ n int }{}

	for name, call := range map[string]func(*C){
		"Fail":           func(c *C) { c.Fail() },
		"Zero":           func(c *C) { c.Zero(1) },
		"NotZero":        func(c *C) { c.NotZero(0) },
		"Greater":        func(c *C) { c.Greater(2, 1) },
		"GreaterOrEqual": func(c *C) { c.GreaterOrEqual(2, 1) },
		"Less":           func(c *C) { c.Less(1, 2) },
		"LessOrEqual":    func(c *C) { c.LessOrEqual(1, 2) },
		"InDelta":        func(c *C) { c.InDelta(1.0, 2.0, 0.1) },
		"InDeltaNaN":     func(c *C) { c.InDelta(1.0, math.NaN(), 0.1) },
		"InDeltaNegSkew": func(c *C) { c.InDelta(1.0, 1.0, -1) },
		"StrContains":    func(c *C) { c.StrContains("hello", "z") },
		"NotStrContains": func(c *C) { c.NotStrContains("hello", "ell") },
		"ElementsMatch":  func(c *C) { c.ElementsMatch([]int{1, 2}, []int{1, 3}) },
		"EqualErrorNil":  func(c *C) { c.EqualError(nil, "boom") },
		"EqualErrorDiff": func(c *C) { c.EqualError(errors.New("bang"), "boom") },
		"ErrorAs":        func(c *C) { _ = c.ErrorAs[*customErr](errors.New("plain")) },
		"Panics":         func(c *C) { c.Panics(func() {}) },
		"NotPanics":      func(c *C) { c.NotPanics(func() { panic("x") }) },
		"Same":           func(c *C) { c.Same(p, &struct{ n int }{}) },
		"NotSame":        func(c *C) { c.NotSame(p, p) },
		"Nil":            func(c *C) { c.Nil(p) },
		"NotNil":         func(c *C) { c.NotNil(nil) },
	} {
		r := &recorder{}
		call(NewAborting(r))
		if !r.failed {
			t.Errorf("%s: bad input did not fail", name)
		}
	}
}

// TestElementsMatchIsAMultiset pins the distinction from a set comparison.
// [a a b] against [a b b] has the same distinct elements and different
// counts, and treating those as equal is the mistake this assertion exists to
// avoid.
func TestElementsMatchIsAMultiset(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	NewAborting(r).ElementsMatch([]string{"a", "a", "b"}, []string{"a", "b", "b"})
	if !r.failed {
		t.Error("differing duplicate counts should not match")
	}

	r2 := &recorder{}
	NewAborting(r2).ElementsMatch([]string{}, []string{})
	if r2.failed {
		t.Error("two empty slices should match")
	}
}

type customErr struct{ code int }

func (e *customErr) Error() string { return fmt.Sprintf("custom %d", e.code) }

// TestErrorAsReturnsTheMatch pins that the match comes back typed, so a
// caller can assert on its fields without declaring a variable first and
// trusting it was populated.
func TestErrorAsReturnsTheMatch(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	c := NewAborting(r)
	wrapped := fmt.Errorf("context: %w", &customErr{code: 42})

	got := c.ErrorAs[*customErr](wrapped)
	if r.failed {
		t.Fatalf("ErrorAs should have matched: %v %v", r.fatals, r.errors)
	}
	if got == nil || got.code != 42 {
		t.Errorf("want the wrapped *customErr with code 42, got %#v", got)
	}
}

// TestNilSeesThroughATypedNil is the entire reason Nil and NotNil take any.
//
// A nil *T stored in an interface is not nil to the == operator, and is nil
// to everything that dereferences it. An assertion that agreed with == here
// would pass the exact value that panics in production.
func TestNilSeesThroughATypedNil(t *testing.T) {
	t.Parallel()

	var p *customErr
	var v any = p

	// The premise is not asserted at runtime because it cannot fail:
	// staticcheck's SA4023 reports v == nil here as never true, which is the
	// statement this test is built on, proved statically rather than by an
	// if.

	r := &recorder{}
	NewAborting(r).Nil(v)
	if r.failed {
		t.Errorf("Nil should see through a typed nil: %v %v", r.fatals, r.errors)
	}

	r2 := &recorder{}
	NewAborting(r2).NotNil(v)
	if !r2.failed {
		t.Error("NotNil should reject a typed nil")
	}
}

// TestNilRejectsNonNilableKinds guards against Nil quietly becoming a synonym
// for Zero. An int is never nil, and a caller writing Nil(0) meant something
// else and should be told.
func TestNilRejectsNonNilableKinds(t *testing.T) {
	t.Parallel()

	for name, v := range map[string]any{"int": 0, "string": "", "struct": struct{}{}} {
		r := &recorder{}
		NewAborting(r).Nil(v)
		if !r.failed {
			t.Errorf("Nil(%s) should fail; it is not a nilable kind", name)
		}
	}
}

// TestNilAcceptsEveryNilableKind is the other half: the reason this takes any
// is that the nilable kinds have no common constraint, so each one has to
// work.
func TestNilAcceptsEveryNilableKind(t *testing.T) {
	t.Parallel()

	var (
		ch  chan int
		fn  func()
		m   map[string]int
		s   []int
		ptr *customErr
		err error
	)
	for name, v := range map[string]any{
		"chan": ch, "func": fn, "map": m, "slice": s, "pointer": ptr, "interface": err,
	} {
		r := &recorder{}
		NewAborting(r).Nil(v)
		if r.failed {
			t.Errorf("Nil(%s) should pass: %v %v", name, r.fatals, r.errors)
		}
	}
}

// TestPanicsReturnsTheRecoveredValue lets a caller assert on what was
// panicked with rather than only that something was.
func TestPanicsReturnsTheRecoveredValue(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	got := NewAborting(r).Panics(func() { panic("boom") })
	if r.failed {
		t.Fatalf("Panics should have passed: %v %v", r.fatals, r.errors)
	}
	if s, ok := got.(string); !ok || s != "boom" {
		t.Errorf("want the recovered value %q, got %#v", "boom", got)
	}
}

// TestAddedAssertionsCarryTheMessage pins that the trailing msgAndArgs
// convention reaches the new assertions too, since it is the half a generic
// comparator cannot supply.
func TestAddedAssertionsCarryTheMessage(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	NewAborting(r).Greater(10, 1, "shard count for %s", "tenant-a")
	if len(r.fatals) != 1 || !strings.Contains(r.fatals[0], "shard count for tenant-a") {
		t.Errorf("want the caller's message in the failure, got %v", r.fatals)
	}
}

// TestOrderedComparisonsRejectNaN is why the four ordered assertions are
// written as !(got OP want) and carry a nolint for QF1001. Inverting the
// operator as that check suggests would make every one of these pass, since
// NaN compares false against everything.
func TestOrderedComparisonsRejectNaN(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	for name, call := range map[string]func(*C){
		"Greater":        func(c *C) { c.Greater(1.0, nan) },
		"GreaterOrEqual": func(c *C) { c.GreaterOrEqual(1.0, nan) },
		"Less":           func(c *C) { c.Less(1.0, nan) },
		"LessOrEqual":    func(c *C) { c.LessOrEqual(1.0, nan) },
	} {
		r := &recorder{}
		call(NewAborting(r))
		if !r.failed {
			t.Errorf("%s should reject NaN, which is ordered against nothing", name)
		}
	}
}

// TestLengthAssertionsCoverEveryKindWithALength is why Len, Empty and
// NotEmpty take any. Constraining them to ~[]E compiled and then refused a
// map, which is how this was found: converting a real suite produced
// c.Len(map[string]*api.Context, 2) and the build failed.
func TestLengthAssertionsCoverEveryKindWithALength(t *testing.T) {
	t.Parallel()

	for name, v := range map[string]any{
		"slice":  []int{1, 2},
		"map":    map[string]int{"a": 1, "b": 2},
		"string": "ab",
		"array":  [2]int{1, 2},
	} {
		r := &recorder{}
		c := NewCollecting(r)
		c.Len(v, 2)
		c.NotEmpty(v)
		if r.failed {
			t.Errorf("%s: want length 2 and non-empty: %v %v", name, r.fatals, r.errors)
		}
	}

	// A nil slice or map has a length, and it is zero.
	var nilSlice []int
	var nilMap map[string]int
	for name, v := range map[string]any{"nil slice": nilSlice, "nil map": nilMap} {
		r := &recorder{}
		NewCollecting(r).Empty(v)
		if r.failed {
			t.Errorf("%s should be empty, not unmeasurable: %v", name, r.errors)
		}
	}

	// Something with no length is a caller error, not a zero length.
	r := &recorder{}
	NewCollecting(r).Len(42, 0)
	if !r.failed {
		t.Error("Len on an int should refuse rather than report zero")
	}
}

// TestEqDeepComparesUnexportedFields is the difference from EqDiff, and the
// reason both exist: go-cmp panics on an unexported field rather than
// reading it, which turns a mechanical reflect.DeepEqual migration into a
// panic unless the caller is offered this.
func TestEqDeepComparesUnexportedFields(t *testing.T) {
	t.Parallel()

	type hidden struct {
		Shown  string
		hidden int
	}

	r := &recorder{}
	NewCollecting(r).EqDeep(hidden{"a", 1}, hidden{"a", 1})
	if r.failed {
		t.Errorf("identical values should match: %v", r.errors)
	}

	r2 := &recorder{}
	NewCollecting(r2).EqDeep(hidden{"a", 1}, hidden{"a", 2})
	if !r2.failed {
		t.Error("a difference in an unexported field should fail")
	}

	// EqDiff still refuses, which is why this is a second method and not a
	// change to that one.
	defer func() {
		if recover() == nil {
			t.Error("EqDiff should still panic on an unexported field")
		}
	}()
	NewCollecting(&recorder{}).EqDiff(hidden{"a", 1}, hidden{"a", 2})
}

// TestDeepEqualityIsNotTheNegationOfEquality pins why EqDeep and NotEqDeep
// both exist rather than one being the other's !.
//
// Two pointers to identical structs are deeply equal and not ==, so a
// conversion that mapped deep equality onto Eq or NotEq would invert every
// pointer comparison. Kubernetes tests are mostly pointer comparisons.
func TestDeepEqualityIsNotTheNegationOfEquality(t *testing.T) {
	t.Parallel()

	type box struct{ N int }
	p, q := &box{1}, &box{1}

	if p == q {
		t.Fatal("premise broken: distinct pointers should not be ==")
	}

	r := &recorder{}
	c := NewCollecting(r)
	c.EqDeep(p, q) // deeply equal: passes
	c.NotEq(p, q)  // not ==: also passes
	if r.failed {
		t.Errorf("both should hold for distinct pointers to equal values: %v", r.errors)
	}

	// And the two disagree, which is the point.
	r2 := &recorder{}
	NewCollecting(r2).NotEqDeep(p, q)
	if !r2.failed {
		t.Error("NotEqDeep should reject deeply equal values")
	}
}

// NotErrorIs is not NoError. A call can legitimately fail and the test's
// point be that it did not fail in one particular way, which is the case
// that had no spelling here until it was added.
func TestNotErrorIsIsNotNoError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")
	other := errors.New("other")
	wrapped := fmt.Errorf("context: %w", sentinel)

	pass := &recorder{}
	c := NewCollecting(pass)
	c.NotErrorIs(other, sentinel)
	c.NotErrorIs(nil, sentinel)
	// Crucially: a non-nil error that is not the sentinel passes, where
	// NoError would have failed it.
	c.NotErrorIs(other, sentinel)
	if pass.failed {
		t.Fatalf("good input should not fail: %v", pass.errors)
	}

	for name, err := range map[string]error{
		"direct":  sentinel,
		"wrapped": wrapped,
	} {
		r := &recorder{}
		NewCollecting(r).NotErrorIs(err, sentinel)
		if !r.failed {
			t.Errorf("NotErrorIs(%s) should have failed", name)
		}
	}
}

// NotEqDiff closes the same hole NotEqDeep does one level down: == is not the
// negation of a deep comparison, and NotEq does not even compile on a map or
// a slice.
func TestNotEqDiffIsNotNotEq(t *testing.T) {
	t.Parallel()

	type point struct{ X, Y int }

	pass := &recorder{}
	c := NewCollecting(pass)
	c.NotEqDiff([]int{1, 2}, []int{1, 3})
	c.NotEqDiff(map[string]int{"a": 1}, map[string]int{"a": 2})
	c.NotEqDiff(&point{1, 2}, &point{1, 3})
	if pass.failed {
		t.Fatalf("unequal values should not fail: %v", pass.errors)
	}

	// Two distinct pointers to identical structs: NotEq passes on them,
	// because it compares addresses, and this has to fail.
	a, b := &point{1, 2}, &point{1, 2}
	NewCollecting(&recorder{}).NotEq(a, b) // passes; not asserted, just noted

	for name, call := range map[string]func(*C){
		"equal slices":   func(c *C) { c.NotEqDiff([]int{1}, []int{1}) },
		"equal maps":     func(c *C) { c.NotEqDiff(map[string]int{"a": 1}, map[string]int{"a": 1}) },
		"equal pointees": func(c *C) { c.NotEqDiff(a, b) },
	} {
		r := &recorder{}
		call(NewCollecting(r))
		if !r.failed {
			t.Errorf("%s should have failed", name)
		}
	}
}
