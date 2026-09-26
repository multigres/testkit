// SPDX-License-Identifier: Apache-2.0

// Package assert is a typed assertion vocabulary for Go tests.
//
// The assertions hang off a value that carries the T, so a test body threads
// one receiver rather than passing t into every helper. They are fatal by
// default, because most setup cannot meaningfully continue past a failure;
// Check returns a view whose assertions report and continue instead, which is
// a per-call decision rather than a mode the whole file has to adopt.
//
// The comparisons are generic methods, which is what requires Go 1.27 and
// what makes a comparison between mismatched types a compile error rather
// than a runtime one.
//
// Scope was chosen by counting what the test suites in these projects call,
// so the set is sized to real use rather than to completeness. That is also
// the bar for adding to it.
//
// Two kinds of assertion are left out on purpose. Type-coercing equality,
// which accepts 1 against int64(1), gives up the compile-time check the
// generic signatures exist for; convert explicitly at the call site instead.
// And IsType and Implements, which generics turned into compile-time
// concerns, so a runtime assertion has nothing to add.
package assert

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/go-cmp/cmp"
)

// TB is the slice of *testing.T this package needs.
//
// An interface rather than the concrete type, for the reason knowndefect.go
// already gives: the Fatalf and Errorf glue can then be exercised against a
// recorder instead of a real failure, which is the only way to test an
// assertion helper's failure path. Fatalf calls runtime.Goexit, so a fake that
// had to survive it is the trap this package refuses.
//
// Every exported helper here takes TB rather than *testing.T, which is both
// honest about what they use and what lets C expose them as methods. It is
// deliberately not testing.TB: that interface has an unexported method
// precisely to stop anyone outside the standard library implementing it, which
// would put the recorder above out of reach.
type TB interface {
	Helper()
	Error(args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Cleanup(func())
	Failed() bool
	Name() string
	Context() context.Context
}

// C is one test's handle: the T, and the assertions hanging off the same
// receiver.
//
// It exists because test bodies otherwise thread state by hand. Measured
// across multigres-operator's scenario tests before it existed: 112 calls took
// t as a leading argument, and none of that is what those tests are about.
//
// Assertions are fatal by default, because most of a test's setup cannot
// meaningfully continue past a failure. Use Check for the collecting form,
// which is worth having for the cases that genuinely want every violation in
// one run rather than the first.
//
// Embed it to add a domain vocabulary on the same receiver; testkit/ctrltest
// is the worked example. An embedder must shadow every method that returns a
// *C, which is Check, Require and Sub, or a caller silently drops back to
// this type and loses everything the outer one added. ctrltest.C keeps a
// compile-time assertion over all three, which is worth copying: the
// regression is otherwise silent, since every existing call site still
// compiles and only the added methods quietly disappear.
//
// A *C does not itself satisfy TB, and cannot: Error(err error, ...) shadows
// TB's Error(...any) with an incompatible signature, which is the right trade
// because a typed error assertion is worth more than the embedding. So a
// helper taking a TB, which is most of testkit, takes c.TB rather than c.
//
// Not safe for use from a goroutine other than the one that owns its T.
// Fatalf calls runtime.Goexit, which on a worker goroutine kills that
// goroutine without failing the test, so the assertion silently vanishes.
// Parallel subtests are fine: each gets its own T, and therefore its own C.
type C struct {
	TB

	check bool
}

// NewAborting returns a C reporting through t, whose assertions abort the
// test at the first failure.
//
// Aborting rather than Fatal, though Fatalf is the mechanism, so that it
// pairs with NewCollecting: both name what happens to the test rather than
// how severe the message is. It is also the older word, since C's assert
// calls abort.
//
// Neither constructor is spelled New. The two modes are used about equally
// across these projects, so making either the unmarked default would mean
// every reader of a call site had to remember which it was. Naming both puts
// the mode where it is chosen.
func NewAborting(t TB) *C {
	return &C{TB: t}
}

// NewCollecting returns a C whose assertions report and continue.
//
// Use this when a test is a list of independent facts about one result and
// seeing all the failures beats seeing whichever came first, and Require
// inside it for the assertion that makes the rest meaningless.
func NewCollecting(t TB) *C {
	return &C{TB: t, check: true}
}

// Sub is this case bound to a subtest's T.
//
// A C carries the T it reports through, so a parent's C used inside a subtest
// attributes the failure to the parent, and its Fatalf would Goexit the
// subtest's goroutine while marking the wrong test failed.
//
// The collecting mode does not carry over: a subtest is a fresh decision.
func (c *C) Sub(t TB) *C {
	c.Helper()
	out := *c
	out.TB = t
	out.check = false
	return &out
}

// Check returns a C whose assertions report and continue rather than abort.
//
// The returned value shares everything else with its receiver, so
// c.Check().Eq(...) is a per-call decision rather than a mode the whole test
// has to adopt.
func (c *C) Check() *C {
	out := *c
	out.check = true
	return &out
}

// fail routes to Errorf or Fatalf. Every assertion below goes through it, so
// the fatal/continue decision lives in exactly one place.
func (c *C) fail(format string, args ...any) {
	c.Helper()
	if c.check {
		c.Errorf(format, args...)
		return
	}
	c.Fatalf(format, args...)
}

// msg renders a caller-supplied explanation, if any. Every assertion takes a
// trailing msgAndArgs so the failure can state the claim rather than only the
// values, which is the half a generic comparator cannot supply.
//
// A lone string is printed verbatim, so a stray % in it is safe. Two or more
// arguments are treated as a format and its operands, and a mismatch there
// renders as fmt's %!v(MISSING) rather than being caught: go vet does not
// classify these assertions as printf wrappers, because they hand the
// rendered string to fail through a single %s rather than forwarding the
// varargs. The alternative would require escaping every non-format message
// that happens to contain a %.
func msg(msgAndArgs []any) string {
	if len(msgAndArgs) == 0 {
		return ""
	}
	if format, ok := msgAndArgs[0].(string); ok && len(msgAndArgs) > 1 {
		return ": " + fmt.Sprintf(format, msgAndArgs[1:]...)
	}
	return ": " + fmt.Sprint(msgAndArgs...)
}

// NoError fails unless err is nil.
func (c *C) NoError(err error, msgAndArgs ...any) {
	c.Helper()
	if err != nil {
		c.fail("unexpected error: %v%s", err, msg(msgAndArgs))
	}
}

// Error fails unless err is non-nil.
func (c *C) Error(err error, msgAndArgs ...any) {
	c.Helper()
	if err == nil {
		c.fail("expected an error, got nil%s", msg(msgAndArgs))
	}
}

// True fails unless cond holds.
func (c *C) True(cond bool, msgAndArgs ...any) {
	c.Helper()
	if !cond {
		c.fail("expected true%s", msg(msgAndArgs))
	}
}

// False fails unless cond does not hold.
func (c *C) False(cond bool, msgAndArgs ...any) {
	c.Helper()
	if cond {
		c.fail("expected false%s", msg(msgAndArgs))
	}
}

// Eq fails unless want and got are equal.
//
// Generic rather than taking any, so a comparison between mismatched types
// is a compile error instead of a runtime failure.
//
// It compares with ==, which for a Kubernetes type means its representation
// rather than its value. resource.Quantity is the trap: it is comparable, so
// this accepts it, but it caches the string it was parsed from, and
// Eq(MustParse("100m"), MustParse("0.1")) is false even though Cmp reports
// them equal. Use EqDiff on a Quantity, which dispatches to its Equal method.
//
// time.Time is the same shape and is worth naming separately, because it
// arrives from the standard library rather than from Kubernetes. It is
// comparable, so this accepts it, and == compares the wall clock, the
// monotonic reading and the *time.Location pointer, so two values the same
// instant apart in any of those are unequal here. The standard library says
// so itself and tells you to use Equal; EqDiff dispatches to it.
func (c *C) Eq[T comparable](want, got T, msgAndArgs ...any) {
	c.Helper()
	if want != got {
		c.fail("want %v, got %v%s", want, got, msg(msgAndArgs))
	}
}

// NotEq fails if want and got are equal.
func (c *C) NotEq[T comparable](want, got T, msgAndArgs ...any) {
	c.Helper()
	if want == got {
		c.fail("expected a value other than %v%s", want, msg(msgAndArgs))
	}
}

// Len fails unless v has exactly n elements.
//
// Takes any for the reason Nil does: len is defined on slices, maps, strings,
// arrays and channels, and no constraint covers exactly those. Constraining
// to ~[]E would compile and then refuse the map the caller actually had,
// which is a worse answer than a runtime kind check.
func (c *C) Len(v any, n int, msgAndArgs ...any) {
	c.Helper()
	got, ok := lengthOf(v)
	if !ok {
		c.fail("Len needs something with a length, got %T%s", v, msg(msgAndArgs))
		return
	}
	if got != n {
		c.fail("want %d element(s), got %d: %v%s", n, got, v, msg(msgAndArgs))
	}
}

// Empty fails unless v has no elements. A nil slice or map is empty.
func (c *C) Empty(v any, msgAndArgs ...any) {
	c.Helper()
	got, ok := lengthOf(v)
	if !ok {
		c.fail("Empty needs something with a length, got %T%s", v, msg(msgAndArgs))
		return
	}
	if got != 0 {
		c.fail("want empty, got %d element(s): %v%s", got, v, msg(msgAndArgs))
	}
}

// Contains fails unless s contains want.
func (c *C) Contains[S ~[]E, E comparable](s S, want E, msgAndArgs ...any) {
	c.Helper()
	for _, got := range s {
		if got == want {
			return
		}
	}
	c.fail("want %v to be present in %v%s", want, s, msg(msgAndArgs))
}

// NotContains fails if s contains unwanted.
func (c *C) NotContains[S ~[]E, E comparable](s S, unwanted E, msgAndArgs ...any) {
	c.Helper()
	for _, got := range s {
		if got == unwanted {
			c.fail("want %v to be absent from %v%s", unwanted, s, msg(msgAndArgs))
			return
		}
	}
}

// HasKey fails unless m has the given key.
func (c *C) HasKey[M ~map[K]V, K comparable, V any](m M, key K, msgAndArgs ...any) {
	c.Helper()
	if _, ok := m[key]; !ok {
		c.fail("want key %v to be present in %v%s", key, m, msg(msgAndArgs))
	}
}

// NotHasKey fails if m has the given key.
func (c *C) NotHasKey[M ~map[K]V, K comparable, V any](m M, key K, msgAndArgs ...any) {
	c.Helper()
	if v, ok := m[key]; ok {
		c.fail("want key %v to be absent, got value %v%s", key, v, msg(msgAndArgs))
	}
}

// NotEmpty fails if v has no elements.
func (c *C) NotEmpty(v any, msgAndArgs ...any) {
	c.Helper()
	got, ok := lengthOf(v)
	if !ok {
		c.fail("NotEmpty needs something with a length, got %T%s", v, msg(msgAndArgs))
		return
	}
	if got == 0 {
		c.fail("want at least one element, got none%s", msg(msgAndArgs))
	}
}

// EqDiff fails unless want and got are deeply equal, reporting a cmp diff.
//
// The one to reach for on maps, slices and structs, where Eq does not compile
// and a bool comparator would say only that two multi-key values differed
// without saying where. Also the right choice for a resource.Quantity or a
// metav1.Time, which Eq compares by representation; both implement Equal, and
// go-cmp prefers an Equal method over reflecting into the type.
//
// Panics on a type that has unexported fields AND no Equal method, as go-cmp
// always does. Most Kubernetes API types are safe for that reason rather than
// by luck. When it does panic, compare a projection rather than reaching for
// reflect.DeepEqual.
func (c *C) EqDiff[T any](want, got T, msgAndArgs ...any) {
	c.Helper()
	if diff := cmp.Diff(want, got); diff != "" {
		c.fail("mismatch (-want +got):\n%s%s", diff, msg(msgAndArgs))
	}
}

// EqDiffOpts is EqDiff with cmp options, for a comparison that needs to
// ignore a field or supply its own comparator.
//
// A separate method rather than a variadic tail on EqDiff: EqDiff's
// msgAndArgs is already `...any`, and a cmp.Option satisfies any exactly as
// well as a format string does, so a call mixing the two would be ambiguous
// at best and silently misparsed at worst. Taking opts as its own slice
// parameter keeps the two apart.
func (c *C) EqDiffOpts[T any](want, got T, opts []cmp.Option, msgAndArgs ...any) {
	c.Helper()
	if diff := cmp.Diff(want, got, opts...); diff != "" {
		c.fail("mismatch (-want +got):\n%s%s", diff, msg(msgAndArgs))
	}
}

// NotEqDiff fails if want and got are deeply equal.
//
// The counterpart to EqDiff, and it exists for the reason NotEqDeep does:
// == is not the negation of deep equality. NotEq does not compile on a map
// or a slice at all, and on a pointer it compares addresses, so two distinct
// pointers to identical structs pass NotEq and fail here. Reach for this
// wherever EqDiff is the positive form.
func (c *C) NotEqDiff[T any](want, got T, msgAndArgs ...any) {
	c.Helper()
	if cmp.Diff(want, got) == "" {
		c.fail("want a value not deeply equal to %v%s", want, msg(msgAndArgs))
	}
}

// EqDeep fails unless want and got are deeply equal, including unexported
// fields, reporting a cmp diff.
//
// The counterpart to EqDiff. go-cmp refuses an unexported field by default,
// on the view that a test reaching into another package's internals is
// asserting about something it was not given; that holds often enough to be
// the default and not always, since a type you own compared wholesale is
// ordinary.
//
// Prefer EqDiff. Reach for this where reflect.DeepEqual would have been the
// answer, and note that it couples the test to fields the package may change
// without telling anyone.
func (c *C) EqDeep[T any](want, got T, msgAndArgs ...any) {
	c.Helper()
	if sameAddress(want, got) {
		return
	}
	if diff := cmp.Diff(want, got, exportAll); diff != "" {
		c.fail("mismatch (-want +got):\n%s%s", diff, msg(msgAndArgs))
	}
}

// NotEqDeep fails if want and got are deeply equal, including unexported
// fields.
//
// The counterpart to EqDeep, and it exists for the same reason: == is not
// the negation of deep equality. Two pointers to identical structs are
// deeply equal and not ==, so NotEq passes where this fails. A migration
// that mapped a deep comparison onto NotEq would invert exactly those cases.
func (c *C) NotEqDeep[T any](want, got T, msgAndArgs ...any) {
	c.Helper()
	if sameAddress(want, got) || cmp.Diff(want, got, exportAll) == "" {
		c.fail("want a value deeply unequal to %v%s", want, msg(msgAndArgs))
	}
}

// exportAll lets go-cmp read unexported fields on every type it walks. It is
// the whole difference between EqDeep and EqDiff.
var exportAll = cmp.Exporter(func(reflect.Type) bool { return true })

// sameAddress reports whether two pointers are the same pointer.
//
// reflect.DeepEqual short-circuits there ("pointer values are deeply equal
// if they are equal using Go's == operator") and go-cmp does not: it walks
// the pointee, and a struct holding a func field is then unequal to itself,
// because neither go-cmp nor DeepEqual calls two non-nil funcs equal. So
// EqDeep(cfg, cfg) failed on a config carrying accessor closures. Taking
// the short-circuit restores what a reader means by asking whether it is
// the same object, and keeps parity with the DeepEqual this stands in for.
func sameAddress(a, b any) bool {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Kind() != reflect.Pointer || rb.Kind() != reflect.Pointer {
		return false
	}
	return !ra.IsNil() && ra.Pointer() == rb.Pointer()
}

// ErrorIs fails unless err matches target under errors.Is.
func (c *C) ErrorIs(err, target error, msgAndArgs ...any) {
	c.Helper()
	if !errors.Is(err, target) {
		c.fail("want an error matching %v, got %v%s", target, err, msg(msgAndArgs))
	}
}

// NotErrorIs fails if err matches target under errors.Is.
//
// The negated form, which is not the same claim as "no error": a call can
// legitimately fail and the test's point be that it did not fail *this* way.
// NoError is the assertion for "it did not fail at all".
func (c *C) NotErrorIs(err, target error, msgAndArgs ...any) {
	c.Helper()
	if errors.Is(err, target) {
		c.fail("want an error not matching %v, got %v%s", target, err, msg(msgAndArgs))
	}
}

// ErrorContains fails unless err is non-nil and its message contains want.
//
// A blunt instrument, deliberately: prefer ErrorIs where a sentinel exists.
// This is for the errors that only ever arrive as a string, which in this
// suite means most of what the API server returns.
func (c *C) ErrorContains(err error, want string, msgAndArgs ...any) {
	c.Helper()
	if err == nil {
		c.fail("want an error containing %q, got nil%s", want, msg(msgAndArgs))
		return
	}
	if !strings.Contains(err.Error(), want) {
		c.fail("want an error containing %q, got %v%s", want, err, msg(msgAndArgs))
	}
}

// ErrorWhen fails unless err's presence matches wantErr.
//
// The assertion behind `if (err != nil) != wantErr { fail }`, the shape a
// table test writes when one row wants an error and the next does not. Two
// messages rather than one, because "want true, got false" from a bare
// comparison of the boolean says nothing about which direction went wrong:
// this says whether an error was wanted and missing, or unwanted and present.
func (c *C) ErrorWhen(wantErr bool, err error, msgAndArgs ...any) {
	c.Helper()
	switch {
	case wantErr && err == nil:
		c.fail("expected an error, got nil%s", msg(msgAndArgs))
	case !wantErr && err != nil:
		c.fail("unexpected error: %v%s", err, msg(msgAndArgs))
	}
}

// Fail fails unconditionally. For the branch a test reached and should not
// have, where there is no condition left to assert.
func (c *C) Fail(msgAndArgs ...any) {
	c.Helper()
	c.fail("failed%s", msg(msgAndArgs))
}

// Zero fails unless v is its type's zero value.
func (c *C) Zero[T comparable](v T, msgAndArgs ...any) {
	c.Helper()
	var zero T
	if v != zero {
		c.fail("want the zero value, got %v%s", v, msg(msgAndArgs))
	}
}

// NotZero fails if v is its type's zero value.
func (c *C) NotZero[T comparable](v T, msgAndArgs ...any) {
	c.Helper()
	var zero T
	if v == zero {
		c.fail("want a non-zero value, got %v%s", v, msg(msgAndArgs))
	}
}

// Same fails unless want and got are the same pointer.
//
// Identity, not equality: two distinct objects with identical contents fail
// here and pass EqDiff. Generic over the pointee so a comparison between
// pointers to different types is a compile error.
func (c *C) Same[T any](want, got *T, msgAndArgs ...any) {
	c.Helper()
	if want != got {
		c.fail("want the same pointer (%p), got %p%s", want, got, msg(msgAndArgs))
	}
}

// NotSame fails if want and got are the same pointer.
func (c *C) NotSame[T any](want, got *T, msgAndArgs ...any) {
	c.Helper()
	if want == got {
		c.fail("want a different pointer, got the same one (%p)%s", want, msg(msgAndArgs))
	}
}

// StrContains fails unless s contains sub.
//
// A separate name from Contains rather than an overload, which Go does not
// have. Worth knowing when migrating from a library whose Contains is
// polymorphic over strings, slices and maps: the slice form here is
// Contains, this is the string form, and the error form is ErrorContains.
func (c *C) StrContains(s, sub string, msgAndArgs ...any) {
	c.Helper()
	if !strings.Contains(s, sub) {
		c.fail("want %q to contain %q%s", s, sub, msg(msgAndArgs))
	}
}

// NotStrContains fails if s contains sub.
func (c *C) NotStrContains(s, sub string, msgAndArgs ...any) {
	c.Helper()
	if strings.Contains(s, sub) {
		c.fail("want %q not to contain %q%s", s, sub, msg(msgAndArgs))
	}
}

// ElementsMatch fails unless want and got hold the same elements in any
// order, counting duplicates.
//
// A multiset comparison, not a set one: [a a b] and [a b b] do not match.
// Reach for this when the production order is genuinely unspecified, such as
// a map iteration; prefer EqDiff where order is part of the contract, since
// it reports where the difference is.
func (c *C) ElementsMatch[S ~[]E, E comparable](want, got S, msgAndArgs ...any) {
	c.Helper()
	counts := make(map[E]int, len(want))
	for _, e := range want {
		counts[e]++
	}
	for _, e := range got {
		counts[e]--
	}
	for _, n := range counts {
		if n != 0 {
			c.fail("want the same elements in any order\nwant: %v\ngot:  %v%s",
				want, got, msg(msgAndArgs))
			return
		}
	}
}

// ElementsMatchDeep is ElementsMatch for elements that == cannot compare,
// or compares wrongly.
//
// The ordinary form hashes elements into a map, which needs comparable and
// gives pointers address identity. A []*Foo whose elements hold equal values
// at different addresses matches nothing under ==, so the assertion fails on
// input a reader would call identical. This form compares with go-cmp,
// including unexported fields, at O(n*m) rather than O(n).
func (c *C) ElementsMatchDeep[S ~[]E, E any](want, got S, msgAndArgs ...any) {
	c.Helper()
	if len(want) != len(got) {
		c.fail("want the same elements in any order\nwant: %v\ngot:  %v%s",
			want, got, msg(msgAndArgs))
		return
	}
	used := make([]bool, len(got))
	for _, w := range want {
		found := false
		for i, g := range got {
			if used[i] || !cmp.Equal(w, g, exportAll) {
				continue
			}
			used[i], found = true, true
			break
		}
		if !found {
			c.fail("want the same elements in any order\nwant: %v\ngot:  %v%s",
				want, got, msg(msgAndArgs))
			return
		}
	}
}

// EqualError fails unless err is non-nil and its message is exactly want.
//
// Stricter than ErrorContains and more brittle: an error message is rarely a
// contract, so prefer ErrorIs against a sentinel. This exists for the cases
// where the message is the thing under test, such as a formatter.
func (c *C) EqualError(err error, want string, msgAndArgs ...any) {
	c.Helper()
	if err == nil {
		c.fail("want the error %q, got nil%s", want, msg(msgAndArgs))
		return
	}
	if err.Error() != want {
		c.fail("want the error %q, got %q%s", want, err.Error(), msg(msgAndArgs))
	}
}

// ErrorAs finds the first error in err's tree matching T and returns it.
//
// The extraction is errors.AsType, which the standard library gained in 1.26.
// What this adds is the assertion: AsType hands back a bool to check, and a
// test that forgets to check it carries on with the zero value. T is not
// inferable from the arguments, so instantiate it: c.ErrorAs[*net.OpError](err).
//
// On a collecting receiver a failure here still returns the zero value and
// execution continues, which is usually a nil dereference on the next line.
// Reach for c.Require().ErrorAs[...] where the result is used, which is
// almost everywhere.
func (c *C) ErrorAs[T error](err error, msgAndArgs ...any) T {
	c.Helper()
	target, ok := errors.AsType[T](err)
	if !ok {
		c.fail("want an error of type %T in the tree, got %v%s", target, err, msg(msgAndArgs))
	}
	return target
}

// Panics fails unless fn panics, and returns the recovered value.
func (c *C) Panics(fn func(), msgAndArgs ...any) (recovered any) {
	c.Helper()
	func() {
		defer func() { recovered = recover() }()
		fn()
	}()
	if recovered == nil {
		c.fail("want a panic, got none%s", msg(msgAndArgs))
	}
	return recovered
}

// NotPanics fails if fn panics, reporting what it panicked with.
//
// The panic is not re-raised: the point is to fail this test with a readable
// message, and re-raising would replace that with a runtime trace from
// whatever goroutine the deferred call unwound into.
func (c *C) NotPanics(fn func(), msgAndArgs ...any) {
	c.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		fn()
	}()
	if recovered != nil {
		c.fail("want no panic, got %v%s", recovered, msg(msgAndArgs))
	}
}

// Require returns a C whose assertions abort rather than report and continue.
//
// The inverse of Check, and the two exist as a pair because neither mode is
// the right default everywhere. A test whose assertions are mostly
// independent checks wants New(t).Check() at the top and Require() at the two
// places a failure makes the rest meaningless; a test that is mostly setup
// wants the opposite. Without an inverse the first choice would be one-way,
// and a test would have to carry two receivers to get both.
func (c *C) Require() *C {
	out := *c
	out.check = false
	return &out
}
