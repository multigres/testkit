// SPDX-License-Identifier: Apache-2.0

package assert

import "cmp"

// The ordered comparisons live in their own file because they need the
// standard library's cmp, and assert.go already binds that identifier to
// github.com/google/go-cmp/cmp. Aliasing either one would leave every reader
// of those files checking which cmp they are looking at.
//
// Each comparison below is written as !(got OP want) rather than the
// inverted operator staticcheck's QF1001 suggests, and the nolint on each is
// load bearing. cmp.Ordered includes the floats, and IEEE comparison is not
// a total order: with got = NaN, !(got > want) is true and fails, which is
// right, while got <= want is false and would pass. De Morgan does not hold
// where a value compares false against everything, so the "simplification"
// would silently accept NaN in all four of these.

// Greater fails unless got is greater than want.
//
// Argument order is (want, got) to match Eq, which puts the expectation
// first. Read it as "greater than want".
func (c *C) Greater[T cmp.Ordered](want, got T, msgAndArgs ...any) {
	c.Helper()
	//nolint:staticcheck // QF1001: unsound for NaN, see the note above.
	if !(got > want) {
		c.fail("want a value greater than %v, got %v%s", want, got, msg(msgAndArgs))
	}
}

// GreaterOrEqual fails unless got is greater than or equal to want.
func (c *C) GreaterOrEqual[T cmp.Ordered](want, got T, msgAndArgs ...any) {
	c.Helper()
	//nolint:staticcheck // QF1001: unsound for NaN, see the note above.
	if !(got >= want) {
		c.fail("want a value >= %v, got %v%s", want, got, msg(msgAndArgs))
	}
}

// Less fails unless got is less than want.
func (c *C) Less[T cmp.Ordered](want, got T, msgAndArgs ...any) {
	c.Helper()
	//nolint:staticcheck // QF1001: unsound for NaN, see the note above.
	if !(got < want) {
		c.fail("want a value less than %v, got %v%s", want, got, msg(msgAndArgs))
	}
}

// LessOrEqual fails unless got is less than or equal to want.
func (c *C) LessOrEqual[T cmp.Ordered](want, got T, msgAndArgs ...any) {
	c.Helper()
	//nolint:staticcheck // QF1001: unsound for NaN, see the note above.
	if !(got <= want) {
		c.fail("want a value <= %v, got %v%s", want, got, msg(msgAndArgs))
	}
}

// InDelta fails unless got is within delta of want.
//
// Float comparison, and constrained to floats deliberately: an integer
// tolerance is almost always a sign the caller wanted GreaterOrEqual and
// LessOrEqual. NaN fails on either side, since no tolerance contains it, and
// a negative delta is a programming error rather than an empty window.
func (c *C) InDelta[T ~float32 | ~float64](want, got, delta T, msgAndArgs ...any) {
	c.Helper()
	if delta < 0 {
		c.fail("InDelta needs a non-negative delta, got %v", delta)
		return
	}
	if want != want || got != got { // NaN on either side
		c.fail("want %v within %v of %v, and NaN is within nothing%s",
			got, delta, want, msg(msgAndArgs))
		return
	}
	diff := want - got
	if diff < 0 {
		diff = -diff
	}
	if diff > delta {
		c.fail("want %v within %v of %v, off by %v%s",
			got, delta, want, diff, msg(msgAndArgs))
	}
}
