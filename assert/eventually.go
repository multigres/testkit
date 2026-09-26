// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"time"
)

// pollInterval is how long Eventually waits between attempts.
const pollInterval = 250 * time.Millisecond

// pollUntil is the shared poll loop for Eventually and EventuallyTrue: call
// check, and if it is not yet satisfied, sleep for at most tick before
// trying again, without ever sleeping past deadline. It reports whether
// check ever succeeded, and leaves failing to its caller: a callback
// invoked from here would run in its own frame, one this package's own
// Helper() calls cannot reach, and every failure would report a line inside
// this file instead of the caller's.
//
// check runs on the caller's own goroutine, not a separate one: a Require()
// inside it is legal, unlike under testify's Eventually, which runs the
// condition on its own goroutine. The consequence of running it inline is
// that a check which blocks forever hangs the test until go test's own
// -timeout, rather than failing with a timed-out message; that trade-off
// buys the legal Require() and is worth documenting rather than hiding.
//
// check is called once before the deadline is consulted, so a condition
// that already holds passes whatever the timeout is, and it is called
// again right at the deadline rather than being given up on one tick
// early: the loop only gives up once time.Now() is no longer before
// deadline, not merely once the next full tick would cross it.
func pollUntil(timeout, tick time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(min(tick, time.Until(deadline)))
	}
}

// Eventually polls until cond returns nil, failing with the last error.
//
// It lives in a non-test file because consumers of this package need it: the
// convergence a scenario waits on is the consumer's, not this package's.
func Eventually(t TB, timeout time.Duration, what string, cond func() error) {
	t.Helper()
	var last error
	ok := pollUntil(timeout, pollInterval, func() bool {
		last = cond()
		return last == nil
	})
	if !ok {
		t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
	}
}

// Eventually polls cond until it returns nil or timeout elapses, failing
// through the receiver's own mode: aborting on an aborting receiver, and
// reporting and continuing on a collecting one, the same as every other
// assertion on C.
func (c *C) Eventually(timeout time.Duration, what string, cond func() error) {
	c.Helper()
	var last error
	ok := pollUntil(timeout, pollInterval, func() bool {
		last = cond()
		return last == nil
	})
	if !ok {
		c.fail("timed out after %s waiting for %s: %v", timeout, what, last)
	}
}

// EventuallyTrue polls cond at the given tick until it returns true, failing
// through the receiver's own mode if timeout elapses first. This is the
// form that maps from testify's assert.Eventually and require.Eventually,
// which take an explicit tick rather than a fixed poll interval.
func (c *C) EventuallyTrue(timeout, tick time.Duration, cond func() bool, msgAndArgs ...any) {
	c.Helper()
	if !pollUntil(timeout, tick, cond) {
		c.fail("timed out after %s%s", timeout, msg(msgAndArgs))
	}
}
