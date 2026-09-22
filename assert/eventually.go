// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"time"
)

// pollInterval is how long Eventually waits between attempts.
const pollInterval = 250 * time.Millisecond

// Eventually polls until cond returns nil, failing with the last error.
//
// It lives in a non-test file because consumers of this package need it: the
// convergence a scenario waits on is the consumer's, not this package's.
//
// cond is called once before the deadline is consulted, so a condition that
// already holds passes whatever the timeout is. Checking the clock first
// instead made Eventually(t, 0, ...) fail without ever evaluating cond, and
// report the failure as "timed out ...: <nil>", naming an error nothing had
// produced. It also means a timeout shorter than the poll interval is one
// attempt rather than none.
func Eventually(t TB, timeout time.Duration, what string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		last := cond()
		if last == nil {
			return
		}
		if !time.Now().Add(pollInterval).Before(deadline) {
			t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
			// Reached only under a TB whose Fatalf returns, which a real
			// *testing.T never does; without it such a TB polls forever.
			return
		}
		time.Sleep(pollInterval)
	}
}

// Eventually polls cond until it returns nil or timeout elapses.
func (c *C) Eventually(timeout time.Duration, what string, cond func() error) {
	c.Helper()
	Eventually(c.TB, timeout, what, cond)
}
