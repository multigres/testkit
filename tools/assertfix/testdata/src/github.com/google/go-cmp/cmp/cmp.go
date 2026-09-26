// SPDX-License-Identifier: Apache-2.0

// Package cmp is a stub of github.com/google/go-cmp/cmp, carrying only the
// signatures the cmp.Diff-to-EqDiff tests need to type-check.
//
// A stub rather than the real dependency, on the same terms as the testify
// stub beside it: analysistest resolves imports out of testdata/src as a
// GOPATH, and tools/assertfix's own go.mod does not carry go-cmp (it needs
// golang.org/x/tools and nothing else).
package cmp

type Option any

func Diff(x, y any, opts ...Option) string { return "" }
