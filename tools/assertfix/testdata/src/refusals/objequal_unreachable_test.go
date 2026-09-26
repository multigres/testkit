// SPDX-License-Identifier: Apache-2.0

// This file's ObjectsAreEqual calls sit where substituteObjEq cannot reach
// them: inside another call's own arguments (not a testify call, and not
// ObjectsAreEqual itself), and inside a for/range loop, a node this
// function was never extended to descend into. Both look, positionally,
// like they are embedded inside a site that converts, and once did get
// wrongly counted as resolved on exactly that reasoning (review-2's m1):
// the site's rendered text still contained the literal ObjectsAreEqual
// call afterward, with the testify import that call needs already removed
// out from under it. objEqReplaced (cond.go) now answers "did render()
// actually reach and replace this one", not "is it positionally inside a
// site that converts", so both of these correctly keep this file's testify
// import and leave everything in it alone.
package refusals

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkTrue(b bool) bool { return b }

func TestObjectsAreEqualNestedInsideAnUnrelatedCallIsNotCounted(t *testing.T) {
	a, b := 1, 2
	if !checkTrue(assert.ObjectsAreEqual(a, b)) {
		t.Error("mismatch")
	}
}

func TestObjectsAreEqualInsideAForLoopIsNotCounted(t *testing.T) {
	xs := []int{1, 2, 3}
	require.Eventually(t, func() bool {
		for _, x := range xs {
			if !assert.ObjectsAreEqual(x, 0) {
				return false
			}
		}
		return true
	}, 0, 0)
}
