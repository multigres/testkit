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

type hidden struct{ n int }

func doThing() error { return nil }
