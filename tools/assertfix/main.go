// SPDX-License-Identifier: Apache-2.0

// Command assertfix rewrites stdlib table-test assertions into
// testkit/assert calls, as a go/analysis fixtool.
//
//	go fix -fixtool=$(go env GOPATH)/bin/assertfix ./...
//
// It exists because the alternative, a regex pass over the source, cannot
// answer the one question that decides half the rewrites: whether a type is
// comparable. assert.Eq is generic over comparable, so rewriting
//
//	if got != want { t.Errorf(...) }
//
// to c.Eq(want, got) does not compile when the operands are slices, maps, or
// structs containing either, and a syntactic tool cannot tell. This one asks
// the type checker and emits EqDiff instead. The same information picks
// NoError over Nil for an error, and StrContains over Contains for a string.
//
// Using the analysis framework also keeps the edits surgical. A SuggestedFix
// is a set of byte ranges, not a reprinted file, so comments outside the
// replaced statement are untouched. That matters here: these tests carry long
// explanatory comments, and a tool that reformatted them would lose the thing
// most worth keeping.
package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/analysis/unitchecker"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "assertfix",
	Doc:      "rewrite stdlib test assertions into testkit/assert calls",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func main() { unitchecker.Main(Analyzer) }

// site is one convertible assertion: either an `if cond { t.Errorf(...) }`
// or a single testify call. Both collapse to "replace this whole node with
// one method call on the scope's receiver".
type site struct {
	node    ast.Node
	abort   bool
	method  string
	args    []string
	testify bool
	// hoist is a statement to insert immediately before node, rendered but
	// not yet indented: the if-with-init's own init clause, moved out
	// because the message would otherwise need to evaluate it twice, or
	// because it declares more than one name.
	hoist string
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	defer clearObjEqPass(pass)

	// Only _test.go files. A helper in production code can take a
	// *testing.T and still be production code: testkit's own golden package
	// exports AssertYAML(t, ...) and documents itself as a leaf that imports
	// nothing but the standard library, go-cmp and yaml. Rewriting it added
	// an import and broke that promise, which is a change to the package's
	// public dependency graph rather than to a test.
	// The assert package's own tests are the one place this rewrite is
	// circular: adding the import is a cycle, and even in an external test
	// package an assertion library that tests itself with itself hides its
	// own failures.
	if strings.HasPrefix(pass.Pkg.Path(), assertPath) {
		return nil, nil
	}

	// Upgrades an already-converted Eq("", cmp.Diff(...)) call, wherever one
	// sits, independent of everything below: it is not gated on a scope, a
	// file's testify status, or anything else this pass collects.
	upgradeEqCmpDiff(pass)

	inTest := func(pos token.Pos) bool {
		f := fileOf(pass, pos)
		if f == nil || !strings.HasSuffix(pass.Fset.Position(pos).Filename, "_test.go") {
			return false
		}
		// testify's suite package is a different testing model, not a
		// vocabulary, so a per-assertion rewrite has nothing to say about it.
		if usesTestifySuite(f) {
			return false
		}
		// A file using some other package named assert cannot take ours
		// under the same name, and a file mixing two assertion libraries is
		// worse than one left alone. testify is the exception: it is removed
		// rather than coexisted with, so it does not count here.
		return !bindsAssertElsewhere(f)
	}

	// Collect per enclosing function body, because the receiver declaration
	// is inserted once per scope and its mode depends on every site in that
	// scope: a scope mixing Errorf and Fatalf gets a collecting receiver with
	// Require() on the aborting sites, so no test changes behaviour.
	type scope struct {
		body  *ast.BlockStmt
		tName string
		recv  string
		sites []site
	}
	var scopes []*scope

	collect := func(ft *ast.FuncType, body *ast.BlockStmt, tName string) {
		sc := &scope{body: body, tName: tName}

		// The block and index of every statement in this function, for hoist
		// safety: nothing later in the same block already claims the name.
		parent := indexParents(body)

		// A goto anywhere in the function rules out hoisting for the whole
		// scope: hoisting an init to a new standalone statement can insert a
		// variable declaration between a goto and its label, which fails to
		// compile ("goto jumps over declaration of ..."), and finding out
		// whether THIS particular goto actually jumps over THIS particular
		// hoist site is more analysis than the rarity of goto in test code
		// justifies. Inlining is unaffected: it introduces no declaration.
		hasGoto := false
		ast.Inspect(body, func(n ast.Node) bool {
			if hasGoto {
				return false
			}
			if bs, ok := n.(*ast.BranchStmt); ok && bs.Tok == token.GOTO {
				hasGoto = true
				return false
			}
			return true
		})

		// An if that is itself another if's else branch cannot be replaced by
		// an expression: `} else if c { t.Errorf(...) }` would become
		// `} else c.Eq(...)`, which does not parse. Collect those first and
		// skip them rather than trying to rewrite the whole chain.
		isElse := map[ast.Node]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			if ifs, ok := n.(*ast.IfStmt); ok && ifs.Else != nil {
				isElse[ifs.Else] = true
			}
			return true
		})

		ast.Inspect(body, func(n ast.Node) bool {
			// Do not descend into a nested func(t *testing.T) body; it is
			// its own scope and is visited separately.
			if fl, ok := n.(*ast.FuncLit); ok && fl.Body != body {
				if nestedT(fl.Type) != "" {
					return false
				}
			}
			if es, ok := n.(*ast.ExprStmt); ok {
				ce, ok := es.X.(*ast.CallExpr)
				if !ok {
					return true
				}
				beginObjEqCandidate(pass)
				m, a, ab, ok := testifyCall(pass, ce, tName)
				if !ok {
					return true
				}
				sc.sites = append(sc.sites, site{
					node: es, abort: ab, method: m, args: a, testify: true,
				})
				commitObjEqCandidate(pass)
				// Stop here. This whole statement is being replaced, and
				// arguments can hold statements of their own:
				// require.NotPanics(t, func() { ... if x { t.Errorf(...) } })
				// collected the inner site too, and the two edits overlapped.
				return false
			}
			ifs, ok := n.(*ast.IfStmt)
			if !ok || ifs.Else != nil || isElse[n] || len(ifs.Body.List) != 1 {
				return true
			}
			// A compound condition combined with an init is left alone
			// entirely: compound conditions are out of scope on their own
			// (splitting `||` is only sound if the left operand aborts, and
			// `&&` does not split at all), and hoisting the init just to
			// still decline on the condition buys nothing.
			var initAssign *ast.AssignStmt
			if ifs.Init != nil {
				as, isAssign := ifs.Init.(*ast.AssignStmt)
				if !isAssign || as.Tok != token.DEFINE {
					return true
				}
				if be, isBin := unparen(ifs.Cond).(*ast.BinaryExpr); isBin &&
					(be.Op == token.LAND || be.Op == token.LOR) {
					return true
				}
				initAssign = as
			}
			// Eligible for inlining only when the init declares exactly one
			// name from exactly one expression: `if err := f(); ...`. A
			// multi-value init (`_, ok := m[k]`, `got, want := a, b`) can
			// only be hoisted, never inlined, because there is more than
			// one name to substitute and the existing message-rewriting
			// logic only ever tracks one.
			inlineName := ""
			if initAssign != nil && len(initAssign.Lhs) == 1 && len(initAssign.Rhs) == 1 {
				if id, isIdent := initAssign.Lhs[0].(*ast.Ident); isIdent && id.Name != "_" {
					inlineName = id.Name
				}
			}
			es, ok := ifs.Body.List[0].(*ast.ExprStmt)
			if !ok {
				return true
			}
			ce, ok := es.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != tName {
				return true
			}
			switch sel.Sel.Name {
			case "Errorf", "Fatalf", "Error", "Fatal":
			default:
				return true
			}
			hasFormat := sel.Sel.Name == "Errorf" || sel.Sel.Name == "Fatalf"
			beginObjEqCandidate(pass)
			if method, args, handled := cmpDiffSite(pass, ifs, ce, hasFormat); handled {
				sc.sites = append(sc.sites, site{
					node:   ifs,
					abort:  strings.HasPrefix(sel.Sel.Name, "Fatal"),
					method: method,
					args:   args,
				})
				commitObjEqCandidate(pass)
				return false
			}
			// cmpDiffSite's own decline may have rendered something (a
			// message argument, before a later one in the same call failed
			// its own check): fresh candidate, fresh buffer, so none of that
			// leaks into whatever mapCond renders next for the same ifs.
			beginObjEqCandidate(pass)
			method, cargs := mapCond(pass, ifs.Cond)
			if method == "" {
				return true
			}
			// A message argument lives inside the failure branch, so the
			// condition guards it. `if p != nil { t.Errorf("got %s",
			// p.Id.Name) }` evaluates p.Id.Name only when p is non-nil;
			// as an assertion argument it is evaluated always, and the
			// passing case then panics. True and False assert the whole
			// condition rather than a piece of it, so guardsItsMessage's
			// narrower, guard-shape-specific check does not apply to them;
			// messageArgsAreSafe asks the more conservative question that
			// does.
			if method == "True" || method == "False" {
				if !messageArgsAreSafe(pass, ce.Args) {
					return true
				}
			} else if guardsItsMessage(pass, ifs) || boundsItsMessage(pass, ifs) {
				return true
			}
			margs := make([]string, 0, len(ce.Args))
			for _, a := range ce.Args {
				margs = append(margs, render(pass, a))
			}
			if sel.Sel.Name == "Errorf" || sel.Sel.Name == "Fatalf" {
				margs = trimMessage(margs, cargs)
			} else {
				// t.Error and t.Fatal have no format string, so there is no
				// verb for trimMessage to find a trailing argument behind;
				// every argument is a value, printed as-is. One that is
				// itself already one of the assertion's own arguments
				// (`t.Fatal(err)` after `if err != nil`) would otherwise
				// print twice: once through the assertion's own message,
				// once again as a bare, redundant trailing argument.
				margs = dropRedundant(margs, cargs)
			}
			hoistText := ""
			inlineOK := false
			if inlineName != "" {
				// Only the arguments matter for the -f variants, whose
				// first argument is a format string: `"err: %v"`
				// mentioning err is text rather than a second evaluation.
				// t.Error and t.Fatal have no format string and every
				// argument is a value, so all of them count. Getting that
				// wrong left `t.Fatal(err)` referring to a variable whose
				// declaration had just been inlined away.
				// A string literal is text, never a reference, whichever
				// variant this is. Checking by position instead got both
				// ends wrong: it missed t.Fatal(err), whose first argument
				// is a value, and it declined t.Error("...was accepted")
				// for containing the variable's name in prose.
				safe := true
				for _, m := range margs {
					if isStringLit(m) || !mentions(m, inlineName) {
						continue
					}
					safe = false
					break
				}
				if safe {
					rhs := render(pass, initAssign.Rhs[0])
					substituted := make([]string, len(cargs))
					replaced := false
					for i, a := range cargs {
						if a == inlineName {
							a = rhs
							replaced = true
						}
						substituted[i] = a
					}
					if replaced {
						cargs = substituted
						inlineOK = true
					}
				}
			}
			// The message still needs the name as itself (not just as
			// prose, which trimMessage already dropped), so substituting
			// would evaluate the init expression twice; or this was a
			// multi-value init, which is never inlined. Either way, hoist
			// the init to its own statement instead, which evaluates it
			// once regardless, or decline if that is not safe either.
			if !inlineOK && initAssign != nil {
				if hasGoto {
					return true
				}
				names := make([]*ast.Ident, 0, len(initAssign.Lhs))
				for _, l := range initAssign.Lhs {
					if id, isIdent := l.(*ast.Ident); isIdent {
						names = append(names, id)
					}
				}
				if !hoistSafe(pass, ifs, names, parent) {
					return true
				}
				// Hoisting makes every declared name a real, always-in-scope
				// variable, evaluated as a message argument whether the
				// assertion passes or fails. `if v, ok := m[k]; ok {
				// t.Errorf("unexpected %s", v.Name) }` passes on exactly the
				// row where v is the zero value, so v.Name would panic on
				// the passing path once hoisted. guardsItsMessage already
				// catches this for the non-init `x != nil` shape; this is
				// the same guard for every name an init declares, since
				// inlining's own mentions check would have caught it (a
				// dereference always mentions its base) but hoisting has no
				// equivalent check of its own without this.
				for _, id := range names {
					if id.Name == "_" {
						continue
					}
					for _, a := range ce.Args {
						if derefs(pass, a, id.Name) {
							return true
						}
					}
				}
				hoistText = render(pass, initAssign)
			}
			sc.sites = append(sc.sites, site{
				node:   ifs,
				abort:  strings.HasPrefix(sel.Sel.Name, "Fatal"),
				method: method,
				args:   append(cargs, margs...),
				hoist:  hoistText,
			})
			commitObjEqCandidate(pass)
			return false
		})
		if len(sc.sites) > 0 {
			sc.recv = freeName(pass, body, ft)
			scopes = append(scopes, sc)
		}
	}

	insp.Preorder([]ast.Node{(*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)}, func(n ast.Node) {
		switch f := n.(type) {
		case *ast.FuncDecl:
			if f.Body == nil || !inTest(f.Pos()) {
				return
			}
			if name := nestedT(f.Type); name != "" {
				collect(f.Type, f.Body, name)
			}
		case *ast.FuncLit:
			if !inTest(f.Pos()) {
				return
			}
			if name := nestedT(f.Type); name != "" {
				collect(f.Type, f.Body, name)
			}
		}
	})

	// Converting testify is all-or-nothing per file, because both packages
	// want the identifier `assert` and only one can have it. A file with a
	// call this tool declines (a helper taking testing.TB, an Eventually, an
	// ErrorAs whose shape differs) keeps testify and is left entirely alone,
	// including its stdlib assertions: adding our import beside testify's
	// would not compile, and aliasing ours to something else would leave the
	// file reading out of two vocabularies at once.
	//
	// objectsAreEqualFixes runs after every scope above has been collected,
	// which is what makes objEqReplaced (cond.go) complete: collection's own
	// render() calls, on every other site's own condition and arguments,
	// are what actually decide which ObjectsAreEqual calls survive into the
	// final text, and objectsAreEqualFixes only has to ask that record the
	// question rather than re-derive it.
	objEqFixed := objectsAreEqualFixes(pass)
	converted := map[*ast.File]int{}
	for _, sc := range scopes {
		f := fileOf(pass, sc.body.Pos())
		for _, s := range sc.sites {
			if s.testify {
				converted[f]++
			}
		}
	}
	blocked := map[*ast.File]bool{}
	for _, f := range pass.Files {
		if testifyRefs(pass, f)-objEqFixed[f] > converted[f] {
			blocked[f] = true
			reportBlockers(pass, f)
		}
	}
	kept := scopes[:0]
	for _, sc := range scopes {
		if f := fileOf(pass, sc.body.Pos()); f != nil && blocked[f] {
			continue
		}
		kept = append(kept, sc)
	}
	scopes = kept

	// One import diagnostic per file: drop testify, add ours. Reported
	// separately from the rewrites rather than bundled into one of them.
	// Bundling made the import share that fix's fate, and a fix the driver
	// declined to apply took the import with it, leaving a file full of
	// assert calls that did not compile.
	touched := map[*ast.File]bool{}
	for _, sc := range scopes {
		if f := fileOf(pass, sc.body.Pos()); f != nil {
			touched[f] = true
		}
	}
	// A file fully cleared of testify by objectsAreEqualFixes alone, with no
	// site of ours to justify adding our import, still needs testify's import
	// dropped: the fix above removed its last reference, and an import with
	// no remaining use of it is a compile error.
	testifyOnlyCleared := map[*ast.File]bool{}
	for f, n := range objEqFixed {
		if !touched[f] && !blocked[f] && n > 0 && testifyRefs(pass, f) == n {
			testifyOnlyCleared[f] = true
		}
	}
	importFiles := map[*ast.File]bool{}
	for f := range touched {
		importFiles[f] = true
	}
	for f := range testifyOnlyCleared {
		importFiles[f] = true
	}
	for f := range importFiles {
		var edits []analysis.TextEdit
		for _, imp := range f.Imports {
			if !isTestifyImport(imp) {
				continue
			}
			start, end := lineSpan(pass, imp)
			edits = append(edits, analysis.TextEdit{Pos: start, End: end})
		}
		if touched[f] && !hasAssertImport(f) {
			if e, ok := importEdit(f); ok {
				edits = append(edits, e)
			}
		}
		// Every ObjectsAreEqual this pass resolved, standalone or embedded,
		// ends up calling reflect.DeepEqual somewhere in the file's final
		// text, so its import rides along with this same edit rather than
		// one of its own: see the longer comment on objectsAreEqualFixes.
		if objEqFixed[f] > 0 && !hasImport(f, "reflect") {
			if e, ok := importPathEdit(f, "reflect"); ok {
				edits = append(edits, e)
			}
		}
		if len(edits) == 0 {
			continue
		}
		pass.Report(analysis.Diagnostic{
			Pos:     f.Pos(),
			Message: "imports need the testkit/assert path",
			SuggestedFixes: []analysis.SuggestedFix{{
				Message:   "fix the assert imports",
				TextEdits: edits,
			}},
		})
	}

	for _, sc := range scopes {
		var edits []analysis.TextEdit
		aborting, collecting := 0, 0
		for _, s := range sc.sites {
			if s.abort {
				aborting++
			} else {
				collecting++
			}
		}
		mixed := aborting > 0 && collecting > 0
		ctor := "NewCollecting"
		if collecting == 0 {
			ctor = "NewAborting"
		}

		// A scope with one assertion gets no declaration at all: the
		// receiver would be named once and used once, and
		// assert.NewAborting(t).NoError(err) is the same line count the
		// original testify call had. Measured on multigres, where table
		// tests put a single assertion in each t.Run, this is the whole
		// difference between the conversion adding lines and removing them.
		inline := len(sc.sites) == 1

		for _, s := range sc.sites {
			base := sc.recv
			if inline {
				base = fmt.Sprintf("assert.%s(%s)", ctor, sc.tName)
			} else if mixed && s.abort {
				base = sc.recv + ".Require()"
			}
			if s.hoist != "" {
				edits = append(edits, hoistEdit(pass, s.node, s.hoist))
			}
			leading, trailing := keptComments(pass, s.node)
			edits = append(edits, analysis.TextEdit{
				Pos: s.node.Pos(),
				End: s.node.End(),
				NewText: []byte(leading +
					fmt.Sprintf("%s.%s(%s)", base, s.method, strings.Join(s.args, ", ")) +
					trailing),
			})
		}

		if inline {
			pass.Report(analysis.Diagnostic{
				Pos:     sc.body.Pos(),
				Message: "1 assertion can use testkit/assert",
				SuggestedFixes: []analysis.SuggestedFix{{
					Message:   "rewrite with testkit/assert",
					TextEdits: edits,
				}},
			})
			continue
		}

		// Insert the receiver after any leading t.Parallel()/t.Setenv chain,
		// so it reads where a human would have written it.
		at := sc.body.Lbrace + 1
		for _, st := range sc.body.List {
			if !isTestPreamble(st, sc.tName) {
				break
			}
			at = st.End()
		}
		// Then push it to the start of the next line rather than inserting
		// mid-line. A trailing comment lives after the token this lands on,
		// so inserting here would move that comment down onto the line the
		// declaration now occupies. For prose that is merely untidy; for
		// `func TestX(t *testing.T) { //nolint:gocyclo` it silently
		// reattaches the directive to a different statement.
		at = nextLineStart(pass, at)
		edits = append(edits, analysis.TextEdit{
			Pos:     at,
			End:     at,
			NewText: []byte(fmt.Sprintf("\t%s := assert.%s(%s)\n", sc.recv, ctor, sc.tName)),
		})

		pass.Report(analysis.Diagnostic{
			Pos:     sc.body.Pos(),
			Message: fmt.Sprintf("%d assertion(s) can use testkit/assert", len(sc.sites)),
			SuggestedFixes: []analysis.SuggestedFix{{
				Message:   "rewrite with testkit/assert",
				TextEdits: edits,
			}},
		})
	}
	return nil, nil
}

// nextLineStart returns the first position on the line after pos, so an
// insertion lands on a line of its own. Falls back to pos at the end of the
// file, where there is no next line and nothing after pos to displace.
func nextLineStart(pass *analysis.Pass, pos token.Pos) token.Pos {
	tf := pass.Fset.File(pos)
	if tf == nil {
		return pos
	}
	next := pass.Fset.Position(pos).Line + 1
	if next > tf.LineCount() {
		return pos
	}
	return tf.LineStart(next)
}

// nestedT returns the parameter name bound to a testing handle, or "".
//
// *testing.T, *testing.B, *testing.F and testing.TB all satisfy assert.TB,
// and test helpers in these repos are written against all four.
func nestedT(ft *ast.FuncType) string {
	if ft.Params == nil {
		return ""
	}
	for _, f := range ft.Params.List {
		if len(f.Names) != 1 {
			continue
		}
		e := f.Type
		if star, ok := e.(*ast.StarExpr); ok {
			e = star.X
		}
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "testing" {
			continue
		}
		switch sel.Sel.Name {
		case "T", "B", "F", "TB":
			return f.Names[0].Name
		}
	}
	return ""
}

// isTestPreamble reports whether st is a leading call this tool inserts the
// receiver declaration after rather than before: t.Parallel(), t.Setenv(),
// t.Helper() and any t.Skip variant.
//
// A leading unconditional t.Skip still returns from the function, so a
// receiver declared above it is never reached and staticcheck's SA4006
// reports it as assigned and never used. Skipf and SkipNow both start with
// "Skip", so a prefix check covers all three without naming them separately.
func isTestPreamble(st ast.Stmt, tName string) bool {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return false
	}
	ce, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != tName {
		return false
	}
	switch sel.Sel.Name {
	case "Parallel", "Setenv", "Helper":
		return true
	}
	return strings.HasPrefix(sel.Sel.Name, "Skip")
}

// keptComments returns the comments inside node, rendered for the single
// line that replaces it: those that started their own line go above it at
// node's indentation, and the rest (trailing a line, or inside an
// expression) go at its end.
//
// The replacement spans the whole if statement, so without this every
// comment inside it is deleted. The one that matters most is a trailing
// directive on the `if ... {` line: `//nolint:staticcheck` there suppresses
// a finding on the condition, which the converted line still carries, so
// dropping it turns a clean lint run into a failing one. Moving it to the
// end of the new line keeps it on the line it applies to.
func keptComments(pass *analysis.Pass, node ast.Node) (leading, trailing string) {
	f := fileOf(pass, node.Pos())
	if f == nil {
		return "", ""
	}
	indent := strings.Repeat("\t", pass.Fset.Position(node.Pos()).Column-1)
	for _, cg := range f.Comments {
		if cg.End() <= node.Pos() || cg.Pos() >= node.End() {
			continue
		}
		for _, c := range cg.List {
			if c.Pos() <= node.Pos() || c.End() > node.End() {
				continue
			}
			if startsOwnLine(pass, c.Pos()) {
				leading += c.Text + "\n" + indent
			} else {
				trailing += " " + c.Text
			}
		}
	}
	return leading, trailing
}

// freeName picks a receiver name not already used in this scope.
//
// `c` is the name worth having and plenty of tests already use it for a
// config, a client or a channel. Shadowing one produces code that compiles
// in some scopes and not others, so take the first name nobody has claimed.
//
// "This scope" is the function's own scope and every scope enclosing it, not
// just the identifiers its body mentions. The body walk alone missed a
// parameter the body never references (`func(t *testing.T, c *Client)`),
// which still owns its name, so `c :=` failed to compile. go/types records a
// function's scope, parameters included, on its *ast.FuncType rather than on
// the body block, and its parents run out through enclosing functions, the
// file's imports and the package. Walking the chain also avoids names that
// are only visible, not used, which is stricter than correctness needs (any
// name the body does use is already an identifier in the body) but keeps a
// closure from reading as though it redeclares an outer variable. The one
// outer name it cannot see is a receiver this tool inserts into an enclosing
// function, since that is not in the type-checked source; shadowing it is
// harmless, because every receiver's sites sit inside its own function.
// The body walk stays for the opposite direction: a `c` declared in a block
// nested inside the body lives in a child scope the chain never reaches, and
// the receiver declared above it would be shadowed there.
func freeName(pass *analysis.Pass, body *ast.BlockStmt, ft *ast.FuncType) string {
	used := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			used[id.Name] = true
		}
		return true
	})
	for sc := pass.TypesInfo.Scopes[ft]; sc != nil; sc = sc.Parent() {
		for _, n := range sc.Names() {
			used[n] = true
		}
	}
	return firstUnused(used)
}

// firstUnused takes the first candidate name the used set does not hold.
// The set, and therefore the interesting part, is freeName's; this half is
// pure so it stays unit-testable without a type-checked package.
func firstUnused(used map[string]bool) string {
	for _, n := range []string{"c", "ck", "chk", "asrt", "assertC"} {
		if !used[n] {
			return n
		}
	}
	return "assertC0"
}

const assertPath = "github.com/multigres/testkit/assert"

func fileOf(pass *analysis.Pass, pos token.Pos) *ast.File {
	for _, f := range pass.Files {
		if f.Pos() <= pos && pos <= f.End() {
			return f
		}
	}
	return nil
}

func hasAssertImport(f *ast.File) bool {
	return hasImport(f, assertPath)
}

func hasImport(f *ast.File, path string) bool {
	for _, imp := range f.Imports {
		if imp.Path != nil && imp.Path.Value == strconv.Quote(path) {
			return true
		}
	}
	return false
}

// importEdit inserts the assert import as its own group at the end of the
// block, which is where goimports would put a third-party path in these
// repos. gofmt sorts within the group afterwards.
func importEdit(f *ast.File) (analysis.TextEdit, bool) {
	return importPathEdit(f, assertPath)
}

// importPathEdit is importEdit generalised to any path, for the one other
// case that needs to add an import: a rewritten assert.ObjectsAreEqual needs
// reflect, which the file may not have had a reason to import before.
func importPathEdit(f *ast.File, path string) (analysis.TextEdit, bool) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT || gd.Rparen == token.NoPos {
			continue
		}
		return analysis.TextEdit{
			Pos:     gd.Rparen,
			End:     gd.Rparen,
			NewText: []byte("\n\t" + strconv.Quote(path) + "\n"),
		}, true
	}
	// A single unparenthesised import: rewrite it as a block.
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT || len(gd.Specs) != 1 {
			continue
		}
		spec := gd.Specs[0].(*ast.ImportSpec)
		return analysis.TextEdit{
			Pos: gd.Pos(),
			End: gd.End(),
			NewText: []byte("import (\n\t" + spec.Path.Value + "\n\n\t" +
				strconv.Quote(path) + "\n)"),
		}, true
	}
	return analysis.TextEdit{}, false
}

// mentions reports whether a rendered expression uses the identifier name,
// as a whole word rather than a substring.
func mentions(expr, name string) bool {
	for i := 0; i+len(name) <= len(expr); i++ {
		if expr[i:i+len(name)] != name {
			continue
		}
		before := byte(' ')
		if i > 0 {
			before = expr[i-1]
		}
		after := byte(' ')
		if i+len(name) < len(expr) {
			after = expr[i+len(name)]
		}
		if !isIdentByte(before) && !isIdentByte(after) {
			return true
		}
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// bindsAssertElsewhere reports whether the file already has an identifier
// "assert" bound to some other import.
func bindsAssertElsewhere(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path == nil || imp.Path.Value == strconv.Quote(assertPath) {
			continue
		}
		if isTestifyImport(imp) {
			continue
		}
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			p := strings.Trim(imp.Path.Value, `"`)
			if i := strings.LastIndex(p, "/"); i >= 0 {
				name = p[i+1:]
			} else {
				name = p
			}
		}
		if name == "assert" {
			return true
		}
	}
	return false
}

// guardsItsMessage reports whether the failure message dereferences a value
// the condition proves safe only inside the branch.
//
// The message is evaluated when the condition holds; as an assertion argument
// it is evaluated always. That only matters where the *negation* makes the
// expression panic, which in practice is two shapes:
//
//	if p != nil    { t.Errorf("got %s", p.Id.Name) }   // p is nil outside
//	if len(s) != 0 { t.Errorf("got %v", s[0]) }        // s is empty outside
//
// A comparison like `if cl.Server != "x"` guards nothing: cl.Server is
// readable either way. An earlier version declined on any shared identifier
// and took conversion from 78% to 44% for no safety gain.
//
// This only runs for the specific mappings (Nil, NotNil, NoError, Error, Len,
// Empty, NotEmpty) whose condition is already one of these two shapes by the
// time it gets here: every guard shape this recognizes is peeled off inside
// mapCond before it ever reaches a compound condition or falls through to
// fallback, so there is no `&&`/`||` case for this function to consider.
// messageArgsAreSafe covers that ground instead, for the methods that assert
// the whole condition rather than a piece of it.
func guardsItsMessage(pass *analysis.Pass, ifs *ast.IfStmt) bool {
	guarded := guardedRoot(pass, ifs.Cond)
	if guarded == "" {
		return false
	}
	call := ifs.Body.List[0].(*ast.ExprStmt).X.(*ast.CallExpr)
	for _, a := range call.Args {
		if derefs(pass, a, guarded) {
			return true
		}
	}
	return false
}

// boundsItsMessage reports whether the failure message indexes or slices
// with something the condition constrains, which is the bounds-check
// counterpart of guardsItsMessage's nil and emptiness shapes:
//
//	if i >= 0      { t.Fatalf("at %d: %v", i, turns[i]) }   // i is -1 outside
//	if n < len(s)  { t.Errorf("got %v", s[n]) }             // n is past the end
//
// The condition is evaluated on both paths, so an index expression that
// appears in the condition itself (`if got[i] != want[i] { ... got[i] }`) is
// already proven safe and does not count. Map indexing never panics and is
// skipped. What is left is any index or slice sharing a variable with the
// condition, where the condition's truth may be the only thing keeping it in
// range. Deciding whether this particular comparison actually bounds this
// particular index is range analysis, and a declined site stays a correct
// if-statement, so this declines on the shared variable alone.
func boundsItsMessage(pass *analysis.Pass, ifs *ast.IfStmt) bool {
	condVars := map[types.Object]bool{}
	condExprs := map[string]bool{}
	ast.Inspect(ifs.Cond, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			if obj, ok := pass.TypesInfo.Uses[v].(*types.Var); ok {
				condVars[obj] = true
			}
		case *ast.IndexExpr, *ast.SliceExpr:
			condExprs[render(pass, v.(ast.Expr))] = true
		}
		return true
	})
	if len(condVars) == 0 {
		return false
	}
	sharesVar := func(e ast.Expr) bool {
		found := false
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && condVars[pass.TypesInfo.Uses[id]] {
				found = true
			}
			return !found
		})
		return found
	}
	call := ifs.Body.List[0].(*ast.ExprStmt).X.(*ast.CallExpr)
	for _, a := range call.Args {
		found := false
		ast.Inspect(a, func(n ast.Node) bool {
			if found {
				return false
			}
			switch v := n.(type) {
			case *ast.IndexExpr:
				if _, isMap := pass.TypesInfo.TypeOf(v.X).Underlying().(*types.Map); isMap {
					return true
				}
			case *ast.SliceExpr:
			default:
				return true
			}
			e := n.(ast.Expr)
			if !condExprs[render(pass, e)] && sharesVar(e) {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// guardedRoot returns the rendered expression a nil or emptiness check
// protects. Rendered rather than an identifier name, because the guarded
// thing is as often a field path as a variable: pvc.Spec.StorageClassName
// != nil guards *pvc.Spec.StorageClassName just as p != nil guards p.Field.
func guardedRoot(pass *analysis.Pass, cond ast.Expr) string {
	be, ok := cond.(*ast.BinaryExpr)
	if !ok {
		return ""
	}
	switch be.Op {
	case token.NEQ, token.GTR:
	default:
		return ""
	}
	if isNil(be.Y) {
		return render(pass, be.X)
	}
	if inner := builtinLen(be.X); inner != nil && isZero(be.Y) {
		return render(pass, inner)
	}
	return ""
}

// messageArgsAreSafe reports whether every message argument can be
// evaluated unconditionally with no side effect and no possible panic,
// which is what True and False need: both pass the condition itself
// through unchanged as the asserted value, so the condition's own
// short-circuiting is preserved exactly as it was, but the message
// arguments beside it are new call arguments, evaluated on every run
// where the original only evaluated them inside the failing branch.
//
// Recognizing which part of an arbitrary condition guards which part of
// the message does not generalize: a guard can be parenthesized, negated,
// written with its operands swapped, joined into the condition by `||`
// rather than `&&`, or be a shape (a plain bool, a length compared to
// something other than zero) this tool never tried to recognize as a
// guard at all. `||` is not even the same risk as `&&`: `a || b` being
// false requires both to be false, so a guard on either conjunct is
// exactly as broken by it as a guard on the whole condition is; nothing
// about `||` implies the guard held.
//
// So this asks a different, narrower question instead: not "does the
// condition make this safe", but "is this safe regardless of the
// condition", by construction. An argument qualifies only as a literal, a
// plain identifier, or a selector whose every step resolves to a
// non-pointer, non-interface value: tc.name off a struct value cannot
// panic; p.Name off a pointer can, if p is nil on the pass, and an
// interface step can too, since even referencing a method value through a
// nil interface panics. Anything else, an index, a slice, a dereference,
// a type assertion, a receive, or a call, is declined outright rather than
// analyzed for whether this particular one is safe.
func messageArgsAreSafe(pass *analysis.Pass, args []ast.Expr) bool {
	for _, a := range args {
		if !safeMessageArg(pass, a) {
			return false
		}
	}
	return true
}

func safeMessageArg(pass *analysis.Pass, e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return safeMessageArg(pass, v.X)
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		if !safeMessageArg(pass, v.X) {
			return false
		}
		// A package-qualified identifier (metav1.ConditionTrue) is not a
		// selection at all in go/types' terms (it resolves through Uses,
		// not Selections), so sel == nil already excludes it; likewise a
		// direct pointer dereference (p.Name where p itself is *T) and a
		// field promoted through an embedded pointer or interface (o.Name,
		// where Outer embeds *Inner) both set Indirect(), since both
		// dereference something to reach the field, whether that
		// dereference is written explicitly or promoted. Requiring
		// Kind() == FieldVal also excludes a method value (o.M, bound but
		// not called here), which can panic the same way through an
		// embedded nil pointer or interface; a method expression (T.M) has
		// no Selection kind matching FieldVal either, so it is declined
		// too, harmlessly, since it evaluates no receiver and was never
		// the risk this guards against.
		sel := pass.TypesInfo.Selections[v]
		if sel == nil || sel.Indirect() || sel.Kind() != types.FieldVal {
			return false
		}
		return true
	default:
		return false
	}
}

// derefs reports whether expr reaches through the guarded expression rather
// than merely naming it. `p` is safe to evaluate; `p.Field`, `p[0]` and `*p`
// are not.
func derefs(pass *analysis.Pass, expr ast.Expr, guarded string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		var base ast.Expr
		switch v := n.(type) {
		case *ast.SelectorExpr:
			base = v.X
		case *ast.IndexExpr:
			base = v.X
		case *ast.StarExpr:
			base = v.X
		default:
			return true
		}
		if render(pass, base) == guarded {
			found = true
			return false
		}
		return true
	})
	return found
}

// isStringLit reports whether a rendered argument is a string constant.
func isStringLit(s string) bool {
	return len(s) > 0 && (s[0] == '"' || s[0] == '`')
}

// isTestifyImport reports whether an import is testify's assert or require.
func isTestifyImport(imp *ast.ImportSpec) bool {
	if imp.Path == nil {
		return false
	}
	p := strings.Trim(imp.Path.Value, `"`)
	return p == "github.com/stretchr/testify/assert" ||
		p == "github.com/stretchr/testify/require"
}

// testifyRefs counts references to a testify package in a file, which is the
// number of call sites a full conversion has to account for.
func testifyRefs(pass *analysis.Pass, f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		pn, ok := pass.TypesInfo.Uses[id].(*types.PkgName)
		if !ok {
			return true
		}
		switch pn.Imported().Path() {
		case "github.com/stretchr/testify/assert", "github.com/stretchr/testify/require":
			n++
		}
		return true
	})
	return n
}

// lineSpan returns the range covering every whole line a node sits on,
// including the trailing newline, so deleting it leaves no blank line behind.
func lineSpan(pass *analysis.Pass, n ast.Node) (token.Pos, token.Pos) {
	tf := pass.Fset.File(n.Pos())
	start := tf.LineStart(pass.Fset.Position(n.Pos()).Line)
	if end := pass.Fset.Position(n.End()).Line + 1; end <= tf.LineCount() {
		return start, tf.LineStart(end)
	}
	return start, n.End()
}

// reportBlockers names the testify calls that held a file back, on stderr
// and only when asked. Without it, "this file was skipped" is the whole
// answer, and the next question is always which call did it.
func reportBlockers(pass *analysis.Pass, f *ast.File) {
	if os.Getenv("ASSERTFIX_DEBUG") == "" {
		return
	}
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pn, ok := pass.TypesInfo.Uses[id].(*types.PkgName)
		if !ok || !strings.HasPrefix(pn.Imported().Path(), "github.com/stretchr/testify/") {
			return true
		}
		if m, _, _, ok := testifyCall(pass, ce, scopeT(pass, ce)); ok {
			_ = m
			return true
		}
		fmt.Fprintf(os.Stderr, "assertfix: %s: declined %s.%s\n",
			pass.Fset.Position(ce.Pos()), id.Name, sel.Sel.Name)
		return true
	})
}

// scopeT guesses the testing handle in scope for a call, for debug output
// only: the real collection already knows it.
func scopeT(pass *analysis.Pass, ce *ast.CallExpr) string {
	if len(ce.Args) == 0 {
		return ""
	}
	id, ok := ce.Args[0].(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}
