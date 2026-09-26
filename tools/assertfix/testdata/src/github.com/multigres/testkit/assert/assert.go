// SPDX-License-Identifier: Apache-2.0

// Package assert is a stub of github.com/multigres/testkit/assert, carrying
// only what the upgrade-path tests need to type-check: a call already using
// this package's own vocabulary, resolved back to it by the analyzer the
// same way a testify call is resolved to testify's.
//
// A stub rather than the real dependency, on the same terms as the testify
// stub beside it: analysistest resolves imports out of testdata/src as a
// GOPATH, and tools/assertfix does not otherwise depend on testkit/assert at
// all, on purpose (see this module's own doc comment).
package assert

type TB interface {
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

type C struct {
	TB
	check bool
}

func NewAborting(t TB) *C   { return &C{TB: t} }
func NewCollecting(t TB) *C { return &C{TB: t, check: true} }

func (c *C) Eq(want, got any, msgAndArgs ...any) {}

func (c *C) Require() *C {
	out := *c
	out.check = false
	return &out
}

func (c *C) Check() *C {
	out := *c
	out.check = true
	return &out
}
