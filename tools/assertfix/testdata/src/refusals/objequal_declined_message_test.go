// SPDX-License-Identifier: Apache-2.0

// This file's one site renders fine, embedding and replacing its nested
// ObjectsAreEqual as part of the whole condition becoming True/False, and
// still declines afterward on the message. Isolated in its own file rather
// than alongside objequal_unreachable_test.go's own refusals: those are
// never reached at all by substituteObjEq regardless of timing, so mixing
// this one in with them left this file's own testify import as extra
// weight keeping the file blocked either way, which is exactly the kind of
// thing that let review-3's m2 go unnoticed by any existing test.
package refusals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type widget struct{ Name string }

// The condition renders fine (embedding and replacing the nested
// ObjectsAreEqual as part of it), and still declines afterward on the
// message: p.Name is a selector off a pointer, unsafe by messageArgsAreSafe
// regardless of what the condition says about p. Counting the substitution
// anyway, the way collect() did before review-3's m2 fix, would remove the
// testify import this call still needs, since this is the file's only
// ObjectsAreEqual site and nothing else in it converts either.
func TestObjectsAreEqualEmbeddedInADeclinedMessageIsNotCounted(t *testing.T) {
	a, b := 1, 2
	var p *widget
	if !assert.ObjectsAreEqual(a, b) {
		t.Errorf("mismatch %s", p.Name)
	}
}
