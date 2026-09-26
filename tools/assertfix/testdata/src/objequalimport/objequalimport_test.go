// SPDX-License-Identifier: Apache-2.0

package objequalimport // want `imports need the testkit/assert path`

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// This file's only ObjectsAreEqual use is embedded inside a compound
// condition's own fallback conversion, with no standalone site anywhere in
// the file to add the reflect import for it: the import has to ride on this
// call's own enclosing conversion instead, or the file does not compile.
func TestObjectsAreEqualEmbedsWithNoStandaloneSiteToShareTheImport(t *testing.T) { // want `1 assertion can use testkit/assert`
	want, got := []string{"a"}, []string{"a"}
	other := true
	if other || !assert.ObjectsAreEqual(want, got) {
		t.Errorf("mismatch")
	}
}
