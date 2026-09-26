// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/tools/go/analysis"
)

// objEqReplaced records, per pass, every assert.ObjectsAreEqual call
// substituteObjEq has actually replaced on behalf of a site collection has
// decided to keep. Keyed by pass rather than reset per run(): golang.org/x/tools'
// own checker runs one goroutine per package's analysis, concurrently,
// inside a single process under analysistest (confirmed with -race, which
// is why this is a pass-keyed map behind a mutex rather than a
// package-level variable reset at the top of run()), so a single shared
// map would race, and even data-race-free, would mix one package's
// substitutions into another's. objequal.go's own pass over the file,
// which always runs after collection has already rendered every other site
// in the same pass, consults it to tell a call collection's own rendering
// actually reached from one that only looks like it did, being
// positionally inside a site that converts without actually being on the
// path substituteObjEq walks for it (nested inside some other call's own
// arguments, or inside a for/range/switch/var this function was never
// extended to look inside).
//
// A render() call is not, by itself, proof that its site survives:
// collect() renders a candidate's condition (through mapCond, building
// cargs) before the decline checks that follow it (messageArgsAreSafe,
// guardsItsMessage, the inline/hoist safety checks), so a candidate
// rendered and then declined must not count. objEqPending is the
// candidate-scoped buffer that closes that gap: beginObjEqCandidate resets
// it fresh before collect() starts rendering a new candidate (a fresh
// if/testify call, or a fresh attempt at the same one, since cmpDiffSite
// and mapCond are two separate attempts at the same ifs and a decline by
// the first must not leak into the second), render() writes into whichever
// buffer is currently active, and commitObjEqCandidate folds it into
// objEqReplaced only at the point collect() actually appends the site. A
// candidate that declines without committing leaves its pending entries to
// be silently discarded by the next beginObjEqCandidate call, or, at the
// end of the file, never read at all: no explicit discard needed.
//
// Outside collect() entirely (upgradeEqCmpDiff's own rewrite, and
// objectsAreEqualFixes' standalone, non-skip fixes), no candidate is ever
// begun, so recordObjEqReplaced's nil-pending branch records immediately:
// both of those apply their edit unconditionally, with no decline check
// between rendering and reporting, so there is nothing to buffer against.
var (
	objEqMu       sync.Mutex
	objEqPending  = map[*analysis.Pass]map[*ast.CallExpr]bool{}
	objEqReplaced = map[*analysis.Pass]map[*ast.CallExpr]bool{}
)

// beginObjEqCandidate starts (or restarts) the pending buffer for the
// candidate collect() is about to render. Call it immediately before the
// first render() call for a new candidate, and again before any later,
// separate attempt at the same node whose earlier attempt was declined.
func beginObjEqCandidate(pass *analysis.Pass) {
	objEqMu.Lock()
	defer objEqMu.Unlock()
	objEqPending[pass] = map[*ast.CallExpr]bool{}
}

// commitObjEqCandidate folds the current pending buffer into the permanent
// record. Call it only at the point a candidate is confirmed kept (right
// after collect() appends its site), never on a decline path.
func commitObjEqCandidate(pass *analysis.Pass) {
	objEqMu.Lock()
	defer objEqMu.Unlock()
	pending := objEqPending[pass]
	delete(objEqPending, pass)
	if len(pending) == 0 {
		return
	}
	m := objEqReplaced[pass]
	if m == nil {
		m = map[*ast.CallExpr]bool{}
		objEqReplaced[pass] = m
	}
	for ce := range pending {
		m[ce] = true
	}
}

func recordObjEqReplaced(pass *analysis.Pass, ce *ast.CallExpr) {
	objEqMu.Lock()
	defer objEqMu.Unlock()
	if p, ok := objEqPending[pass]; ok {
		p[ce] = true
		return
	}
	m := objEqReplaced[pass]
	if m == nil {
		m = map[*ast.CallExpr]bool{}
		objEqReplaced[pass] = m
	}
	m[ce] = true
}

func wasObjEqReplaced(pass *analysis.Pass, ce *ast.CallExpr) bool {
	objEqMu.Lock()
	defer objEqMu.Unlock()
	return objEqReplaced[pass][ce]
}

// clearObjEqPass drops this pass's own entries once run() is done with
// them. unitchecker runs one package per process, so the entry would only
// ever be read once regardless; but analysistest, multichecker, and gopls
// all keep the process (and this map) alive across many passes, and each
// entry retains its whole TypesInfo and AST for as long as it lives. run()
// defers this at the top, so it fires however run() returns, including its
// early return for the assert package's own tests.
func clearObjEqPass(pass *analysis.Pass) {
	objEqMu.Lock()
	defer objEqMu.Unlock()
	delete(objEqReplaced, pass)
	delete(objEqPending, pass)
}

// objEqPassCount reports how many passes' worth of state objEqReplaced and
// objEqPending are still holding, for a test to confirm clearObjEqPass
// actually ran rather than reading the maps directly, which the race
// detector would flag as unsynchronized access against a concurrent pass.
func objEqPassCount() (replaced, pending int) {
	objEqMu.Lock()
	defer objEqMu.Unlock()
	return len(objEqReplaced), len(objEqPending)
}

func render(pass *analysis.Pass, n ast.Node) string {
	var b bytes.Buffer
	if err := format.Node(&b, pass.Fset, substituteObjEq(pass, n)); err != nil {
		return ""
	}
	return b.String()
}

// substituteObjEq returns n, or a rebuilt copy of n with every
// assert.ObjectsAreEqual(a, b) sub-node replaced by reflect.DeepEqual(a, b),
// never mutating the original: n may be shared with other computations
// (type information, other edits' position arithmetic), so a replacement
// always builds new nodes on the path to a match and returns the original,
// unchanged, everywhere else. Every ObjectsAreEqual call it does replace is
// also recorded via recordObjEqReplaced, keyed by its original node:
// objequal.go's own pass over the file needs to tell an embedded call this
// function actually reaches from one it does not (nested inside some other
// call's arguments, a for/range/switch/var this function was never
// extended to look inside),
// since both look the same from outside, positionally contained in a site
// that converts, but only one of them is actually rewritten by it.
//
// This only has to cover the node shapes that actually reach render() built
// around a condition or a call's arguments: parens, a negation, a compound
// boolean expression, the call itself, and a closure passed as an argument
// (testify's Eventually), whose body render also has to look inside. Any
// shape not listed here is returned unchanged, which only means a missed
// substitution, never an incorrect one.
func substituteObjEq(pass *analysis.Pass, n ast.Node) ast.Node {
	switch v := n.(type) {
	case nil:
		return nil
	case *ast.ParenExpr:
		x, _ := substituteObjEq(pass, v.X).(ast.Expr)
		if x == v.X {
			return v
		}
		cp := *v
		cp.X = x
		return &cp
	case *ast.UnaryExpr:
		x, _ := substituteObjEq(pass, v.X).(ast.Expr)
		if x == v.X {
			return v
		}
		cp := *v
		cp.X = x
		return &cp
	case *ast.BinaryExpr:
		x, _ := substituteObjEq(pass, v.X).(ast.Expr)
		y, _ := substituteObjEq(pass, v.Y).(ast.Expr)
		if x == v.X && y == v.Y {
			return v
		}
		cp := *v
		cp.X, cp.Y = x, y
		return &cp
	case *ast.CallExpr:
		if a, b, isObjEq := objectsAreEqualCall(pass, v); isObjEq {
			recordObjEqReplaced(pass, v)
			return &ast.CallExpr{
				Fun: &ast.SelectorExpr{
					X:   ast.NewIdent("reflect"),
					Sel: ast.NewIdent("DeepEqual"),
				},
				Args: []ast.Expr{a, b},
			}
		}
		return v
	case *ast.FuncLit:
		body, _ := substituteObjEq(pass, v.Body).(*ast.BlockStmt)
		if body == v.Body {
			return v
		}
		cp := *v
		cp.Body = body
		return &cp
	case *ast.BlockStmt:
		changed := false
		list := make([]ast.Stmt, len(v.List))
		for i, st := range v.List {
			ns, _ := substituteObjEq(pass, st).(ast.Stmt)
			list[i] = ns
			if ns != st {
				changed = true
			}
		}
		if !changed {
			return v
		}
		cp := *v
		cp.List = list
		return &cp
	case *ast.ReturnStmt:
		changed := false
		results := make([]ast.Expr, len(v.Results))
		for i, r := range v.Results {
			nr, _ := substituteObjEq(pass, r).(ast.Expr)
			results[i] = nr
			if nr != r {
				changed = true
			}
		}
		if !changed {
			return v
		}
		cp := *v
		cp.Results = results
		return &cp
	case *ast.IfStmt:
		cond, _ := substituteObjEq(pass, v.Cond).(ast.Expr)
		body, _ := substituteObjEq(pass, v.Body).(*ast.BlockStmt)
		if cond == v.Cond && body == v.Body {
			return v
		}
		cp := *v
		cp.Cond, cp.Body = cond, body
		return &cp
	case *ast.ExprStmt:
		x, _ := substituteObjEq(pass, v.X).(ast.Expr)
		if x == v.X {
			return v
		}
		cp := *v
		cp.X = x
		return &cp
	case *ast.AssignStmt:
		changed := false
		rhs := make([]ast.Expr, len(v.Rhs))
		for i, r := range v.Rhs {
			nr, _ := substituteObjEq(pass, r).(ast.Expr)
			rhs[i] = nr
			if nr != r {
				changed = true
			}
		}
		if !changed {
			return v
		}
		cp := *v
		cp.Rhs = rhs
		return &cp
	default:
		return n
	}
}

// isError reports whether e's static type is an interface implementing
// error, as opposed to a concrete type (typically a pointer) that merely
// satisfies it.
//
// The distinction matters because NoError, Error and ErrorWhen all take the
// error interface, and a nil check on it is not always the same claim as a
// nil check on a concrete operand. `err error` compared to nil asks whether
// the interface itself is nil. A concrete `*fieldErr` compared to nil asks
// whether that pointer is nil, which is a different, stronger question:
// storing a nil *fieldErr into an error interface produces a non-nil
// interface holding a nil pointer, so `err != nil` on the interface is true
// for exactly the value that `p != nil` on the concrete pointer says is
// false. Mapping the concrete comparison onto an interface-typed assertion
// would silently ask the interface question instead, and get the typed-nil
// row backwards. Requiring the static type to already be the interface
// rules that out: this only fires where the source was already comparing
// the interface, not a concrete value that happens to implement it.
func isError(pass *analysis.Pass, e ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	if _, ok := t.Underlying().(*types.Interface); !ok {
		return false
	}
	errIface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return types.Implements(t, errIface)
}

// sameType reports whether both operands infer one type parameter.
//
// Eq and EqDiff are each generic over a single T, so a comparison between an
// interface and a concrete type implementing it does not compile even though
// the Go expression `a != b` does. That distinction is invisible to a
// syntactic rewriter and is why this tool type-checks.
func sameType(pass *analysis.Pass, a, b ast.Expr) bool {
	ta, tb := pass.TypesInfo.TypeOf(a), pass.TypesInfo.TypeOf(b)
	if ta == nil || tb == nil {
		return false
	}
	return types.Identical(ta, tb)
}

// comparableOperands reports whether assert.Eq would compile for these two,
// which is the question a syntactic rewriter cannot answer and the main
// reason this tool uses the type checker at all.
func comparableOperands(pass *analysis.Pass, a, b ast.Expr) bool {
	if !sameType(pass, a, b) {
		return false
	}
	return strictlyComparable(pass.TypesInfo.TypeOf(a))
}

// strictlyComparable reports whether a type can be a type argument for a
// comparable parameter, which is stricter than whether == compiles for it.
//
// == on two interface values compiles and panics at run time when the
// dynamic types turn out not to be comparable; the constraint refuses those
// up front. types.Comparable answers the operator's question and so says yes
// to any, which is how c.Eq and c.Contains over a []any got emitted and
// failed to compile. types.Satisfies is not the fix: the predeclared
// comparable's strictness lives in the name rather than in the interface it
// unwraps to, so Satisfies(any, that interface) is true as well.
func strictlyComparable(t types.Type) bool {
	if t == nil {
		return false
	}
	switch u := t.Underlying().(type) {
	case *types.Interface:
		return false
	case *types.Struct:
		for i := range u.NumFields() {
			if !strictlyComparable(u.Field(i).Type()) {
				return false
			}
		}
		return true
	case *types.Array:
		return strictlyComparable(u.Elem())
	default:
		return types.Comparable(t)
	}
}

var wantish = regexp.MustCompile(`(?i)want|expect`)

// wantFirst orders a comparison's operands as (want, got). A literal or a
// want-ish name is the expectation; otherwise the right operand is, which is
// how `if got != want` is written.
func wantFirst(pass *analysis.Pass, a, b ast.Expr) (string, string) {
	sa, sb := render(pass, a), render(pass, b)
	aw := isLiteral(a) || wantish.MatchString(sa)
	bw := isLiteral(b) || wantish.MatchString(sb)
	if aw && !bw {
		return sa, sb
	}
	return sb, sa
}

func isLiteral(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.UnaryExpr:
		return isLiteral(v.X)
	case *ast.Ident:
		return v.Name == "true" || v.Name == "false" || v.Name == "nil"
	case *ast.CompositeLit:
		return true
	}
	return false
}

func callOf(e ast.Expr, pkg, fn string) []ast.Expr {
	ce, ok := e.(*ast.CallExpr)
	if !ok {
		return nil
	}
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != fn {
		return nil
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != pkg {
		return nil
	}
	return ce.Args
}

func builtinLen(e ast.Expr) ast.Expr {
	ce, ok := e.(*ast.CallExpr)
	if !ok || len(ce.Args) != 1 {
		return nil
	}
	id, ok := ce.Fun.(*ast.Ident)
	if !ok || id.Name != "len" {
		return nil
	}
	return ce.Args[0]
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// unparen strips redundant parens, which a hand-written
// `(err != nil) != wantErr` carries for readability but the AST does not
// need: == and != are already left-associative at one precedence level, so
// the parenthesised and unparenthesised forms parse identically.
func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// errWhenArgs recognises `(err != nil) != wantErr` and its equivalents:
// `(err == nil) == wantErr`, and both again with the two sides swapped. All
// four say the same thing, because != and == are commutative over bool
// operands; a human picks whichever reads best at the call site, and this
// tool should not care which they picked.
//
// op is the outer comparison's operator, and is deliberately not tried
// against its own negation: `(err != nil) == wantErr` is a different claim
// from `(err != nil) != wantErr`, so only NEQ paired with an inner NEQ, and
// EQL paired with an inner EQL, are accepted.
func errWhenArgs(
	pass *analysis.Pass,
	op token.Token,
	x, y ast.Expr,
) (wantErr, errExpr ast.Expr, ok bool) {
	if w, e, ok := errWhenSide(pass, op, x, y); ok {
		return w, e, true
	}
	return errWhenSide(pass, op, y, x)
}

// errWhenSide checks whether a is `<errExpr> op nil` and b is some other
// boolean expression that is not itself a nil comparison, which is what rules
// out the unrelated `(a != nil) != (b != nil)` shape.
func errWhenSide(
	pass *analysis.Pass,
	op token.Token,
	a, b ast.Expr,
) (wantErr, errExpr ast.Expr, ok bool) {
	inner, isBin := unparen(a).(*ast.BinaryExpr)
	if !isBin || inner.Op != op || !isNil(inner.Y) || !isError(pass, inner.X) {
		return nil, nil, false
	}
	if bi, isBin := unparen(b).(*ast.BinaryExpr); isBin &&
		(bi.Op == token.NEQ || bi.Op == token.EQL) &&
		isNil(bi.Y) {
		return nil, nil, false
	}
	if !isBoolTyped(pass, b) {
		return nil, nil, false
	}
	return b, inner.X, true
}

// isBoolTyped reports whether e's type is exactly bool, or an untyped bool
// constant.
//
// a (the nil comparison) always has type bool, never a named bool-underlying
// type, since `x != nil` is not a constant expression. For `a op b` to have
// already type-checked, b's type must be identical to bool or an untyped
// constant assignable to it: a named type with underlying bool does not
// compare against plain bool (Go requires identical types, and named types
// are never identical to their underlying type), so this guard cannot be
// defeated that way. It is defeated when b is itself an interface, such as
// wantErr any: bool compares against any without either side needing to be
// bool, but ErrorWhen(wantErr bool, ...) cannot take that value unconverted.
func isBoolTyped(pass *analysis.Pass, e ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && (b.Kind() == types.Bool || b.Kind() == types.UntypedBool)
}

func isZero(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Value == "0"
}

// mapCond turns a *failure* condition into the assertion that asserts its
// negation, or returns "" when it cannot do so with confidence. Leaving a
// site alone costs a line of verbosity; getting it wrong costs a test.
func mapCond(pass *analysis.Pass, cond ast.Expr) (string, []string) {
	switch c := cond.(type) {
	case *ast.ParenExpr:
		return mapCond(pass, c.X)

	case *ast.UnaryExpr:
		if c.Op != token.NOT {
			return "", nil
		}
		inner := c.X
		if args := callOf(inner, "strings", "Contains"); len(args) == 2 {
			return "StrContains", []string{render(pass, args[0]), render(pass, args[1])}
		}
		if args := callOf(inner, "strings", "HasPrefix"); len(args) == 2 {
			return "", nil // no HasPrefix assertion; leave it
		}
		if args := callOf(inner, "errors", "Is"); len(args) == 2 {
			return "ErrorIs", []string{render(pass, args[0]), render(pass, args[1])}
		}
		if args := callOf(inner, "slices", "Equal"); len(args) == 2 {
			return eqMethod(pass, args[0]), []string{render(pass, args[1]), render(pass, args[0])}
		}
		if args := callOf(inner, "slices", "Contains"); len(args) == 2 {
			return "Contains", []string{render(pass, args[0]), render(pass, args[1])}
		}
		if args := callOf(inner, "reflect", "DeepEqual"); len(args) == 2 {
			// DeepEqual reads unexported fields, so the faithful
			// translation is EqDeep unless the type is one go-cmp would
			// walk anyway.
			if !sameType(pass, args[0], args[1]) {
				return "", nil
			}
			return eqMethod(pass, args[0]), []string{render(pass, args[1]), render(pass, args[0])}
		}
		if isPlainBool(pass, inner) {
			return "True", []string{render(pass, inner)}
		}
		return fallback(pass, c)

	case *ast.BinaryExpr:
		switch c.Op {
		case token.NEQ:
			if wantErr, errExpr, ok := errWhenArgs(pass, token.NEQ, c.X, c.Y); ok {
				return "ErrorWhen", []string{render(pass, wantErr), render(pass, errExpr)}
			}
			if isNil(c.Y) {
				if isError(pass, c.X) {
					return "NoError", []string{render(pass, c.X)}
				}
				return "Nil", []string{render(pass, c.X)}
			}
			if x := builtinLen(c.X); x != nil {
				if isZero(c.Y) {
					return "Empty", []string{render(pass, x)}
				}
				return "Len", []string{render(pass, x), render(pass, c.Y)}
			}
			if comparableOperands(pass, c.X, c.Y) {
				w, g := wantFirst(pass, c.X, c.Y)
				return "Eq", []string{w, g}
			}
			// Everything else != compiles for but comparable refuses is an
			// interface, or holds one. On those != is identity, not value
			// equality: `transport.Transport != http.DefaultTransport` asks
			// whether it is that same transport. Rewriting it to a deep
			// comparison replaces the question, and the answer changes.
			// Assert the expression whole instead.
			return fallback(pass, c)

		case token.EQL:
			if wantErr, errExpr, ok := errWhenArgs(pass, token.EQL, c.X, c.Y); ok {
				return "ErrorWhen", []string{render(pass, wantErr), render(pass, errExpr)}
			}
			if isNil(c.Y) {
				if isError(pass, c.X) {
					return "Error", []string{render(pass, c.X)}
				}
				return "NotNil", []string{render(pass, c.X)}
			}
			if x := builtinLen(c.X); x != nil && isZero(c.Y) {
				return "NotEmpty", []string{render(pass, x)}
			}
			if comparableOperands(pass, c.X, c.Y) {
				w, g := wantFirst(pass, c.X, c.Y)
				return "NotEq", []string{w, g}
			}
			return fallback(pass, c) // no NotEqDiff, so assert the whole thing

		case token.GTR: // if got > want { fail } asserts got <= want
			if ordered(pass, c.X, c.Y) {
				return "LessOrEqual", []string{render(pass, c.Y), render(pass, c.X)}
			}
		case token.LSS:
			if ordered(pass, c.X, c.Y) {
				return "GreaterOrEqual", []string{render(pass, c.Y), render(pass, c.X)}
			}
		case token.GEQ:
			if ordered(pass, c.X, c.Y) {
				return "Less", []string{render(pass, c.Y), render(pass, c.X)}
			}
		case token.LEQ:
			if ordered(pass, c.X, c.Y) {
				return "Greater", []string{render(pass, c.Y), render(pass, c.X)}
			}
		}
		return fallback(pass, c)
	}

	// Un-negated calls: `if strings.Contains(s, x) { fail }` asserts absence.
	// Reached only for a bare call, since the negated forms are handled above.
	if args := callOf(cond, "strings", "Contains"); len(args) == 2 {
		return "NotStrContains", []string{render(pass, args[0]), render(pass, args[1])}
	}
	if args := callOf(cond, "slices", "Contains"); len(args) == 2 {
		return "NotContains", []string{render(pass, args[0]), render(pass, args[1])}
	}

	return fallback(pass, cond)
}

// fallback asserts a whole boolean condition rather than picking it apart,
// for shapes with nothing more specific to say.
//
// `if cond { t.Errorf(m) }` fails exactly when cond is true, so False(cond)
// is equivalent for any condition whatever its shape, evaluated once as
// before: the condition itself carries no new risk, compound or not, since
// passing it through unchanged preserves its own `&&`/`||` short-circuiting
// exactly as it ran before conversion. The message beside it is the actual
// hazard this cannot see from here: it is a new argument to a call, always
// evaluated, where the original only evaluated it inside the failing
// branch, and this function has no access to it to check. main.go's
// messageArgsAreSafe is the guard for that, applied wherever this returns
// "False" (and to the sibling "True" case in the caller above).
//
// cond is reached here in two shapes: a plain bool expression (isPlainBool's
// negative case), whose recorded type is the defined bool, and a direct
// comparison (the NEQ/EQL "everything else" branches above), whose type a
// comparison operator always records as untyped bool regardless of its
// operands' types, per the language spec. Checking only types.Bool made this
// fallback dead for every comparison it was written for; both kinds are
// checked here because both are exactly the boolean condition this asserts.
func fallback(pass *analysis.Pass, cond ast.Expr) (string, []string) {
	t := pass.TypesInfo.TypeOf(cond)
	if t == nil {
		return "", nil
	}
	b, ok := t.Underlying().(*types.Basic)
	if !ok || (b.Kind() != types.Bool && b.Kind() != types.UntypedBool) {
		return "", nil
	}
	return "False", []string{render(pass, cond)}
}

func isPlainBool(pass *analysis.Pass, e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.SelectorExpr, *ast.CallExpr:
	default:
		return false
	}
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.Bool
}

func ordered(pass *analysis.Pass, a, b ast.Expr) bool {
	ta, tb := pass.TypesInfo.TypeOf(a), pass.TypesInfo.TypeOf(b)
	if ta == nil || tb == nil {
		return false
	}
	ok := func(t types.Type) bool {
		bt, is := t.Underlying().(*types.Basic)
		return is && bt.Info()&(types.IsInteger|types.IsFloat|types.IsString) != 0
	}
	return ok(ta) && ok(tb) && types.Identical(ta.Underlying(), tb.Underlying())
}

// trimMessage drops the part of a test's message that the assertion now
// prints for itself.
//
// `t.Errorf("Server = %q, want %q", got, want)` carries three things: a
// label, the got value and the want value. The assertion prints the last two,
// so passing the original through verbatim renders them twice. Reducing it to
// the label is the difference between a call that reads like an assertion and
// one that reads like a macro expansion.
//
// Only the exactly-recognised shape is trimmed, and only when the trailing
// arguments are literally the expressions being compared. Anything else is
// passed through whole, because a message that says something the values do
// not is the half the assertion cannot supply.
func trimMessage(msgArgs, cmpArgs []string) []string {
	if len(msgArgs) == 0 {
		return msgArgs
	}
	fmtStr := msgArgs[0]
	rest := msgArgs[1:]
	if len(fmtStr) < 2 || fmtStr[0] != '"' || fmtStr[len(fmtStr)-1] != '"' {
		return msgArgs
	}

	// Trailing-tail trim first: `"Write(%q) error: %v", path, err` keeps its
	// label and its own argument and drops only the part the assertion
	// prints. Without this the whole message is retained, which for an
	// inlined init clause means declining the site entirely rather than
	// evaluating the call twice.
	if len(rest) > 0 && slices.Contains(cmpArgs, rest[len(rest)-1]) {
		if tm := tailVerb.FindStringSubmatch(fmtStr); tm != nil {
			trimmed := append([]string{tm[1] + `"`}, rest[:len(rest)-1]...)
			// Recurse: the shortened message may now be a bare label.
			return trimMessage(trimmed, cmpArgs)
		}
	}

	m := labelFmt.FindStringSubmatch(fmtStr)
	if m == nil {
		return msgArgs
	}
	label, verbs := m[1], strings.Count(m[2], "%")
	if verbs != len(rest) || label == "" {
		return msgArgs
	}
	// Every remaining argument has to be one of the compared expressions,
	// otherwise the message is carrying information of its own.
	for _, r := range rest {
		if !slices.Contains(cmpArgs, r) {
			return msgArgs
		}
	}
	return []string{`"` + label + `"`}
}

// A trailing verb preceded by a separator: the `: %v` of
// `"doing x: %v"`, which is the value the assertion is about to print.
var tailVerb = regexp.MustCompile(`^(".*?)[:,]?\s*%[#+\-0-9.]*[a-zA-Z]"$`)

// A label, then only formatting: `Server = %q`, `len(x) = %d, want %d`.
var labelFmt = regexp.MustCompile(`^"([^"%]*?)\s*(?:=|:)?\s*((?:%[#+\-0-9.]*[a-zA-Z][^"%]*)+)"$`)

// dropRedundant removes a t.Error/t.Fatal argument that already appears
// among the assertion's own arguments, the reason trimMessage exists for the
// -f variants: an argument with no format string to hide behind is printed
// verbatim, and one that is also, say, the err just handed to NoError would
// otherwise print twice.
func dropRedundant(msgArgs, cmpArgs []string) []string {
	out := make([]string, 0, len(msgArgs))
	for _, m := range msgArgs {
		if slices.Contains(cmpArgs, m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// cmpSafe reports whether go-cmp can compare this type without panicking,
// which is what EqDiff needs and what reflect.DeepEqual does not require.
//
// The two are not interchangeable and that is how this check was found: the
// rewrite turned a DeepEqual over concur.config into EqDiff, and go-cmp
// panics on an unexported field rather than reading it. DeepEqual reads it.
// So a conversion that looks like a straight substitution silently replaces
// an assertion with a panic.
//
// An Equal method makes a type safe regardless, because go-cmp prefers it
// over reflecting inside. Everything else has to be exported all the way
// down, checked recursively with a seen set for the self-referential cases.
func cmpSafe(t types.Type) bool {
	return cmpSafeIn(t, map[types.Type]bool{})
}

func cmpSafeIn(t types.Type, seen map[types.Type]bool) bool {
	if t == nil {
		return false
	}
	if seen[t] {
		return true
	}
	seen[t] = true

	if hasEqualMethod(t) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return true
	case *types.Slice:
		return cmpSafeIn(u.Elem(), seen)
	case *types.Array:
		return cmpSafeIn(u.Elem(), seen)
	case *types.Pointer:
		return cmpSafeIn(u.Elem(), seen)
	case *types.Map:
		return cmpSafeIn(u.Key(), seen) && cmpSafeIn(u.Elem(), seen)
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			if !f.Exported() {
				return false
			}
			if !cmpSafeIn(f.Type(), seen) {
				return false
			}
		}
		return true
	case *types.Interface:
		// The dynamic type is what go-cmp will actually see, and it is not
		// knowable here.
		return false
	}
	return false
}

func hasEqualMethod(t types.Type) bool {
	for _, cand := range []types.Type{t, types.NewPointer(t)} {
		ms := types.NewMethodSet(cand)
		for i := 0; i < ms.Len(); i++ {
			if ms.At(i).Obj().Name() == "Equal" {
				return true
			}
		}
	}
	return false
}

// eqMethod picks the diffing assertion that will not panic on this type.
//
// EqDiff is preferred because go-cmp's refusal to read unexported fields is
// a useful default: it says a test is asserting about another package's
// internals. Where the type has them anyway, EqDeep is the faithful
// translation of what the test was already doing with reflect.DeepEqual.
func eqMethod(pass *analysis.Pass, e ast.Expr) string {
	if cmpSafe(pass.TypesInfo.TypeOf(e)) {
		return "EqDiff"
	}
	return "EqDeep"
}
