// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// The conversions, end to end, against golden files.
//
// Golden rather than assertions on diagnostics, because what this tool
// produces is source: a fix that reports the right message and emits the
// wrong receiver is a failure, and only the applied output shows it. The
// testify stubs under testdata/src are there because analysistest resolves
// imports out of that tree as a GOPATH.
func TestConversions(t *testing.T) {
	analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), Analyzer,
		"stdlibcase", "testifycase")
}

// The refusals, which are the half of this tool that carries the risk. A
// site left alone costs three lines; a site converted wrongly costs a test,
// silently. testdata/src/refusals has no `want` comments and no golden file,
// so analysistest fails on any diagnostic at all.
func TestRefusals(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "refusals")
}

// A file holding one testify call this tool declines keeps testify and is
// left entirely alone, including its stdlib assertions: both packages want
// the identifier `assert` and only one can have it.
func TestTestifyConversionIsAllOrNothingPerFile(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "blockedfile")
}

func TestMentionsMatchesWholeIdentifiersOnly(t *testing.T) {
	for _, tc := range []struct {
		expr, name string
		want       bool
	}{
		{"err", "err", true},
		{"fmt.Errorf(\"%w\", err)", "err", true},
		{"err.Error()", "err", true},
		{"errs", "err", false},
		{"myerr", "err", false},
		{"my_err", "err", false},
		{"err2", "err", false},
		{`"was accepted"`, "err", false},
		{"", "err", false},
	} {
		if got := mentions(tc.expr, tc.name); got != tc.want {
			t.Errorf("mentions(%q, %q) = %v, want %v", tc.expr, tc.name, got, tc.want)
		}
	}
}

// The receiver name is `c` where it is free, and the first unused name
// otherwise. Shadowing an existing `c` would compile in some scopes and not
// others, which is the worst of both.
func TestFreeNameAvoidsEveryIdentifierInScope(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{
		{`{ x := 1; _ = x }`, "c"},
		{`{ c := 1; _ = c }`, "ck"},
		{`{ c, ck := 1, 2; _, _ = c, ck }`, "chk"},
		{`{ c, ck, chk, asrt, assertC := 1, 2, 3, 4, 5; _, _, _, _, _ = c, ck, chk, asrt, assertC }`, "assertC0"},
	} {
		if got := freeName(parseBlock(t, tc.body)); got != tc.want {
			t.Errorf("freeName(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestNestedTFindsEveryTestingHandle(t *testing.T) {
	for _, tc := range []struct {
		sig, want string
	}{
		{"func(t *testing.T)", "t"},
		{"func(b *testing.B)", "b"},
		{"func(f *testing.F)", "f"},
		{"func(tb testing.TB)", "tb"},
		{"func(name string, t *testing.T)", "t"},
		{"func(s string)", ""},
		{"func()", ""},
		// Two names on one parameter is not a handle this tool can address.
		{"func(a, t *testing.T)", ""},
	} {
		expr, err := parser.ParseExpr(tc.sig + " {}")
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sig, err)
		}
		fn, ok := expr.(*ast.FuncLit)
		if !ok {
			t.Fatalf("parse %q: got %T", tc.sig, expr)
		}
		if got := nestedT(fn.Type); got != tc.want {
			t.Errorf("nestedT(%q) = %q, want %q", tc.sig, got, tc.want)
		}
	}
}

func TestIsStringLitRecognisesBothQuoteForms(t *testing.T) {
	for expr, want := range map[string]bool{
		`"a"`:    true,
		"`a`":    true,
		`x`:      false,
		`'a'`:    false,
		``:       false,
		`f("a")`: false,
	} {
		if got := isStringLit(expr); got != want {
			t.Errorf("isStringLit(%q) = %v, want %v", expr, got, want)
		}
	}
}

func parseBlock(t *testing.T, src string) *ast.BlockStmt {
	t.Helper()
	expr, err := parser.ParseExpr("func() " + src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	fn, ok := expr.(*ast.FuncLit)
	if !ok {
		t.Fatalf("parse %q: got %T", src, expr)
	}
	return fn.Body
}

// The golden files are compared as text, so a fix that emitted syntactically
// broken Go would match a golden recording the same broken Go and the test
// would pass. Parsing them closes that, cheaply and without needing the
// import graph inside testdata's GOPATH tree.
func TestGoldenFilesAreValidGo(t *testing.T) {
	goldens, err := filepath.Glob(
		filepath.Join(analysistest.TestData(), "src", "*", "*.golden"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(goldens) == 0 {
		t.Fatal("no golden files found; the conversion tests are not testing anything")
	}
	for _, g := range goldens {
		_, err := parser.ParseFile(token.NewFileSet(), g, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("%s is not valid Go: %v", g, err)
		}
	}
}

// Every golden must also actually use the package this tool exists to move
// code onto. A fix that quietly stopped rewriting anything would leave the
// goldens equal to their inputs and every other test here would still pass.
func TestGoldenFilesActuallyConvert(t *testing.T) {
	goldens, err := filepath.Glob(
		filepath.Join(analysistest.TestData(), "src", "*", "*.golden"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, g := range goldens {
		//nolint:gosec // g came from a glob over this package's own testdata
		out, err := os.ReadFile(g)
		if err != nil {
			t.Fatalf("read %s: %v", g, err)
		}
		if !bytes.Contains(out, []byte(assertPath)) {
			t.Errorf("%s does not import %s; did the rewrite stop firing?", g, assertPath)
		}
		if bytes.Contains(out, []byte("stretchr/testify")) {
			t.Errorf("%s still imports testify after conversion", g)
		}

		in, err := os.ReadFile(strings.TrimSuffix(g, ".golden"))
		if err != nil {
			t.Fatalf("read input for %s: %v", g, err)
		}
		if bytes.Equal(in, out) {
			t.Errorf("%s is identical to its input", g)
		}
	}
}
