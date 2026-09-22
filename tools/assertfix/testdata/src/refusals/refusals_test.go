// SPDX-License-Identifier: Apache-2.0

// Package refusals holds the sites assertfix must leave exactly as they are.
// A site left alone costs three lines; a site converted wrongly costs a test,
// silently, because a weakened assertion still passes. There are deliberately
// no `want` comments in this file: the assertion is that nothing is reported.
package refusals

import "testing"

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

// Inlining the init would evaluate f() twice, because the message names the
// variable as a value rather than as prose.
func TestInitIsNotInlinedWhenTheMessageUsesIt(t *testing.T) {
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

// Not inside an if at all, so there is no condition to turn into a claim.
func TestBareFailureIsNotAnAssertion(t *testing.T) {
	switch 1 {
	case 2:
		t.Errorf("unreachable")
	}
}

func f() error { return nil }
