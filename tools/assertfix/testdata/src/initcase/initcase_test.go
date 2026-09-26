// SPDX-License-Identifier: Apache-2.0

package initcase // want `imports need the testkit/assert path`

import (
	"fmt"
	"testing"
)

// The message wraps err for more context rather than naming it as prose, so
// it cannot be inlined: substituting f() into both the assertion and the
// message would evaluate it twice. Hoisting it to its own statement
// evaluates it once either way.
func TestSingleValueHoistsWhenTheMessageNeedsIt(t *testing.T) { // want `1 assertion can use testkit/assert`
	if err := f(); err != nil {
		t.Fatal(fmt.Errorf("lookup: %w", err))
	}
}

// A multi-value init can only be hoisted, never inlined: there is more than
// one name to substitute, and the message-rewriting logic only ever tracks
// one.
func TestMultiValueIndependentPairHoists(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := 1, 2
	if got, want := a, b; got != want {
		t.Errorf("got = %d, want %d", got, want)
	}
}

// The comma-ok idiom is a multi-value init too, even though one name is
// blank.
func TestMultiValueCommaOkHoists(t *testing.T) { // want `1 assertion can use testkit/assert`
	m := map[string]int{"a": 1}
	if _, ok := m["a"]; !ok {
		t.Errorf("missing key")
	}
}

func TestMultiValueErrorTupleHoists(t *testing.T) { // want `1 assertion can use testkit/assert`
	if _, err := lookup(); err == nil {
		t.Errorf("expected an error")
	}
}

// Of two sites in one block sharing the same declared name, only the later
// one can ever hoist: the earlier one always finds a later use of the same
// name and declines rather than risk widening into it.
func TestOnlyTheLastOfARepeatedNameHoists(t *testing.T) { // want `1 assertion can use testkit/assert`
	if err := f(); err != nil {
		t.Fatal(fmt.Errorf("first: %w", err))
	}
	if err := g(); err != nil {
		t.Fatal(fmt.Errorf("second: %w", err))
	}
}

func f() error              { return nil }
func g() error              { return nil }
func lookup() (bool, error) { return false, nil }

type row struct{ want int }

// A later field selector of the same name as a hoisted local does not block
// the hoist: r.want names a struct field, a different namespace entirely
// from the local "want" this hoists into, and isField is what tells them
// apart rather than the identifier's spelling alone.
func TestFieldSelectorOfTheSameNameDoesNotBlockAHoist(t *testing.T) { // want `1 assertion can use testkit/assert`
	r := row{want: 2}
	if got, want := 1, 2; got != want {
		t.Errorf("got = %d, want %d", got, want)
	}
	_ = r.want
}
