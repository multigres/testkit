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

	"golang.org/x/tools/go/analysis"
)

func render(pass *analysis.Pass, n ast.Node) string {
	var b bytes.Buffer
	if err := format.Node(&b, pass.Fset, n); err != nil {
		return ""
	}
	return b.String()
}

func isError(pass *analysis.Pass, e ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	errIface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return types.Implements(t, errIface) || types.Implements(types.NewPointer(t), errIface)
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

// fallback asserts the whole condition is false.
//
// `if cond { t.Errorf(m) }` fails exactly when cond is true, so False(cond)
// is equivalent for any condition whatever its shape, with the expression
// evaluated once as before. That makes it the right answer for everything
// the specific mappings above decline: compound conditions especially, where
// splitting on || is only sound if the left operand aborts and splitting on
// && is not sound at all, since !(a && b) is a disjunction rather than two
// claims.
//
// Specific assertions are tried first because they print the values they
// compared. Where no want and got exist to print, nothing is lost.
func fallback(pass *analysis.Pass, cond ast.Expr) (string, []string) {
	t := pass.TypesInfo.TypeOf(cond)
	if t == nil {
		return "", nil
	}
	b, ok := t.Underlying().(*types.Basic)
	if !ok || b.Kind() != types.Bool {
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
