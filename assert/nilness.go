// SPDX-License-Identifier: Apache-2.0

package assert

import "reflect"

// Nil and NotNil are the two assertions here that take any rather than a type
// parameter, and that is deliberate rather than a gap in the thesis.
//
// Go cannot express "nilable" as a constraint: pointers, maps, slices,
// channels, funcs and interfaces are nilable, and no type set covers exactly
// those. A generic form would have to pick one kind, and the measured call
// sites are spread across all of them.
//
// Taking any also buys something the language operator does not give you. An
// interface holding a nil pointer is not nil:
//
//	var p *T          // nil
//	var v any = p     // v != nil, because v has a type
//
// which is the trap behind a large share of "impossible" nil-pointer panics.
// These assertions look through to the dynamic value, so NotNil(v) fails on
// that value where v != nil would have passed it. Stronger than ==, not
// looser.
//
// For a typed pointer where the compile-time check is available and worth
// more, Eq((*T)(nil), got) still works and catches a type mismatch.

// isNil reports whether v is nil, or is a non-nil interface holding a nil
// value of a nilable kind.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		// A non-nilable kind is never nil. Notably this makes Nil(0) and
		// Nil("") fail rather than quietly pass, which is what a caller who
		// meant Zero should find out.
		return false
	}
}

// Nil fails unless v is nil, including an interface holding a nil pointer.
func (c *C) Nil(v any, msgAndArgs ...any) {
	c.Helper()
	if !isNil(v) {
		c.fail("want nil, got %v (%T)%s", v, v, msg(msgAndArgs))
	}
}

// NotNil fails if v is nil, including an interface holding a nil pointer.
//
// That second case is the reason to use this rather than v != nil: a nil *T
// in an any is not nil to the operator, and is nil to everything that then
// dereferences it.
func (c *C) NotNil(v any, msgAndArgs ...any) {
	c.Helper()
	if isNil(v) {
		c.fail("want a non-nil value, got %v (%T)%s", v, v, msg(msgAndArgs))
	}
}

// lengthOf returns len(v) for any kind len accepts, and false otherwise.
//
// Separate from the assertions so Len, Empty and NotEmpty give the same
// answer to "is this even measurable", and so the one refusal case is written
// once. A nil slice, map or channel has length zero rather than being
// unmeasurable, which matches the language.
func lengthOf(v any) (int, bool) {
	if v == nil {
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.String, reflect.Array, reflect.Chan:
		return rv.Len(), true
	default:
		return 0, false
	}
}
