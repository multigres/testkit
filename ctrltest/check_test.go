// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// recorder stands in for *testing.T so the failure paths can be exercised.
// Fatalf must not call runtime.Goexit here, which is the whole reason C is
// written against TB rather than the concrete type.
type recorder struct {
	errors []string
	fatals []string
	logs   []string
	failed bool
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, sprintf(format, args...))
	r.failed = true
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, sprintf(format, args...))
	r.failed = true
}

func (r *recorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, sprintf(format, args...))
}

func (r *recorder) Error(args ...any) {
	r.errors = append(r.errors, fmt.Sprint(args...))
	r.failed = true
}
func (r *recorder) Cleanup(func()) {}
func (r *recorder) Failed() bool   { return r.failed }
func (r *recorder) Name() string   { return "recorder" }

// Context returns a live context rather than t.Context, since nothing here
// drives a real API call; the assertions under test never read it.
func (r *recorder) Context() context.Context { return context.Background() }

func sprintf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}

// TestBareCaseRefusesNamespaceScopedMethods pins the guard, and pins it
// against a TB that does not Goexit, which is the case the guard exists for.
// A real *testing.T would abort inside Fatalf and never reach the panic; this
// recorder returns, and without the panic the next line dereferences a nil
// Suite.
func TestBareCaseRefusesNamespaceScopedMethods(t *testing.T) {
	t.Parallel()
	ck := assert.NewCollecting(t)

	for name, call := range map[string]func(*C){
		"Client":           func(c *C) { c.Client() },
		"Watch":            func(c *C) { c.Watch() },
		"NewScript":        func(c *C) { c.NewScript() },
		"Cursor":           func(c *C) { c.Cursor() },
		"RequireQuiescent": func(c *C) { c.RequireQuiescent(0, 0) },
		"TryQuiescent":     func(c *C) { _ = c.TryQuiescent(0, 0) },
	} {
		r := &recorder{}
		c := Bare(r)

		func() {
			defer func() {
				ck.NotNil(recover(), "%s on a Bare case did not stop", name)
			}()
			call(c)
		}()

		ck.False(
			len(r.fatals) != 1 || !strings.Contains(r.fatals[0], "needs a namespace"),
			"%s: want one namespace complaint, got %v",
			name,
			r.fatals,
		)
	}
}

// TestBareCaseAllowsAssertions is the other half: a Bare case is useful only
// if the assertions work on it, which is the entire reason it exists.
func TestBareCaseAllowsAssertions(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)

	r := &recorder{}
	c := Bare(r)
	c.NoError(nil)
	c.Eq(1, 1)
	c.Len([]int{1}, 1)
	ck.False(r.failed, "assertions should work on a Bare case, got %v %v", r.fatals, r.errors)
}
