// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fsFixture lays out one of each thing the filesystem assertions distinguish
// and returns the paths: a regular file, a directory, and a name that is
// nothing at all.
func fsFixture(t *testing.T) (file, dir, missing string) {
	t.Helper()
	root := t.TempDir()
	file = filepath.Join(root, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	dir = filepath.Join(root, "a-dir")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	return file, dir, filepath.Join(root, "nothing-here")
}

func TestFilesystemAssertionsPassOnGoodInput(t *testing.T) {
	t.Parallel()

	file, dir, missing := fsFixture(t)
	r := &recorder{}
	c := NewAborting(r)

	c.FileExists(file)
	c.NoFileExists(missing)
	c.DirExists(dir)
	c.NoDirExists(missing)
	// A file is not a directory, so NoDirExists is satisfied by one. That
	// asymmetry with NoFileExists is deliberate and is the pair of cases
	// most likely to be "simplified" into agreement.
	c.NoDirExists(file)

	if r.failed {
		t.Fatalf("good input should not fail: fatals=%v errors=%v", r.fatals, r.errors)
	}
}

func TestFilesystemAssertionsFailOnBadInput(t *testing.T) {
	t.Parallel()

	file, dir, missing := fsFixture(t)

	for name, call := range map[string]func(*C){
		"FileExists/missing":     func(c *C) { c.FileExists(missing) },
		"FileExists/is a dir":    func(c *C) { c.FileExists(dir) },
		"NoFileExists/is a file": func(c *C) { c.NoFileExists(file) },
		// Not "no regular file exists": a cleanup that left a directory of
		// the same name behind has not removed the thing.
		"NoFileExists/is a dir": func(c *C) { c.NoFileExists(dir) },
		"DirExists/missing":     func(c *C) { c.DirExists(missing) },
		"DirExists/is a file":   func(c *C) { c.DirExists(file) },
		"NoDirExists/is a dir":  func(c *C) { c.NoDirExists(dir) },
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			call(NewCollecting(r))
			if !r.failed {
				t.Fatalf("%s should have failed", name)
			}
		})
	}
}

// A path that exists but cannot be statted is reported as the error it is,
// not folded into "does not exist": the two have different fixes.
func TestFilesystemAssertionsDistinguishAnErrorFromAnAbsence(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root can traverse a 0000 directory, so there is no stat error to make")
	}

	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inside := filepath.Join(locked, "f")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restored so t.TempDir's own cleanup can remove the directory; 0700 on
	// a directory is the mode it was created with.
	//nolint:gosec // G302: a directory needs its execute bit to be traversable
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	// NoFileExists is the one that would otherwise pass for the wrong
	// reason: an unreadable parent looks exactly like an absent file, and a
	// test asserting a cleanup worked would go green over a file still
	// sitting there.
	r := &recorder{}
	NewCollecting(r).NoFileExists(inside)
	if !r.failed {
		t.Fatal("an unstattable path should not read as absent")
	}
	if got := r.errors[0]; !strings.Contains(got, "could not stat") {
		t.Errorf("error = %q, want it to name the stat failure", got)
	}
}
