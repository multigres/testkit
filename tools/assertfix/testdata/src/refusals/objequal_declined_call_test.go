// SPDX-License-Identifier: Apache-2.0

// This file holds the one deliberately-blocked testify call in this
// package: EqualValues is declined outright (type-coercing equality, which
// the target package refuses), so there is nothing here to embed the
// ObjectsAreEqual substitution into, and no independent edit is attempted
// either, since that would leave EqualValues's own testify reference
// unresolved regardless, and a second, unrelated diagnostic on the same
// statement besides. Conversion is all-or-nothing per file, so this one
// decline blocks the whole file, which is exactly why it lives alone: every
// other refusal in this package needs to reach the checks it claims to
// test, and sharing a file with this one meant none of them ever did.
package refusals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ObjectsAreEqual is always safe to become reflect.DeepEqual (verified
// against testify's own source; see the assertfix README), but only where
// something converts the call it sits inside, embedding the substitution as
// part of that call's own rewrite.
func TestObjectsAreEqualIsNotRewrittenInsideADeclinedCall(t *testing.T) {
	x, y := 1, 2
	assert.EqualValues(t, assert.ObjectsAreEqual(x, y), true)
}
