// SPDX-License-Identifier: Apache-2.0

package assert

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// tinyTick keeps these tests fast: the poll loop still runs several times,
// but each iteration costs microseconds rather than testkit's default
// 250ms pollInterval.
const tinyTick = time.Millisecond

func TestEventuallyTruePassesAsSoonAsCondHolds(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	NewAborting(r).EventuallyTrue(time.Second, tinyTick, func() bool { return true })
	if r.failed {
		t.Errorf("a condition true on the first try should not fail: %v", r.fatals)
	}
}

// TestEventuallyTrueHonoursTheReceiversMode pins that this, like every other
// assertion on C, fails through c.fail rather than a hardcoded Fatalf.
func TestEventuallyTrueHonoursTheReceiversMode(t *testing.T) {
	t.Parallel()

	aborting := &recorder{}
	NewAborting(aborting).EventuallyTrue(10*tinyTick, tinyTick, func() bool { return false })
	if len(aborting.fatals) != 1 || len(aborting.errors) != 0 {
		t.Errorf("an aborting receiver should fail via Fatalf: fatals=%v errors=%v",
			aborting.fatals, aborting.errors)
	}

	collecting := &recorder{}
	NewCollecting(collecting).EventuallyTrue(10*tinyTick, tinyTick, func() bool { return false })
	if len(collecting.errors) != 1 || len(collecting.fatals) != 0 {
		t.Errorf("a collecting receiver should fail via Errorf and continue: fatals=%v errors=%v",
			collecting.fatals, collecting.errors)
	}
}

// TestEventuallyTrueCallsCondBeforeCheckingTheDeadline pins the same
// guarantee Eventually makes: a zero timeout still gets one evaluation, so a
// condition that already holds passes regardless of how short the timeout is.
func TestEventuallyTrueCallsCondBeforeCheckingTheDeadline(t *testing.T) {
	t.Parallel()

	calls := 0
	r := &recorder{}
	NewAborting(r).EventuallyTrue(0, tinyTick, func() bool {
		calls++
		return true
	})
	if r.failed {
		t.Errorf("a condition true on the first call should pass even at timeout=0: %v", r.fatals)
	}
	if calls != 1 {
		t.Errorf("want exactly one call to cond, got %d", calls)
	}
}

func TestEventuallyTruePollsUntilItHolds(t *testing.T) {
	t.Parallel()

	tries := 0
	r := &recorder{}
	NewAborting(r).EventuallyTrue(time.Second, tinyTick, func() bool {
		tries++
		return tries >= 3
	})
	if r.failed {
		t.Errorf("a condition that eventually holds should not fail: %v", r.fatals)
	}
	if tries < 3 {
		t.Errorf("want at least 3 tries, got %d", tries)
	}
}

func TestEventuallyTrueMessageNamesTheTimeout(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	c := NewAborting(r)
	c.EventuallyTrue(
		10*tinyTick, tinyTick, func() bool { return false }, "waiting for %s", "convergence",
	)
	if len(r.fatals) != 1 {
		t.Fatalf("want one fatal, got %v", r.fatals)
	}
	if !strings.Contains(r.fatals[0], "timed out after") ||
		!strings.Contains(r.fatals[0], "waiting for convergence") {
		t.Errorf("message should name the timeout and carry the caller's message: %q", r.fatals[0])
	}
}

// TestEventuallyTrueChecksRightAtTheDeadline pins the fix for giving up a
// tick early: the old loop gave up as soon as now+tick would cross the
// deadline, which meant a condition that only became true partway through
// the final tick was never seen. The new loop keeps sleeping only up to the
// deadline itself and always checks once more there.
func TestEventuallyTrueChecksRightAtTheDeadline(t *testing.T) {
	t.Parallel()

	tick := 20 * time.Millisecond
	timeout := 5 * tick
	start := time.Now()
	// Satisfied partway through the final tick: the old "now+tick >=
	// deadline" pre-check would have given up one whole tick before this
	// point was ever reached.
	threshold := 4*tick + tick/2

	r := &recorder{}
	NewAborting(r).EventuallyTrue(timeout, tick, func() bool {
		return time.Since(start) >= threshold
	})
	if r.failed {
		t.Errorf("a condition satisfied inside the final tick should still pass: %v", r.fatals)
	}
}

// TestCEventuallyHonoursTheReceiversMode pins the same fix as
// TestEventuallyTrueHonoursTheReceiversMode for the error-returning form:
// (*C).Eventually used to always call Fatalf directly, aborting even a
// collecting receiver, which none of C's other assertions do.
func TestCEventuallyHonoursTheReceiversMode(t *testing.T) {
	t.Parallel()

	errNotYet := errors.New("not yet")

	aborting := &recorder{}
	NewAborting(aborting).Eventually(10*tinyTick, "convergence", func() error { return errNotYet })
	if len(aborting.fatals) != 1 || len(aborting.errors) != 0 {
		t.Errorf("an aborting receiver should fail via Fatalf: fatals=%v errors=%v",
			aborting.fatals, aborting.errors)
	}

	collecting := &recorder{}
	NewCollecting(
		collecting,
	).Eventually(10*tinyTick, "convergence", func() error { return errNotYet })
	if len(collecting.errors) != 1 || len(collecting.fatals) != 0 {
		t.Errorf("a collecting receiver should fail via Errorf and continue: fatals=%v errors=%v",
			collecting.fatals, collecting.errors)
	}
}

// TestCEventuallyChecksRightAtTheDeadline is TestEventuallyTrueChecksRightAtTheDeadline
// for (*C).Eventually, which shares the same poll loop.
func TestCEventuallyChecksRightAtTheDeadline(t *testing.T) {
	t.Parallel()

	// (*C).Eventually polls at the fixed pollInterval, not a caller-chosen
	// tick, so the timeout has to be sized in units of that instead.
	timeout := 5 * pollInterval
	start := time.Now()
	threshold := 4*pollInterval + pollInterval/2

	r := &recorder{}
	NewAborting(r).Eventually(timeout, "convergence", func() error {
		if time.Since(start) >= threshold {
			return nil
		}
		return errors.New("not yet")
	})
	if r.failed {
		t.Errorf("a condition satisfied inside the final tick should still pass: %v", r.fatals)
	}
}

// TestPackageLevelEventuallyAlwaysAborts pins that the free function, unlike
// the two methods above, has no receiver mode to honour and always calls
// Fatalf: it takes a bare TB, which carries no such mode.
func TestPackageLevelEventuallyAlwaysAborts(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	Eventually(r, 10*tinyTick, "convergence", func() error { return errors.New("not yet") })
	if len(r.fatals) != 1 || len(r.errors) != 0 {
		t.Errorf(
			"want exactly one Fatalf and no Errorf, got fatals=%v errors=%v",
			r.fatals,
			r.errors,
		)
	}
}

// eventuallySubprocessEnv, when set, tells the three test functions below
// to actually run instead of skipping: they exist to fail, on purpose, so
// that TestEventuallyFailuresReportTheCallersLine can inspect where go test
// says the failure happened. The recorder every other test in this file
// uses cannot answer that question: its own Helper is a no-op, so it never
// skips a frame either way, and would report the same line whether
// pollUntil's old callback indirection was there or not.
const eventuallySubprocessEnv = "TESTKIT_ASSERT_EVENTUALLY_SUBPROCESS"

func TestEventuallyFailureReportsTheCallersLine_PackageLevel(t *testing.T) {
	if os.Getenv(eventuallySubprocessEnv) == "" {
		t.Skip("only runs as a subprocess of TestEventuallyFailuresReportTheCallersLine")
	}
	Eventually(t, tinyTick, "convergence", func() error { return errors.New("not yet") })
}

func TestEventuallyFailureReportsTheCallersLine_CEventually(t *testing.T) {
	if os.Getenv(eventuallySubprocessEnv) == "" {
		t.Skip("only runs as a subprocess of TestEventuallyFailuresReportTheCallersLine")
	}
	NewAborting(
		t,
	).Eventually(tinyTick, "convergence", func() error { return errors.New("not yet") })
}

func TestEventuallyFailureReportsTheCallersLine_EventuallyTrue(t *testing.T) {
	if os.Getenv(eventuallySubprocessEnv) == "" {
		t.Skip("only runs as a subprocess of TestEventuallyFailuresReportTheCallersLine")
	}
	NewAborting(t).EventuallyTrue(tinyTick, tinyTick, func() bool { return false })
}

// TestEventuallyFailuresReportTheCallersLine pins the fix for M2 (round 2):
// pollUntil used to call a closure written inside eventually.go to report
// the failure, and neither pollUntil's nor that closure's frame is marked a
// helper, so testing reported the closure's own line in this package
// instead of the caller's. Each of the three functions above fails on
// purpose, in a subprocess (a real go test binary, since only the real
// testing.T computes and prints a file:line at all), and this checks the
// reported location names this file, not eventually.go.
func TestEventuallyFailuresReportTheCallersLine(t *testing.T) {
	for _, name := range []string{
		"TestEventuallyFailureReportsTheCallersLine_PackageLevel",
		"TestEventuallyFailureReportsTheCallersLine_CEventually",
		"TestEventuallyFailureReportsTheCallersLine_EventuallyTrue",
	} {
		t.Run(name, func(t *testing.T) {
			//nolint:gosec // os.Args[0] is this test binary itself, and name is one of the three literals above
			cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v")
			cmd.Env = append(os.Environ(), eventuallySubprocessEnv+"=1")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("subprocess unexpectedly passed:\n%s", out)
			}
			if strings.Contains(string(out), "eventually.go:") {
				t.Errorf("failure reported inside eventually.go, not the caller:\n%s", out)
			}
			if !strings.Contains(string(out), "eventually_test.go:") {
				t.Errorf("want failure reported in eventually_test.go, got:\n%s", out)
			}
		})
	}
}
