// SPDX-License-Identifier: Apache-2.0

// Package golden compares a value's YAML marshalling against a checked-in
// file, for tests that would otherwise assert field-by-field against a
// literal struct.
//
// It is a leaf package on purpose: it imports only flag, os, go-cmp and
// sigs.k8s.io/yaml, so any package under test can depend on it without
// risking an import cycle back to itself. Notably it does not import testing,
// which would link the whole testing flag set into any binary that
// transitively depends on it.
//
// Rewrite the files with -golden.update. See Update for why the flag is not
// simply spelled -update.
package golden

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"
)

// TB is the slice of *testing.T this package needs, declared here rather than
// taken from testkit/assert so that the leaf promise above holds: a package
// this one is used to test must be able to depend on it without pulling in
// anything else of ours.
//
// *testing.T satisfies it, so a call site passes t unchanged.
type TB interface {
	Helper()
	Name() string
	Log(args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Update makes AssertYAML rewrite the golden file instead of comparing it.
//
// It is bound to -golden.update rather than to a plain -update, and the
// namespacing is not decoration. A library cannot own the name "update": a
// dependency's package-level flag.Bool runs before its importer's, so this
// package registering "update" would make a consumer's own
// flag.Bool("update", ...), which is the conventional spelling for exactly
// this job, panic the whole test binary at init with "flag redefined". That
// is a hard failure in a package that may only be here for one helper.
//
// So there are three ways in, in the order updating consults them: this
// variable, which a TestMain can set directly; -golden.update, which this
// package owns and nothing else will collide with; and a plain -update if the
// consuming package happens to define one, which is honoured rather than
// redefined.
var Update bool

func init() {
	flag.BoolVar(&Update, "golden.update", false, "rewrite golden files")
}

// updateFlagName is the conventional spelling this package defers to when a
// consumer has already claimed it.
const updateFlagName = "update"

// updating reports whether this run should rewrite rather than compare.
func updating() bool {
	if Update {
		return true
	}
	f := flag.Lookup(updateFlagName)
	if f == nil {
		return false
	}
	// A flag registered by someone else is only readable through Getter,
	// which every flag.Bool satisfies. Anything else named "update" is not
	// ours to interpret.
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return false
	}
	b, ok := g.Get().(bool)
	return ok && b
}

// AssertYAML marshals got to YAML and compares it against the golden file at
// path, which is relative to the caller's package (e.g.
// "testdata/x.golden.yaml").
//
// Run the test with -golden.update to write the file instead of comparing.
// The write logs what it changed relative to the previous content, if any, so
// a bulk update run stays self-reporting rather than silently rewriting a
// file nobody meant to touch.
//
// t is an interface rather than *testing.T so that this package need not
// import testing; *testing.T satisfies it.
func AssertYAML(t TB, got any, path string) {
	t.Helper()

	gotYAML, err := yaml.Marshal(got)
	if err != nil {
		t.Fatalf("marshal %s to YAML: %v", path, err)
	}

	if updating() {
		changeLog, err := writeGolden(path, gotYAML)
		if err != nil {
			t.Fatalf("write golden file %s: %v", path, err)
		}
		if changeLog != "" {
			t.Log(changeLog)
		}
		return
	}

	diff, err := compareGolden(path, gotYAML)
	switch {
	case errors.Is(err, os.ErrNotExist):
		t.Fatalf(
			"golden file %s does not exist; create it with: "+
				"go test -run %s -golden.update",
			path, t.Name(),
		)
	case err != nil:
		t.Fatalf("read golden file: %v", err)
	case diff != "":
		t.Errorf("%s does not match golden file (-want +got):\n%s", path, diff)
	}
}

// compareGolden returns the diff (go-cmp's "-want +got" form) between the
// golden file at path and gotYAML, or an error if the file cannot be read.
// An empty, non-error diff means the two match.
func compareGolden(path string, gotYAML []byte) (string, error) {
	//nolint:gosec // path is a literal testdata path, not user input
	wantYAML, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return cmp.Diff(string(wantYAML), string(gotYAML)), nil
}

// writeGolden writes gotYAML to path and returns a human-readable summary of
// what changed relative to any previous content at path, or "" if there was
// no previous file or no difference. Separating this from AssertYAML is what
// lets a bulk update run stay self-reporting instead of silently rewriting a
// file nobody meant to touch.
func writeGolden(path string, gotYAML []byte) (changeLog string, err error) {
	//nolint:gosec // path is a literal testdata path, not user input
	if want, err := os.ReadFile(path); err == nil {
		if diff := cmp.Diff(string(want), string(gotYAML)); diff != "" {
			changeLog = fmt.Sprintf("updating %s (-old +new):\n%s", path, diff)
		}
	}
	// Create the directory rather than requiring it. Exactly one testdata
	// directory exists in this repo today, in the package this helper moved
	// out of, so the first update in any package the helper is adopted by
	// would otherwise fail on the remedy the missing-file error just told the
	// caller to run.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("create golden directory: %w", err)
	}
	if err := os.WriteFile(path, gotYAML, 0o600); err != nil {
		return "", err
	}
	return changeLog, nil
}
