// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHeavyDependencies is the reason this package is separate from
// ctrltest rather than a file inside it.
//
// assert is meant to be importable by an ordinary unit test in a repo that
// has never heard of Kubernetes. Nothing enforces that at compile time: a
// k8s.io import added here would build fine and only show up later, as
// controller-runtime and its transitive graph appearing in the go.mod of
// something that wanted fifteen assertions. So the boundary is asserted here
// instead, where breaking it is a failing test rather than a surprise in
// somebody else's dependency tree.
//
// Adding to the allowlist is a real decision, not a formality. Everything
// here is paid for by every consumer.
//
// Parsing each file rather than the directory: go/parser.ParseDir is
// deprecated, and the documented replacement is golang.org/x/tools/go/packages,
// which this test cannot use without adding the kind of dependency it exists
// to prevent.
func TestNoHeavyDependencies(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"github.com/google/go-cmp/cmp": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)

			// Standard library paths have no dot in their first segment,
			// which is the same rule the go command uses to tell a stdlib
			// package from a module path.
			if !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
				continue
			}
			if allowed[p] {
				continue
			}
			t.Errorf("%s imports %q, which is not on the allowlist; assert "+
				"must stay importable without pulling in a dependency graph",
				name, p)
		}
	}

	// A guard that silently checks nothing is worse than no guard: a rename
	// or a move that left this looking at an empty directory would keep
	// passing forever.
	if checked == 0 {
		t.Fatal("found no non-test Go files to check; this guard is not looking at the package")
	}
}
