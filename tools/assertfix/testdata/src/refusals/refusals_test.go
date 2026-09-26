// SPDX-License-Identifier: Apache-2.0

// Package refusals holds the sites assertfix must leave exactly as they are.
// A site left alone costs three lines; a site converted wrongly costs a test,
// silently, because a weakened assertion still passes. There are deliberately
// no `want` comments in this file: the assertion is that nothing is reported.
package refusals

import (
	"fmt"
	"testing"
)

type node struct {
	Name string
	Next *node
}

// The failure message dereferences a value that only the branch proves safe.
// As an assertion argument it would be evaluated on the passing path too, and
// the passing path is exactly where p is nil.
func TestGuardedMessageIsNotHoisted(t *testing.T) {
	var p *node
	if p != nil {
		t.Errorf("got %s", p.Name)
	}
	s := []int{}
	if len(s) != 0 {
		t.Errorf("got %v", s[0])
	}
}

// An if that is another if's else branch cannot be replaced by an
// expression: `} else c.Eq(...)` does not parse.
func TestElseBranchIsNotRewritten(t *testing.T) {
	got := 1
	if got == 0 {
		t.Log("zero")
	} else if got != 1 {
		t.Errorf("got = %d", got)
	}
}

// More than one statement in the failure body is more than one assertion can
// say, so the whole site is skipped rather than guessed at.
func TestMultiStatementBodyIsSkipped(t *testing.T) {
	if 1 != 2 {
		t.Log("about to fail")
		t.Errorf("mismatch")
	}
}

// Inlining the init would evaluate f() twice, because the message wraps err
// for context rather than naming it as prose, and hoisting is not the
// fallback here either: err is declared again later in the same block, so
// hoisting the first one would collide with the second.
func TestInitIsNotInlinedOrHoistedWhenBothAreUnsafe(t *testing.T) {
	if err := f(); err != nil {
		t.Fatal(fmt.Errorf("lookup: %w", err))
	}
	err := g()
	_ = err
}

// Hoisting would shadow an outer err, changing what every reference to it
// after this point means.
func TestInitIsNotHoistedOverAnEnclosingDeclaration(t *testing.T) {
	err := g()
	if err := f(); err != nil {
		t.Fatal(fmt.Errorf("lookup: %w", err))
	}
	_ = err
}

// A compound condition combined with an init is out of scope even though
// each half is handled alone: compound conditions convert whole elsewhere in
// this tool, but not in combination with hoisting or inlining an init.
func TestCompoundConditionWithInitIsNotHoisted(t *testing.T) {
	if exists, err := lookup(); err != nil || exists {
		t.Errorf("mismatch")
	}
}

// Not inside an if at all, so there is no condition to turn into a claim.
func TestBareFailureIsNotAnAssertion(t *testing.T) {
	switch 1 {
	case 2:
		t.Errorf("unreachable")
	}
}

// ObjectsAreEqual is always safe to become reflect.DeepEqual (verified
// against testify's own source; see the assertfix README), but only where
// something converts the call it sits inside, embedding the substitution as
// part of that call's own rewrite. The declined-call shape that shares this
// package but not this file, `EqualValues(t, ObjectsAreEqual(x, y), true)`,
// lives in objequal_declined_call_test.go: it is the one testify import in
// this package, and conversion is all-or-nothing per file.

// Hoisting makes v a real, always-in-scope variable, evaluated as a message
// argument whether the assertion passes or fails. The failing condition here
// is exactly "key present", so v is the zero value (nil) on the passing
// path, and v.Name would panic there once hoisted.
func TestHoistDeclinesWhenTheMessageDereferencesACommaOkMapValue(t *testing.T) {
	m := map[string]*node{}
	if v, ok := m["k"]; ok {
		t.Errorf("unexpected %s", v.Name)
	}
}

// Same shape for a type assertion's comma-ok form: p is nil when ok is
// false, which is the passing path for this condition.
func TestHoistDeclinesWhenTheMessageDereferencesATypeAssertionValue(t *testing.T) {
	var x any = &node{}
	if p, ok := x.(*node); ok {
		t.Errorf("unexpected %s", p.Name)
	}
}

// p is nil on the passing path here too: err == nil means lookupNode found
// nothing, and hoisting would still evaluate p.Name unconditionally as the
// message argument.
func TestHoistDeclinesWhenTheMessageDereferencesAnErrorTupleValue(t *testing.T) {
	if p, err := lookupNode(); err == nil {
		t.Fatalf("want error, got %s", p.Name)
	}
}

func f() error                   { return nil }
func g() error                   { return nil }
func lookup() (bool, error)      { return false, nil }
func lookupNode() (*node, error) { return nil, nil }

// A bounds check guards an index the same way a nil check guards a
// dereference: on the passing path i is -1, so turns[i] as an eager assertion
// argument panics exactly when the test would have passed.
func TestBoundsGuardedMessageIsNotConverted(t *testing.T) {
	turns := []node{{Name: "a"}}
	i := -1
	if i >= 0 {
		t.Fatalf("ejected at %d (%s)", i, turns[i].Name)
	}
	n := 3
	if n < len(turns) {
		t.Errorf("got %v", turns[n:])
	}
}
