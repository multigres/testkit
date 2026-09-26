// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// objectsAreEqualFixes rewrites testify's assert.ObjectsAreEqual(a, b) to
// reflect.DeepEqual(a, b). The two are the same claim for every argument
// type: testify's own ObjectsAreEqual checks `exp == nil || act == nil`
// before ever reaching its []byte special case, which is exactly
// reflect.DeepEqual's own nil-versus-non-nil-empty answer, confirmed against
// the testify source from v1.4.0 through v1.12.1 in this module's cache.
// There is no operand type this needs to refuse.
//
// ObjectsAreEqual is a bool helper, not an assertion: it turns up inside an
// ordinary condition, often a compound one, or as an argument to some other
// call, rather than as its own `assert.X(t, ...)` call, so it needs its own
// pass rather than a testifyMap entry.
//
// A call is rewritten here, as its own independent edit, only when it is
// not nested inside an if statement's own init or condition, nor inside any
// other call's argument list: both are places some OTHER conversion in this
// tool may rewrite wholesale, rendering the call (via render, which already
// substitutes any ObjectsAreEqual it finds) as part of that rewrite's own
// text. Emitting a second, independent edit for the same bytes in that case
// would conflict with the first rather than compose with it; skipping it
// here and leaving the substitution to render is what lets both convert
// without the two edits ever overlapping.
//
// Whether that actually happens is not decided by position: collection,
// which always runs before this, has already called render() on every
// fragment of every other site (each testify call's own arguments, each if
// condition's own pieces), and objEqReplaced (cond.go) already has the
// answer. A call positionally inside a site that converts, but nested past
// a node substituteObjEq was never extended to look inside (another call's
// own arguments passed to something other than ObjectsAreEqual, a
// for/range/switch/var), was never actually reached by any of those render()
// calls and is not counted as fixed: it would survive, unedited, in text
// whose testify import is about to be removed out from under it.
//
// Returns the number of call sites resolved per file, standalone or via
// embedding in a site collection has confirmed will actually be rewritten,
// which the caller folds into the testify-reference count that decides
// whether a file is still blocked: a file whose only testify use was
// ObjectsAreEqual has none left to block it once every site is accounted
// for this way. The caller also uses a non-zero count to add a "reflect"
// import for the file: every resolved call, standalone or embedded, ends up
// calling reflect.DeepEqual somewhere in the final text, so this pass does
// not add that import itself. An embedded call has no diagnostic of its own
// to carry one on, and a standalone call's would otherwise duplicate the
// file-level import edit the caller already builds for the testkit/assert
// swap: two separate insertions at the same import block, one per
// diagnostic, is exactly the overlap this whole design exists to avoid.
func objectsAreEqualFixes(pass *analysis.Pass) map[*ast.File]int {
	fixed := map[*ast.File]int{}
	for _, f := range pass.Files {
		skip := map[*ast.CallExpr]bool{}
		markSkip := func(root ast.Node) {
			if root == nil {
				return
			}
			ast.Inspect(root, func(n ast.Node) bool {
				if ce, ok := n.(*ast.CallExpr); ok {
					skip[ce] = true
				}
				return true
			})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.IfStmt:
				markSkip(v.Init)
				markSkip(v.Cond)
			case *ast.CallExpr:
				for _, a := range v.Args {
					markSkip(a)
				}
			}
			return true
		})

		ast.Inspect(f, func(n ast.Node) bool {
			ce, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			_, _, isObjEq := objectsAreEqualCall(pass, ce)
			if !isObjEq {
				return true
			}
			if skip[ce] {
				if wasObjEqReplaced(pass, ce) {
					fixed[f]++
				}
				return true
			}
			edits := []analysis.TextEdit{{
				Pos:     ce.Pos(),
				End:     ce.End(),
				NewText: []byte(objEqReplacement(pass, ce)),
			}}
			pass.Report(analysis.Diagnostic{
				Pos:     ce.Pos(),
				Message: "assert.ObjectsAreEqual can become reflect.DeepEqual",
				SuggestedFixes: []analysis.SuggestedFix{{
					Message:   "rewrite to reflect.DeepEqual",
					TextEdits: edits,
				}},
			})
			fixed[f]++
			return true
		})
	}
	return fixed
}

// objectsAreEqualCall reports whether ce is a call to testify's
// assert.ObjectsAreEqual, resolved through the type checker rather than the
// identifier's spelling, and returns its two operands.
func objectsAreEqualCall(pass *analysis.Pass, ce *ast.CallExpr) (a, b ast.Expr, ok bool) {
	if len(ce.Args) != 2 {
		return nil, nil, false
	}
	sel, isSel := ce.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "ObjectsAreEqual" {
		return nil, nil, false
	}
	pkgIdent, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return nil, nil, false
	}
	pkgName, isPkg := pass.TypesInfo.Uses[pkgIdent].(*types.PkgName)
	if !isPkg || !strings.HasSuffix(pkgName.Imported().Path(), "stretchr/testify/assert") {
		return nil, nil, false
	}
	return ce.Args[0], ce.Args[1], true
}

func objEqReplacement(pass *analysis.Pass, ce *ast.CallExpr) string {
	a, b, _ := objectsAreEqualCall(pass, ce)
	return fmt.Sprintf("reflect.DeepEqual(%s, %s)", render(pass, a), render(pass, b))
}
