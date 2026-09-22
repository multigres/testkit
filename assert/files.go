// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"errors"
	"io/fs"
	"os"
)

// The filesystem assertions are here rather than in assert.go because they are
// the only ones that touch anything outside their arguments, and that is worth
// a file boundary: everything else in this package is a pure comparison, and a
// reader tracking down a test that fails only on one machine should be able to
// find the ones that can.
//
// They clear the same evidence bar as the rest of the vocabulary. Measured
// across multigres' test suites: 37 call sites between the four of them, which
// is more than ErrorIs, Same and ElementsMatch combined.
//
// A file that exists but cannot be statted (an unreadable parent directory, a
// broken symlink's target) is reported as the error it is rather than folded
// into "does not exist". The two have different fixes and a test that confused
// them would send its reader to the wrong one.

// stat classifies a path into exists/kind and any error that is not a plain
// absence. Separate so the four assertions below cannot drift on what counts
// as missing.
func stat(path string) (info fs.FileInfo, exists bool, err error) {
	info, err = os.Stat(path)
	switch {
	case err == nil:
		return info, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

// FileExists fails unless path exists and is a regular file.
//
// A directory at path fails: the assertion is about a file, and reporting a
// directory as satisfying it is how a test passes against a path the code
// under test cannot open.
func (c *C) FileExists(path string, msgAndArgs ...any) {
	c.Helper()
	info, exists, err := stat(path)
	switch {
	case err != nil:
		c.fail("want the file %s, could not stat it: %v%s", path, err, msg(msgAndArgs))
	case !exists:
		c.fail("want the file %s to exist%s", path, msg(msgAndArgs))
	case info.IsDir():
		c.fail("want the file %s, got a directory%s", path, msg(msgAndArgs))
	}
}

// NoFileExists fails if path exists, whatever it is.
//
// Deliberately not "no regular file exists at path": a test asserting that a
// cleanup removed something is not satisfied by a directory of the same name
// having taken its place.
func (c *C) NoFileExists(path string, msgAndArgs ...any) {
	c.Helper()
	info, exists, err := stat(path)
	switch {
	case err != nil:
		c.fail("want nothing at %s, could not stat it: %v%s", path, err, msg(msgAndArgs))
	case exists && info.IsDir():
		c.fail("want nothing at %s, got a directory%s", path, msg(msgAndArgs))
	case exists:
		c.fail("want nothing at %s, got a file of %d byte(s)%s",
			path, info.Size(), msg(msgAndArgs))
	}
}

// DirExists fails unless path exists and is a directory.
func (c *C) DirExists(path string, msgAndArgs ...any) {
	c.Helper()
	info, exists, err := stat(path)
	switch {
	case err != nil:
		c.fail("want the directory %s, could not stat it: %v%s", path, err, msg(msgAndArgs))
	case !exists:
		c.fail("want the directory %s to exist%s", path, msg(msgAndArgs))
	case !info.IsDir():
		c.fail("want the directory %s, got a file%s", path, msg(msgAndArgs))
	}
}

// NoDirExists fails if a directory exists at path. A regular file there
// satisfies it, which is the difference from NoFileExists.
func (c *C) NoDirExists(path string, msgAndArgs ...any) {
	c.Helper()
	info, exists, err := stat(path)
	switch {
	case err != nil:
		c.fail("want no directory at %s, could not stat it: %v%s", path, err, msg(msgAndArgs))
	case exists && info.IsDir():
		c.fail("want no directory at %s, got one%s", path, msg(msgAndArgs))
	}
}
