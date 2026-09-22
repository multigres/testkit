// SPDX-License-Identifier: Apache-2.0

// Package require is a stub of github.com/stretchr/testify/require, on the
// same terms as the assert stub beside it: require aborts where assert
// continues, which is the whole difference assertfix has to preserve.
package require

type TestingT interface {
	Errorf(format string, args ...any)
}

func Equal(t TestingT, expected, actual any, msgAndArgs ...any) bool    { return true }
func NotEqual(t TestingT, expected, actual any, msgAndArgs ...any) bool { return true }
func NoError(t TestingT, err error, msgAndArgs ...any) bool             { return true }
func Error(t TestingT, err error, msgAndArgs ...any) bool               { return true }
func ErrorIs(t TestingT, err, target error, msgAndArgs ...any) bool     { return true }
func True(t TestingT, value bool, msgAndArgs ...any) bool               { return true }
func False(t TestingT, value bool, msgAndArgs ...any) bool              { return true }
func Nil(t TestingT, object any, msgAndArgs ...any) bool                { return true }
func NotNil(t TestingT, object any, msgAndArgs ...any) bool             { return true }
func Len(t TestingT, object any, length int, msgAndArgs ...any) bool    { return true }
func Contains(t TestingT, s, contains any, msgAndArgs ...any) bool      { return true }
func Empty(t TestingT, object any, msgAndArgs ...any) bool              { return true }
func Greater(t TestingT, e1, e2 any, msgAndArgs ...any) bool            { return true }
func NotErrorIs(t TestingT, err, target error, msgAndArgs ...any) bool  { return true }
func FileExists(t TestingT, path string, msgAndArgs ...any) bool        { return true }
func NoFileExists(t TestingT, path string, msgAndArgs ...any) bool      { return true }
func DirExists(t TestingT, path string, msgAndArgs ...any) bool         { return true }
func NoDirExists(t TestingT, path string, msgAndArgs ...any) bool       { return true }

// Declined by assertfix, and here so a test can prove a file holding one is
// left entirely alone.
func EqualValues(t TestingT, expected, actual any, msgAndArgs ...any) bool { return true }
func Regexp(t TestingT, rx, str any, msgAndArgs ...any) bool               { return true }
