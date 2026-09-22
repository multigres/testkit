// SPDX-License-Identifier: Apache-2.0

// Package blockedfile holds a file with one testify call this tool declines.
// Conversion is all-or-nothing per file, because both packages want the
// identifier `assert` and only one can have it, so the declined call holds
// back every other site in the file including the stdlib one. No `want`
// comments: the assertion is that nothing at all is reported.
package blockedfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOneDeclinedCallHoldsBackTheWholeFile(t *testing.T) {
	// Convertible on its own.
	assert.NoError(t, doThing())

	// And this one is not: EqualValues is type-coercing equality, which the
	// target package declines.
	assert.EqualValues(t, 1, int64(1))

	// So even the stdlib site below is left alone, because the import
	// cannot be swapped out from under the call above.
	if 1 != 2 {
		t.Errorf("mismatch")
	}
}

func doThing() error { return nil }
