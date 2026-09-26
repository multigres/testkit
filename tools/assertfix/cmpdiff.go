// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// The cmp.Diff half of the tool.
//
// `if diff := cmp.Diff(a, b); diff != "" { t.Errorf("x mismatch (-want
// +got):\n%s", diff) }` mapped onto Eq("", cmp.Diff(a, b), "x mismatch
// (-want +got):\n") before this, which compiles and passes but prints the
// diff as a quoted "got" value rather than under its own header. EqDiff (and
// EqDiffOpts, for a cmp.Diff call carrying options) already print a
// "(-want +got)" diff themselves, so this maps onto those instead and
// strips the now-redundant decoration from the carried message.

// cmpDiffCall recognises cmp.Diff(a, b) and cmp.Diff(a, b, opts...),
// resolving the package through the type checker rather than by the
// identifier's spelling: a local variable or a differently-imported package
// can also be called cmp, and the old Eq("", cmp.Diff(...)) mapping never
// cared what Diff it was calling, since it only ever compared the result to
// "". EqDiff swaps in go-cmp's own comparison semantics, which is only
// right when it really is go-cmp.
func cmpDiffCall(
	pass *analysis.Pass,
	e ast.Expr,
) (a, b ast.Expr, opts []ast.Expr, ellipsis, ok bool) {
	ce, isCall := e.(*ast.CallExpr)
	if !isCall {
		return nil, nil, nil, false, false
	}
	sel, isSel := ce.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "Diff" || len(ce.Args) < 2 {
		return nil, nil, nil, false, false
	}
	pkgIdent, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return nil, nil, nil, false, false
	}
	pkgName, isPkg := pass.TypesInfo.Uses[pkgIdent].(*types.PkgName)
	if !isPkg || pkgName.Imported().Path() != "github.com/google/go-cmp/cmp" {
		return nil, nil, nil, false, false
	}
	return ce.Args[0], ce.Args[1], ce.Args[2:], ce.Ellipsis != token.NoPos, true
}

func isEmptyStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == `""`
}

// cmpDiffInit recognises `if diff := cmp.Diff(a, b[, opts...]); diff != ""`.
func cmpDiffInit(
	pass *analysis.Pass,
	ifs *ast.IfStmt,
) (varName string, a, b ast.Expr, opts []ast.Expr, ellipsis, ok bool) {
	if ifs.Init == nil {
		return "", nil, nil, nil, false, false
	}
	as, isAssign := ifs.Init.(*ast.AssignStmt)
	if !isAssign || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return "", nil, nil, nil, false, false
	}
	id, isIdent := as.Lhs[0].(*ast.Ident)
	if !isIdent {
		return "", nil, nil, nil, false, false
	}
	a, b, opts, ellipsis, isDiff := cmpDiffCall(pass, as.Rhs[0])
	if !isDiff {
		return "", nil, nil, nil, false, false
	}
	be, isBin := ifs.Cond.(*ast.BinaryExpr)
	if !isBin || be.Op != token.NEQ || !isEmptyStringLit(be.Y) {
		return "", nil, nil, nil, false, false
	}
	condID, isIdent := be.X.(*ast.Ident)
	if !isIdent || condID.Name != id.Name {
		return "", nil, nil, nil, false, false
	}
	return id.Name, a, b, opts, ellipsis, true
}

// cmpDiffBare recognises the plain `cmp.Diff(a, b) != ""` condition, with no
// intervening variable.
func cmpDiffBare(
	pass *analysis.Pass,
	cond ast.Expr,
) (a, b ast.Expr, opts []ast.Expr, ellipsis, ok bool) {
	be, isBin := cond.(*ast.BinaryExpr)
	if !isBin || be.Op != token.NEQ || !isEmptyStringLit(be.Y) {
		return nil, nil, nil, false, false
	}
	return cmpDiffCall(pass, be.X)
}

// optsArg renders a cmp.Diff call's trailing options as the single
// []cmp.Option argument EqDiffOpts takes. cmp.Diff(a, b, opts...), a slice
// already spread with the ellipsis, is passed through as that same slice
// rather than re-wrapped; cmp.Diff(a, b, opt1, opt2) has to become a slice
// literal, since EqDiffOpts takes one slice argument rather than a variadic
// tail (msgAndArgs already occupies that, and a cmp.Option satisfies any as
// well as a format string does).
func optsArg(pass *analysis.Pass, opts []ast.Expr, ellipsis bool) string {
	if len(opts) == 1 && ellipsis {
		return render(pass, opts[0])
	}
	parts := make([]string, len(opts))
	for i, o := range opts {
		parts[i] = render(pass, o)
	}
	return "[]cmp.Option{" + strings.Join(parts, ", ") + "}"
}

// wantWords and gotWords classify a diff header's two role words. Neither
// set is exhaustive; an unrecognised word (or one word from each,
// unmatched, such as "first"/"second", which name no role at all) means
// stripDiffHeader reports ok=false, and the caller declines the EqDiff
// mapping for that site rather than guess which argument is which.
var (
	wantWords = map[string]bool{"want": true, "expected": true, "before": true, "old": true}
	gotWords  = map[string]bool{"got": true, "actual": true, "after": true, "new": true}
)

// diffHeaderRE matches a "(-X +Y)" decoration, with an optional trailing
// colon and surrounding whitespace, anywhere in already-unquoted,
// already-decoded message text: not just at the very end, since a label
// often carries more text after it ("(-got +want) for %s").
var diffHeaderRE = regexp.MustCompile(`\s*\(-(\w[\w.]*)\s+\+(\w[\w.]*)\)\s*:?\s*`)

// stripDiffHeader removes a diff-header decoration from text, if one is
// present anywhere in it, and reports whether want and got need to swap
// places to keep EqDiff's own fixed "(-want +got)" header an accurate
// description of which argument is which. ok is false only when a
// decoration is present whose words this tool does not recognise as either
// role: the header might have been true of some other argument order this
// tool cannot verify, so the safer answer is to leave the whole site alone.
func stripDiffHeader(text string) (cleaned string, swap, ok bool) {
	loc := diffHeaderRE.FindStringSubmatchIndex(text)
	if loc == nil {
		return text, false, true
	}
	minus := strings.ToLower(text[loc[2]:loc[3]])
	plus := strings.ToLower(text[loc[4]:loc[5]])
	left := strings.TrimSpace(text[:loc[0]])
	right := strings.TrimSpace(text[loc[1]:])
	switch {
	case left != "" && right != "":
		cleaned = left + " " + right
	case left != "":
		cleaned = left
	default:
		cleaned = right
	}
	switch {
	case wantWords[minus] && gotWords[plus]:
		return cleaned, false, true
	case gotWords[minus] && wantWords[plus]:
		return cleaned, true, true
	default:
		return text, false, false
	}
}

// trailingVerbRE matches one verb specifier, with its separator, anchored to
// the end of already-decoded text: the shape trimMessage's tailVerb targets,
// reimplemented here on decoded text so \n means a real newline rather than
// the two literal characters a raw source slice would carry.
var trailingVerbRE = regexp.MustCompile(`[:,]?\s*%[#+\-0-9.]*[a-zA-Z]$`)

func stripTrailingVerb(text string) (prefix string, ok bool) {
	loc := trailingVerbRE.FindStringIndex(text)
	if loc == nil || loc[1] != len(text) {
		return text, false
	}
	return text[:loc[0]], true
}

// cmpDiffMessage builds the message arguments for an EqDiff/EqDiffOpts call
// from the original t.Errorf/t.Fatalf/t.Error/t.Fatal call, dropping the
// diff variable's own mention and the now-redundant header decoration.
//
// diffVar is the init-form's variable name, or "" for the bare-call form,
// which never names a variable to check for. ok is false when the shape is
// not one this tool is confident about: the diff value named somewhere
// other than as the last argument, a header naming unrecognised roles, or a
// format string that does not actually end in the verb the trailing-arg
// check expects. Each of those is this function's way of saying "fall back
// to the old mapping" rather than risk printing something wrong.
func cmpDiffMessage(
	pass *analysis.Pass,
	ce *ast.CallExpr,
	hasFormat bool,
	diffVar string,
) (margs []string, swap, ok bool) {
	if !hasFormat {
		// t.Error/t.Fatal: every argument is a value, none of them a format
		// string to edit, but a string literal among them can still carry a
		// header decoration ("mismatch (-got +want):"), which needs the same
		// strip-and-swap treatment as the Errorf/Fatalf path below.
		out := make([]string, 0, len(ce.Args))
		dropped := false
		swap = false
		for _, a := range ce.Args {
			if !dropped && diffVar != "" {
				if id, isIdent := a.(*ast.Ident); isIdent && id.Name == diffVar {
					dropped = true
					continue
				}
			}
			if lit, isLit := a.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
				decoded, err := strconv.Unquote(lit.Value)
				if err != nil {
					return nil, false, false
				}
				cleaned, sw, headerOK := stripDiffHeader(decoded)
				if !headerOK {
					return nil, false, false
				}
				swap = swap || sw
				out = append(out, strconv.Quote(cleaned))
				continue
			}
			out = append(out, render(pass, a))
		}
		return out, swap, true
	}

	fmtLit := ce.Args[0].(*ast.BasicLit)
	decoded, err := strconv.Unquote(fmtLit.Value)
	if err != nil {
		return nil, false, false
	}
	rest := ce.Args[1:]

	if diffVar != "" && len(rest) > 0 {
		last := rest[len(rest)-1]
		id, isIdent := last.(*ast.Ident)
		if !isIdent || id.Name != diffVar {
			return nil, false, false
		}
		stripped, verbOK := stripTrailingVerb(decoded)
		if !verbOK {
			return nil, false, false
		}
		decoded = stripped
		rest = rest[:len(rest)-1]
	}

	cleaned, swap, ok := stripDiffHeader(decoded)
	if !ok {
		return nil, false, false
	}

	out := make([]string, 0, 1+len(rest))
	if cleaned != "" {
		out = append(out, strconv.Quote(cleaned))
	}
	for _, a := range rest {
		out = append(out, render(pass, a))
	}
	return out, swap, true
}

// site describing a cmp.Diff-shaped assertion, or handled=false to fall
// back to the tool's ordinary condition mapping.
func cmpDiffSite(
	pass *analysis.Pass,
	ifs *ast.IfStmt,
	ce *ast.CallExpr,
	hasFormat bool,
) (method string, args []string, handled bool) {
	var a, b ast.Expr
	var opts []ast.Expr
	var ellipsis bool
	diffVar := ""
	if name, aa, bb, oo, ee, isInit := cmpDiffInit(pass, ifs); isInit {
		diffVar, a, b, opts, ellipsis = name, aa, bb, oo, ee
	} else if aa, bb, oo, ee, isBare := cmpDiffBare(pass, ifs.Cond); isBare {
		a, b, opts, ellipsis = aa, bb, oo, ee
	} else {
		return "", nil, false
	}
	// EqDiff and EqDiffOpts each infer one type parameter from both want and
	// got, the same as Eq; cmp.Diff itself takes any for each and does not
	// need them to agree. A map access typed any against a concrete string,
	// say, compiles as an argument to cmp.Diff and not as EqDiff's T, so
	// mismatched types fall back to the old Eq("", cmp.Diff(...)) mapping,
	// which compares the diff string to "" and does not need a and b to
	// match at all.
	if !sameType(pass, a, b) {
		return "", nil, false
	}

	margs, swap, ok := cmpDiffMessage(pass, ce, hasFormat, diffVar)
	if !ok {
		return "", nil, false
	}
	if swap {
		a, b = b, a
	}

	method = "EqDiff"
	cargs := []string{render(pass, a), render(pass, b)}
	if len(opts) > 0 {
		method = "EqDiffOpts"
		cargs = append(cargs, optsArg(pass, opts, ellipsis))
	}
	return method, append(cargs, margs...), true
}

// upgradeEqCmpDiff finds Eq("", cmp.Diff(a, b[, opts...]), msgAndArgs...)
// calls already on our own assert.C, however the receiver got there
// (Require(), Check(), a bare constructor), and rewrites them to EqDiff or
// EqDiffOpts. These are what a v0.1.0 run left behind, mapping the *if*
// version of this shape onto Eq before EqDiff exists to catch it, so a
// re-run against code already converted by that release upgrades it rather
// than leaving the old mapping in place forever.
func upgradeEqCmpDiff(pass *analysis.Pass) {
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			ce, isCall := n.(*ast.CallExpr)
			if !isCall || len(ce.Args) < 2 {
				return true
			}
			sel, isSel := ce.Fun.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "Eq" || !isOurAssertMethod(pass, sel) {
				return true
			}
			if !isEmptyStringLit(ce.Args[0]) {
				return true
			}
			a, b, opts, ellipsis, isDiff := cmpDiffCall(pass, ce.Args[1])
			if !isDiff {
				return true
			}
			// Same type constraint EqDiff itself has to satisfy; see the
			// longer comment on the same check in cmpDiffSite.
			if !sameType(pass, a, b) {
				return true
			}

			msgArgs := ce.Args[2:]
			cleaned, trailing, swap, ok := upgradeMessage(msgArgs)
			if !ok {
				return true
			}
			if swap {
				a, b = b, a
			}

			method := "EqDiff"
			out := []string{render(pass, a), render(pass, b)}
			if len(opts) > 0 {
				method = "EqDiffOpts"
				out = append(out, optsArg(pass, opts, ellipsis))
			}
			if cleaned != "" {
				out = append(out, strconv.Quote(cleaned))
			}
			for _, a := range trailing {
				out = append(out, render(pass, a))
			}

			pass.Report(analysis.Diagnostic{
				Pos:     sel.Sel.Pos(),
				Message: "Eq(\"\", cmp.Diff(...)) can become " + method,
				SuggestedFixes: []analysis.SuggestedFix{{
					Message: "rewrite to " + method,
					TextEdits: []analysis.TextEdit{{
						Pos:     sel.Sel.Pos(),
						End:     ce.End(),
						NewText: []byte(method + "(" + strings.Join(out, ", ") + ")"),
					}},
				}},
			})
			return true
		})
	}
}

// upgradeMessage strips a diff-header decoration from an already-converted
// call's leading message argument, if it has one and it is a string
// literal. ok is false when a decoration is present but unrecognised, the
// same signal as everywhere else in this file: leave it alone rather than
// guess.
func upgradeMessage(msgArgs []ast.Expr) (cleaned string, trailing []ast.Expr, swap, ok bool) {
	if len(msgArgs) == 0 {
		return "", nil, false, true
	}
	lit, isLit := msgArgs[0].(*ast.BasicLit)
	if !isLit || lit.Kind != token.STRING {
		return "", msgArgs, false, true
	}
	decoded, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", nil, false, false
	}
	cleaned, swap, ok = stripDiffHeader(decoded)
	if !ok {
		return "", nil, false, false
	}
	return cleaned, msgArgs[1:], swap, true
}

// isOurAssertMethod reports whether sel resolves to a method declared on
// this package's own assert.C, whatever the receiver expression looks like:
// c.Eq, c.Require().Eq and assert.NewAborting(t).Eq are all the same check.
func isOurAssertMethod(pass *analysis.Pass, sel *ast.SelectorExpr) bool {
	selection, ok := pass.TypesInfo.Selections[sel]
	if !ok {
		return false
	}
	fn, ok := selection.Obj().(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	return fn.Pkg().Path() == assertPath
}
