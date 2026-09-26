// SPDX-License-Identifier: Apache-2.0

package errwhencase // want `imports need the testkit/assert path`

import "testing"

// The operands can be written in either order: `wantErr != (err != nil)`
// says the same thing as `(err != nil) != wantErr`, and a human picks
// whichever reads best at the call site.
func TestErrorWhenSwappedOperandsStillConvert(t *testing.T) { // want `1 assertion can use testkit/assert`
	wantErr := false
	err := doThing()
	if wantErr != (err != nil) {
		t.Errorf("mismatch")
	}
}

// The inner and outer operators have to agree: `(err != nil) == wantErr` is
// a different claim from `(err != nil) != wantErr`, the negation of it, so
// this keeps the generic boolean mapping (NotEq) rather than ErrorWhen,
// which would otherwise assert the opposite of what the condition says.
func TestErrorWhenDeclinesOnMismatchedOperators(t *testing.T) { // want `1 assertion can use testkit/assert`
	wantErr := false
	err := doThing()
	if (err != nil) == wantErr {
		t.Errorf("mismatch")
	}
}

// Two independent nil comparisons is not the ErrorWhen shape at all, even
// though both operands are themselves `x != nil` expressions: neither one
// is a bool named "wantErr" for the other to be compared against, so this
// keeps the generic boolean mapping.
func TestErrorWhenDeclinesOnTwoUnrelatedNilComparisons(t *testing.T) { // want `1 assertion can use testkit/assert`
	err1 := doThing()
	err2 := doThing()
	if (err1 != nil) != (err2 != nil) {
		t.Errorf("mismatch")
	}
}

// A boolean boxed in an interface still type-checks against (err != nil), a
// plain bool, because a concrete type compares against an interface it
// implements. ErrorWhen takes wantErr as a concrete bool, so the interface
// value cannot go there unconverted; this keeps the generic boolean mapping.
func TestErrorWhenDeclinesWhenTheBooleanOperandIsBoxedInAnInterface(t *testing.T) { // want `1 assertion can use testkit/assert`
	var wantErr any = false
	err := doThing()
	if (err != nil) != wantErr {
		t.Errorf("mismatch")
	}
}

func doThing() error { return nil }
