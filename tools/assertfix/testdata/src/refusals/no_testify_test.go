// SPDX-License-Identifier: Apache-2.0

// This file holds refusals that need to sit apart from refusals_test.go: it
// imports no testify, so a wrongly-accepted conversion here is not masked by
// that file's own, unrelated whole-file testify block.
package refusals

import (
	"fmt"
	"testing"
)

// The same shadowing risk as TestInitIsNotHoistedOverAnEnclosingDeclaration
// exists even with no reference after the if at all: the enclosing-scope
// check has to catch this on its own, since there is nothing later in the
// block for the later-reference check to find.
func TestInitIsNotHoistedOverAnEnclosingDeclarationWithNoLaterReference(t *testing.T) {
	err := g()
	t.Log(err)
	if err := f(); err != nil {
		t.Fatal(fmt.Errorf("lookup: %w", err))
	}
}

type box struct{ Name string }

// A guard combined with another conjunct in an && used to be recognized and
// declined by name (guardedRoots' && flattening); that mechanism is gone
// now, replaced by messageArgsAreSafe, which does not try to recognize a
// guard at all. want.Name is a selector off a pointer, so it declines
// regardless of what the condition says about want. This is the shape that
// panicked on the operator's own tests before guardedRoots learned to look
// inside a compound condition, which is also gone now: `tc.expectedResult
// != nil && result != *tc.expectedResult`, message `%+v`, `*tc.expectedResult`.
func TestGuardedMessageIsNotConvertedThroughAConjunct(t *testing.T) {
	var want *box
	got := 1
	if want != nil && got != 0 {
		t.Errorf("got %d, want %s", got, want.Name)
	}
}

// Everything below is from review-2's M1 table: messageArgsAreSafe declines
// each regardless of the condition's shape, which is the point of asking a
// question that does not depend on recognizing a guard.

// A parenthesized guard is still just a selector off a pointer in the
// message; the parens around the guard in the condition are irrelevant to
// that.
func TestParenthesizedGuardIsStillUnsafeInAConjunct(t *testing.T) {
	var p *box
	if (p != nil) && p.Name != "y" {
		t.Errorf("got %s", p.Name)
	}
}

// A guard wrapped in a negation is still irrelevant: the message reads
// p.Name regardless of how the condition arrives at needing p non-nil.
func TestGuardInsideANegationIsStillUnsafe(t *testing.T) {
	var p *box
	if !(p == nil || p.Name == "") {
		t.Errorf("got %s", p.Name)
	}
}

// nil on the left of the comparison is still just a selector off a pointer
// in the message.
func TestGuardWithNilOnTheLeftIsStillUnsafe(t *testing.T) {
	var p *box
	if nil != p && p.Name != "" {
		t.Errorf("got %s", p.Name)
	}
}

// `a || b` being false requires both to be false, which used to be read as
// "implies whatever either alone would guard"; it does not. p != nil being
// false on its own is enough to make the whole `||` false, with p nil.
func TestOrConditionDoesNotImplyEitherOperandsGuardHolds(t *testing.T) {
	var p, q *box
	if p != nil || q != nil {
		t.Errorf("got %s", p.Name)
	}
}

// len(s) == 1 was never a recognized guard shape (only != 0 and > 0 were),
// so this was never declined by guard recognition; messageArgsAreSafe
// declines it anyway, since s[0] is an index expression regardless of what
// guards it.
func TestLenEqualsOneIsNotAGuardShape(t *testing.T) {
	s := []string{}
	if len(s) == 1 && s[0] != "x" {
		t.Errorf("got %s", s[0])
	}
}

// A call in the message is a side effect, not a panic risk, and no guard
// shape was ever going to catch that: dump() used to run only on the
// failing path and now would run on every one. messageArgsAreSafe declines
// any call outright rather than reason about which ones are safe to run
// twice.
func TestMessageSideEffectDoesNotRunOnEveryPass(t *testing.T) {
	a, b := 1, 2
	if a != b || b != 1 {
		t.Errorf("mismatch: %s", dump())
	}
}

func dump() string { return "details" }

// The real operator shape (review-2's M1): a length check guards an index,
// joined by &&, with the indexed value read back in the message. On a green
// run len(requests) is 1 and the message never runs; on the regression the
// preceding assertion exists to report, it would have panicked with index
// out of range, on every subsequent subtest in the same test binary.
func TestLenGuardedIndexInTheMessageOperatorShape(t *testing.T) {
	requests := []box{}
	if len(requests) == 1 && requests[0].Name != "cluster-nil" {
		t.Errorf("Expected cluster-nil, got %s", requests[0].Name)
	}
}

// hoistEdit inserts at the start of the if's own line, which is only where
// the if actually begins when nothing shares that line before it. gofmt'd
// source never has this shape; input that has not been through gofmt yet
// can still reach this tool, and inserting at the raw line start here would
// put the hoisted declaration ahead of t.Run, running it before the
// subtest it belongs to. This has to stay on one line to test anything:
// gofmt would give the if its own line and this refusal would no longer
// apply.
func TestOneLineCompoundStatementDeclinesHoisting(t *testing.T) {
	t.Run("sub", func(t *testing.T) { if err := f(); err != nil { t.Fatal(fmt.Errorf("x: %w", err)) } })
}

// A goto anywhere in the function declines hoisting for the whole scope,
// not just a site the goto happens to jump over: got, want is a two-name
// init, so only hoisting could handle it, and there is no fallback to
// inlining for a name this function was never extended to distinguish
// "does this particular goto jump over this particular site" for.
func TestGotoDeclinesAMultiValueHoistAnywhereInTheFunction(t *testing.T) {
	if got, want := 1, 1; got != want {
		t.Errorf("got = %d, want %d", got, want)
	}
	goto L
L:
}

type Inner struct{ Name string }

type Outer struct {
	*Inner
	ID int
}

// o.Name is a promoted field, reached through Outer's embedded *Inner: o
// itself is a value, so checking only v.X's type (Outer, not a pointer)
// admits it, but evaluating the selector still dereferences the embedded
// pointer, and Inner is nil on a zero-value Outer. The original passes;
// the message never runs, so o.Inner's own nilness never confirms it's
// safe to reach through.
func TestPromotedFieldThroughAnEmbeddedPointerIsUnsafe(t *testing.T) {
	o := Outer{}
	if o.Inner != nil && o.ID != 0 {
		t.Errorf("name %s", o.Name)
	}
}

type Namer interface{ Name() string }

type HasNamer struct{ Namer }

// h.Name is a method value, not a field: Kind() != FieldVal excludes it
// for its own reason (forming a method value evaluates its receiver, which
// panics through a nil embedded interface exactly like a nil embedded
// pointer's field does), not because it is a pointer or interface itself.
// The condition is false, so the original never forms h.Name at all.
func TestMethodValueThroughAnEmbeddedNilInterfaceIsUnsafe(t *testing.T) {
	var h HasNamer
	a, b := 1, 2
	if a != 1 && b != 2 {
		t.Errorf("got %v", h.Name)
	}
}
