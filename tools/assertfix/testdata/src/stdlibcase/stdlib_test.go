// SPDX-License-Identifier: Apache-2.0

package stdlibcase // want `imports need the testkit/assert path`

import (
	"reflect"
	"testing"
)

// One assertion in a scope gets no receiver declaration: naming a variable to
// use it once costs two lines.
func TestSingleAssertionIsInlined(t *testing.T) { // want `1 assertion can use testkit/assert`
	got := "a"
	if got != "b" {
		t.Errorf("got = %q, want %q", got, "b")
	}
}

// Several assertions share a receiver, inserted after the t.Parallel preamble
// rather than at the top of the body.
func TestSeveralAssertionsShareAReceiver(t *testing.T) { // want `3 assertion\(s\) can use testkit/assert`
	t.Parallel()

	got, want := 1, 2
	if got != want {
		t.Errorf("got = %d, want %d", got, want)
	}
	if len("abc") != 3 {
		t.Errorf("len")
	}
	if err := doThing(); err != nil {
		t.Fatalf("doThing: %v", err)
	}
}

// A scope mixing Errorf and Fatalf gets a collecting receiver, with Require()
// on the aborting sites, so no test changes behaviour.
func TestMixedModesKeepTheirAbortSemantics(t *testing.T) { // want `2 assertion\(s\) can use testkit/assert`
	if err := doThing(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if 1 != 2 {
		t.Errorf("mismatch")
	}
}

// A slice cannot be compared with Eq, which is generic over comparable, so
// the type checker picks the diffing form instead. This is the decision a
// regex cannot make, and the reason this tool is an analysis pass.
func TestSliceComparisonBecomesADiff(t *testing.T) { // want `1 assertion can use testkit/assert`
	got, want := []int{1}, []int{2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// Whereas a comparable operand keeps ==, and a struct holding an unexported
// field goes to EqDeep rather than EqDiff, because go-cmp panics where
// reflect.DeepEqual reads it.
func TestComparableAndUnexportedSplitApart(t *testing.T) { // want `2 assertion\(s\) can use testkit/assert`
	if got := 1; got != 2 {
		t.Errorf("got = %d, want %d", got, 2)
	}
	got, want := hidden{1}, hidden{2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hidden: got = %v, want %v", got, want)
	}
}

// ErrorWhen replaces the generic Eq mapping the tool would otherwise pick
// for (err != nil) != wantErr: a bare boolean comparison can't say which
// direction the mismatch went, and ErrorWhen's own message can.
func TestErrorWhenReplacesTheBooleanComparison(t *testing.T) { // want `1 assertion can use testkit/assert`
	wantErr := false
	err := doThing()
	if (err != nil) != wantErr {
		t.Errorf("doThing() error = %v, wantErr %v", err, wantErr)
	}
}

// A concrete error type keeps the generic boolean comparison instead of
// ErrorWhen: getFieldErr returns *fieldErr, not error, and a nil check on
// that concrete pointer is not the same claim as a nil check on the error
// interface ErrorWhen takes. Storing a nil *fieldErr into an error would
// make err != nil true, the opposite of what the concrete comparison here
// says; the fallback keeps the original expression exactly as written, so
// no such boxing ever happens.
func TestErrorWhenDeclinesOverAConcreteErrorType(t *testing.T) { // want `1 assertion can use testkit/assert`
	wantErr := false
	err := getFieldErr(wantErr)
	if (err != nil) != wantErr {
		t.Errorf("getFieldErr() error = %v, wantErr %v", err, wantErr)
	}
}

// The == spelling falls back the same way.
func TestErrorWhenDeclinesOverAConcreteErrorTypeEQL(t *testing.T) { // want `1 assertion can use testkit/assert`
	wantErr := false
	err := getFieldErr(wantErr)
	if (err == nil) == wantErr {
		t.Errorf("mismatch")
	}
}

// The same hole existed for NoError/Error before this fix: a concrete
// *fieldErr falls back to Nil/NotNil, which read through to the pointer's
// own nilness with no interface in between, rather than NoError, which
// would ask the interface's nilness instead.
func TestNoErrorDeclinesOverAConcreteErrorType(t *testing.T) { // want `1 assertion can use testkit/assert`
	err := getFieldErr(false)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// A leading t.Skip is often unconditional, left in for a future PR to lift,
// so the receiver goes after it rather than above: declaring it above an
// unconditional Skip left it never reached, and staticcheck's SA4006
// reported it as never used.
func TestReceiverGoesAfterALeadingSkip(t *testing.T) { // want `2 assertion\(s\) can use testkit/assert`
	t.Skip("not yet supported")

	got, want := 1, 2
	if got != want {
		t.Errorf("got = %d, want %d", got, want)
	}
	if len("abc") != 3 {
		t.Errorf("len")
	}
}

// Two interface values compared for != is identity, not value equality, so
// comparableOperands refuses it (an interface can hold an incomparable
// dynamic type and panic at runtime), and there is no more specific
// assertion for identity either. The whole condition still becomes the
// generic boolean fallback rather than being left alone.
func TestInterfaceIdentityComparisonBecomesTheBooleanFallback(t *testing.T) { // want `1 assertion can use testkit/assert`
	var a, b transport
	if a != b {
		t.Errorf("mismatch")
	}
}

type transport interface{ RoundTrip() }

// t.Fatal has no format string, so err would otherwise print twice: once
// through NoError's own message, once again as this bare, redundant
// trailing argument. dropRedundant removes it.
func TestFatalDropsAnArgumentAlreadyInTheAssertion(t *testing.T) { // want `1 assertion can use testkit/assert`
	err := doThing()
	if err != nil {
		t.Fatal(err)
	}
}

type hidden struct{ n int }

type fieldErr struct{ msg string }

func (e *fieldErr) Error() string { return e.msg }

func getFieldErr(fail bool) *fieldErr {
	if fail {
		return &fieldErr{msg: "boom"}
	}
	return nil
}

func doThing() error { return nil }

// An index the condition itself evaluates is safe on both paths, so it does
// not block conversion the way a bounds-guarded one does.
func TestIndexAlreadyInTheConditionIsConverted(t *testing.T) { // want `1 assertion can use testkit/assert`
	got, want := []int{1}, []int{1}
	i := 0
	if got[i] != want[i] {
		t.Errorf("at %d: got %d", i, got[i])
	}
}

type client struct{}

// A parameter the body never mentions still owns its name in the function's
// scope, so the receiver cannot be `c`.
func TestReceiverAvoidsAnUnusedParameter(t *testing.T) {
	check := func(t *testing.T, c *client) { // want `2 assertion\(s\) can use testkit/assert`
		if 1 != 2 {
			t.Errorf("one")
		}
		if 3 != 4 {
			t.Errorf("three")
		}
	}
	check(t, nil)
}
