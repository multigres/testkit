// SPDX-License-Identifier: Apache-2.0

package cmpdiffcase // want `imports need the testkit/assert path`

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

type thing struct{ N int }

// The common shape: an init-declared diff checked for emptiness, with the
// header the message already carries made redundant by EqDiff's own.
func TestCmpDiffWithWantGotLabel(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	if diff := cmp.Diff(a, b); diff != "" {
		t.Errorf("thing mismatch (-want +got):\n%s", diff)
	}
}

// A label naming the operands in the opposite order needs the arguments to
// trade places, so the header EqDiff prints is still true.
func TestCmpDiffWithGotWantLabelSwapsArguments(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	if diff := cmp.Diff(a, b); diff != "" {
		t.Errorf("thing mismatch (-got +want):\n%s", diff)
	}
}

// No header decoration at all: the message is kept whole, since there is
// nothing here for EqDiff's own header to make redundant.
func TestCmpDiffWithNoLabel(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	if diff := cmp.Diff(a, b); diff != "" {
		t.Errorf("things should be equal, got diff:\n%s", diff)
	}
}

// The plain call in the condition, with no diff variable at all.
func TestCmpDiffBareCondition(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	if cmp.Diff(a, b) != "" {
		t.Error("things did not match")
	}
}

// t.Error has no format string, so cmpDiffMessage takes a separate branch
// from t.Errorf's: this is that branch's own header strip-and-swap, not
// just the shared stripDiffHeader helper it happens to also call.
func TestCmpDiffWithGotWantLabelNoFormatSwapsArguments(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	if diff := cmp.Diff(a, b); diff != "" {
		t.Error("mismatch (-got +want):", diff)
	}
}

// Options already spread as a slice pass straight through to EqDiffOpts.
func TestCmpDiffWithOptionsSlice(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	opts := []cmp.Option{cmpopts.EquateEmpty()}
	if diff := cmp.Diff(a, b, opts...); diff != "" {
		t.Errorf("thing mismatch (-want +got):\n%s", diff)
	}
}

// Explicit option arguments become a slice literal.
func TestCmpDiffWithExplicitOptions(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	if diff := cmp.Diff(a, b, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("thing mismatch (-want +got):\n%s", diff)
	}
}

// A leading argument unrelated to the diff survives the rewrite alongside
// its own verb.
func TestCmpDiffWithALeadingArgument(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	name := "widget"
	if diff := cmp.Diff(a, b); diff != "" {
		t.Errorf("%s should match, but found diff:\n%s", name, diff)
	}
}

// A label naming neither role ("first"/"second" describe position, not
// which one is the expectation) is not guessed at: this keeps the tool's
// older mapping, which still converts but prints the diff as a quoted value
// rather than under EqDiff's own header.
func TestCmpDiffAmbiguousLabelKeepsTheOldMapping(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := thing{1}, thing{2}
	if diff := cmp.Diff(a, b); diff != "" {
		t.Errorf("not deterministic (-first +second):\n%s", diff)
	}
}

// EqDiff and cmp.Diff disagree on whether the two operands need to match:
// EqDiff infers one type parameter for both, cmp.Diff takes any for each.
// Mismatched types here compile as cmp.Diff's arguments and would not as
// EqDiff's, so this keeps the pre-EqDiff mapping instead.
func TestCmpDiffDeclinesOnMismatchedTypes(t *testing.T) { // want `1 assertion can use testkit/assert`
	var got any = "x"
	want := "y"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch:\n%s", diff)
	}
}
