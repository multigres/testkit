// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
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

// errSentinel exercises the errors.Is path, which is the reason ErrorIs
// exists separately from ErrorContains.
var errSentinel = errors.New("sentinel")

func newC() (*C, *recorder) {
	r := &recorder{}
	return NewAborting(r), r
}

// TestCheckRoutesToFatalOrError pins the one decision every assertion shares.
// If this regressed, every assertion in the package would silently become
// non-fatal, and a test would carry on past a failed precondition.
func TestCheckRoutesToFatalOrError(t *testing.T) {
	t.Parallel()

	c, r := newC()
	c.NoError(errors.New("boom"))
	if len(r.fatals) != 1 || len(r.errors) != 0 {
		t.Fatalf(
			"default mode should be fatal, got %d fatal / %d error",
			len(r.fatals),
			len(r.errors),
		)
	}

	c2, r2 := newC()
	c2.Check().NoError(errors.New("boom"))
	if len(r2.errors) != 1 || len(r2.fatals) != 0 {
		t.Fatalf(
			"Check mode should be non-fatal, got %d fatal / %d error",
			len(r2.fatals),
			len(r2.errors),
		)
	}
}

// TestCheckDoesNotMutateTheReceiver guards against Check() flipping the
// original into non-fatal mode, which would make one c.Check() call silently
// disarm every later assertion in the test.
func TestCheckDoesNotMutateTheReceiver(t *testing.T) {
	t.Parallel()

	c, r := newC()
	_ = c.Check()
	c.NoError(errors.New("boom"))
	if len(r.fatals) != 1 {
		t.Fatalf("Check() leaked into the receiver: got %d fatal", len(r.fatals))
	}
}

func TestAssertionsPassOnGoodInput(t *testing.T) {
	t.Parallel()

	c, r := newC()
	c.NoError(nil)
	c.Error(errors.New("x"))
	c.True(true)
	c.False(false)
	c.Eq(1, 1)
	c.Eq("a", "a")
	c.NotEq(1, 2)
	c.Len([]int{1, 2}, 2)
	c.Empty([]string{})
	c.Contains([]string{"a", "b"}, "b")
	c.NotContains([]string{"a"}, "z")
	c.HasKey(map[string]int{"k": 1}, "k")
	c.NotHasKey(map[string]int{"k": 1}, "other")
	c.ElementsMatch([]int{1, 2, 2}, []int{2, 1, 2})
	c.ElementsMatchDeep([]*box{{1}, {2}}, []*box{{2}, {1}})
	c.EqDeep(withFunc, withFunc)
	c.NotEqDeep(withFunc, &funcHolder{f: func() {}})
	c.NotEmpty([]int{1})
	c.EqDiff(map[string]string{"a": "1"}, map[string]string{"a": "1"})
	c.EqDiff([]string{"a"}, []string{"a"})
	c.ErrorIs(fmt.Errorf("wrap: %w", errSentinel), errSentinel)
	c.ErrorContains(errors.New("no such host"), "such host")

	if r.failed {
		t.Fatalf("no assertion should have failed, got fatals=%v errors=%v", r.fatals, r.errors)
	}
}

// withFunc is a pointer to a struct go-cmp calls unequal to itself: neither
// go-cmp nor reflect.DeepEqual considers two non-nil funcs equal, so only
// the pointer-identity short-circuit saves EqDeep(x, x).
type funcHolder struct{ f func() }

var withFunc = &funcHolder{f: func() {}}

// box is a pointer-element stand-in: two *box holding the same N are equal
// to DeepEqual and to go-cmp, and unequal to ==.
type box struct{ N int }

func TestAssertionsFailOnBadInput(t *testing.T) {
	t.Parallel()

	for name, call := range map[string]func(*C){
		"NoError":     func(c *C) { c.NoError(errors.New("x")) },
		"Error":       func(c *C) { c.Error(nil) },
		"True":        func(c *C) { c.True(false) },
		"False":       func(c *C) { c.False(true) },
		"Eq":          func(c *C) { c.Eq(1, 2) },
		"NotEq":       func(c *C) { c.NotEq(1, 1) },
		"Len":         func(c *C) { c.Len([]int{1}, 2) },
		"Empty":       func(c *C) { c.Empty([]int{1}) },
		"Contains":    func(c *C) { c.Contains([]string{"a"}, "z") },
		"NotContains": func(c *C) { c.NotContains([]string{"a"}, "a") },
		"HasKey":      func(c *C) { c.HasKey(map[string]int{}, "k") },
		"NotHasKey":   func(c *C) { c.NotHasKey(map[string]int{"k": 1}, "k") },
		"ElemsMatch":  func(c *C) { c.ElementsMatch([]int{1, 2, 2}, []int{1, 1, 2}) },
		"ElemsDeep":   func(c *C) { c.ElementsMatchDeep([]*box{{1}}, []*box{{2}}) },
		"ElemsDeepN":  func(c *C) { c.ElementsMatchDeep([]*box{{1}}, []*box{{1}, {1}}) },
		"NotEmpty":    func(c *C) { c.NotEmpty([]int{}) },
		"EqDiffMap":   func(c *C) { c.EqDiff(map[string]string{"a": "1"}, map[string]string{"a": "2"}) },
		"EqDiffSlice": func(c *C) { c.EqDiff([]string{"a"}, []string{"b"}) },
		"ErrorIs":     func(c *C) { c.ErrorIs(errors.New("other"), errSentinel) },
		"ErrContains": func(c *C) { c.ErrorContains(errors.New("boom"), "nope") },
		"ErrContNil":  func(c *C) { c.ErrorContains(nil, "nope") },
	} {
		c, r := newC()
		call(c)
		if !r.failed {
			t.Errorf("%s did not fail on bad input", name)
		}
	}
}

// TestMessageIsCarried pins the half a generic comparator cannot supply. The
// values tell you what differed; only the caller's message tells you what
// claim was being made, which is what makes a failure readable.
func TestMessageIsCarried(t *testing.T) {
	t.Parallel()

	c, r := newC()
	c.Eq(1, 2, "pod %s should own the PVC", "pool-0")
	if len(r.fatals) != 1 {
		t.Fatalf("want one fatal, got %v", r.fatals)
	}
	got := r.fatals[0]
	if !strings.Contains(got, "pod pool-0 should own the PVC") {
		t.Errorf("message not carried into the failure: %q", got)
	}
	if !strings.Contains(got, "want 1") || !strings.Contains(got, "got 2") {
		t.Errorf("values not carried into the failure: %q", got)
	}
}

func TestMessageIsOptional(t *testing.T) {
	t.Parallel()

	c, r := newC()
	c.Eq(1, 2)
	if len(r.fatals) != 1 || strings.HasSuffix(r.fatals[0], ":") {
		t.Errorf("bare failure should not end in a dangling separator: %q", r.fatals)
	}
}

// TestRequireInvertsCheck pins the pair. Check on its own is one-way, and a
// test that needs both modes would otherwise have to hold two receivers.
func TestRequireInvertsCheck(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	c := NewAborting(r).Check()

	c.Eq(1, 2)
	if len(r.errors) != 1 || len(r.fatals) != 0 {
		t.Fatalf("Check should report: errors=%v fatals=%v", r.errors, r.fatals)
	}

	c.Require().Eq(3, 4)
	if len(r.fatals) != 1 {
		t.Errorf("Require should abort: fatals=%v", r.fatals)
	}

	// Neither derivation mutates its receiver.
	c.Eq(5, 6)
	if len(r.errors) != 2 {
		t.Errorf("the collecting receiver should still collect: errors=%v", r.errors)
	}
}

// TestNewCollectingMatchesTheComposedForm pins that it agrees with the
// composed form it exists to replace.
func TestNewCollectingMatchesTheComposedForm(t *testing.T) {
	t.Parallel()

	direct := &recorder{}
	NewCollecting(direct).Eq(1, 2)

	composed := &recorder{}
	NewAborting(composed).Check().Eq(1, 2)

	if len(direct.errors) != 1 || len(direct.fatals) != 0 {
		t.Errorf("NewCollecting should report and continue: errors=%v fatals=%v",
			direct.errors, direct.fatals)
	}
	if len(direct.errors) != len(composed.errors) || len(direct.fatals) != len(composed.fatals) {
		t.Errorf("NewCollecting and NewAborting().Check() disagree: %v/%v vs %v/%v",
			direct.errors, direct.fatals, composed.errors, composed.fatals)
	}
}
