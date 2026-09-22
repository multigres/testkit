// SPDX-License-Identifier: Apache-2.0

package golden

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/multigres/testkit/assert"
)

// compareGolden and writeGolden hold all of AssertYAML's decision logic. They
// are tested directly, rather than through AssertYAML's *testing.T, because a
// mismatch or a missing file is supposed to fail the test it's called from:
// routing that through a real t.Run would mark this package's own tests
// failed by design. AssertYAML's two non-failing paths (match, update) are
// still covered end to end below.

func TestCompareGolden(t *testing.T) {
	dir := t.TempDir()

	t.Run("match", func(t *testing.T) {
		c := assert.NewCollecting(t)

		path := filepath.Join(dir, "match.golden.yaml")
		c.Require().NoError(os.WriteFile(path, []byte("a: b\n"), 0o600), "write fixture")
		diff, err := compareGolden(path, []byte("a: b\n"))
		c.Require().NoError(err, "compareGolden")
		c.Eq("", diff, "diff")
	})

	t.Run("mismatch", func(t *testing.T) {
		c := assert.NewCollecting(t)

		path := filepath.Join(dir, "mismatch.golden.yaml")
		c.Require().NoError(os.WriteFile(path, []byte("a: b\n"), 0o600), "write fixture")
		diff, err := compareGolden(path, []byte("a: c\n"))
		c.Require().NoError(err, "compareGolden")
		c.NotEq("", diff, "diff = \"\", want a non-empty diff for differing content")
	})

	t.Run("missing file", func(t *testing.T) {
		c := assert.NewCollecting(t)

		path := filepath.Join(dir, "missing.golden.yaml")
		_, err := compareGolden(path, []byte("a: b\n"))
		c.ErrorIs(err, os.ErrNotExist, "err")
	})
}

func TestWriteGolden(t *testing.T) {
	dir := t.TempDir()

	t.Run("new file", func(t *testing.T) {
		c := assert.NewCollecting(t)

		path := filepath.Join(dir, "new.golden.yaml")
		changeLog, err := writeGolden(path, []byte("a: b\n"))
		c.Require().NoError(err, "writeGolden")
		c.Eq("", changeLog, "changeLog")
		//nolint:gosec // path is a t.TempDir() fixture path, not user input
		got, err := os.ReadFile(path)
		c.Require().NoError(err, "read back %s", path)
		c.Eq("a: b\n", string(got), "file content = %q, want", got)
	})

	t.Run("unchanged content", func(t *testing.T) {
		c := assert.NewCollecting(t)

		path := filepath.Join(dir, "unchanged.golden.yaml")
		c.Require().NoError(os.WriteFile(path, []byte("a: b\n"), 0o600), "write fixture")
		changeLog, err := writeGolden(path, []byte("a: b\n"))
		c.Require().NoError(err, "writeGolden")
		c.Eq("", changeLog, "changeLog")
	})

	t.Run("changed content", func(t *testing.T) {
		c := assert.NewCollecting(t)

		path := filepath.Join(dir, "changed.golden.yaml")
		c.Require().NoError(os.WriteFile(path, []byte("a: b\n"), 0o600), "write fixture")
		changeLog, err := writeGolden(path, []byte("a: c\n"))
		c.Require().NoError(err, "writeGolden")
		c.NotEq("", changeLog, "changeLog = \"\", want a non-empty summary when content changes")
		//nolint:gosec // path is a t.TempDir() fixture path, not user input
		got, err := os.ReadFile(path)
		c.Require().NoError(err, "read back %s", path)
		c.Eq("a: c\n", string(got), "file content = %q, want", got)
	})
}

func TestAssertYAML_Match(t *testing.T) {
	c := assert.NewAborting(t)

	obj := map[string]string{"a": "b"}
	data, err := yaml.Marshal(obj)
	c.NoError(err, "marshal fixture")
	path := filepath.Join(t.TempDir(), "match.golden.yaml")
	c.NoError(os.WriteFile(path, data, 0o600), "write fixture")

	AssertYAML(t, obj, path)
}

func TestAssertYAML_Update(t *testing.T) {
	c := assert.NewAborting(t)

	path := filepath.Join(t.TempDir(), "update.golden.yaml")
	obj := map[string]string{"a": "b"}

	orig := Update
	Update = true
	t.Cleanup(func() { Update = orig })

	AssertYAML(t, obj, path)

	//nolint:gosec // path is a t.TempDir() fixture path, not user input
	got, err := os.ReadFile(path)
	c.NoError(err, "updating did not create %s", path)
	want, err := yaml.Marshal(obj)
	c.NoError(err, "marshal fixture")
	c.Eq(string(want), string(got), "golden file after update = %q, want %q", got, want)
}

// updating consults three sources, and the one worth a test is the third: a
// plain -update defined by the consuming package. This package cannot define
// that flag itself, because a dependency's init runs first and would make the
// consumer's own flag.Bool("update", ...) panic the test binary with "flag
// redefined", so it reads theirs instead.
func TestUpdatingHonoursAConsumersOwnUpdateFlag(t *testing.T) {
	c := assert.NewAborting(t)

	c.False(updating(), "nothing has asked for an update")

	// A flag set of our own rather than the command line's, since the real
	// one is already parsed and shared with every other test in this package.
	consumer := flag.NewFlagSet("consumer", flag.ContinueOnError)
	theirs := consumer.Bool("update", false, "rewrite my golden files")
	c.NoError(consumer.Parse([]string{"-update"}))
	c.True(*theirs)

	// flag.Lookup only sees the command line's set, so stand ours in for it
	// for the length of the test. This is the seam updating() reads through.
	orig := flag.CommandLine
	flag.CommandLine = consumer
	t.Cleanup(func() { flag.CommandLine = orig })

	c.True(updating(), "a consumer's own -update should be honoured")
}

// And the collision that motivated all of it: a consumer defining -update
// must not blow up because this package is linked in. That cannot be asserted
// from inside the package (the panic would be at init, before any test runs),
// so it is asserted structurally instead.
func TestThisPackageDoesNotOwnTheUpdateFlagName(t *testing.T) {
	c := assert.NewAborting(t)

	c.NotNil(flag.Lookup("golden.update"), "the namespaced flag is ours")

	f := flag.Lookup("update")
	if f == nil {
		return // nobody in this binary defines it, which is the usual case
	}
	c.NotStrContains(f.Usage, "rewrite golden files",
		"this package must not register -update; a consumer's own flag.Bool "+
			"would then panic with \"flag redefined\" at init")
}
