// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/multigres/testkit/assert"
)

// fatallerRecorder implements fataller by recording calls instead of acting
// on them, so knownDefect's Fatalf/Logf glue can be checked without needing to
// survive Fatalf's call to runtime.Goexit.
type fatallerRecorder struct {
	fatalfCalls []string
	logfCalls   []string
}

func (r *fatallerRecorder) Helper() {}

func (r *fatallerRecorder) Fatalf(format string, args ...any) {
	r.fatalfCalls = append(r.fatalfCalls, fmt.Sprintf(format, args...))
}

func (r *fatallerRecorder) Logf(format string, args ...any) {
	r.logfCalls = append(r.logfCalls, fmt.Sprintf(format, args...))
}

// TestKnownDefectGlue covers the four lines knownDefectOutcome's own table
// test cannot reach: the Fatalf/Logf dispatch in knownDefect. Without this,
// inverting the fatal guard, deleting it, or swapping Fatalf for Logf all left
// the suite green, because the pure decision function was the only thing
// under test.
func TestKnownDefectGlue(t *testing.T) {
	cases := []struct {
		name          string
		ref           string
		err           error
		wantFatal     []string
		wantLog       []string
		wantCheckCall bool
	}{
		{
			name:          "error present logs and does not fatal",
			ref:           "MGO-123",
			err:           errors.New("status never reaches Ready"),
			wantLog:       []string{"MGO-123", "status never reaches Ready"},
			wantCheckCall: true,
		},
		{
			name: "error nil fatals as fixed",
			ref:  "MGO-123",
			err:  nil,
			wantFatal: []string{
				"MGO-123",
				"appears fixed",
				"replace this pin with a positive assertion",
			},
			wantCheckCall: true,
		},
		{
			name:          "empty ref fatals without running check",
			ref:           "",
			err:           errors.New("would surface if check ran"),
			wantFatal:     []string{"ref must not be empty"},
			wantCheckCall: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)

			rec := &fatallerRecorder{}
			checkCalled := false
			check := func() error {
				checkCalled = true
				return c.err
			}

			knownDefect(rec, c.ref, check)

			ck.Eq(c.wantCheckCall, checkCalled, "check called")

			if len(c.wantFatal) == 0 {
				ck.Require().Empty(rec.fatalfCalls, "Fatalf called with")
				ck.Require().
					Len(rec.logfCalls, 1, "Logf called %d times, want exactly 1", len(rec.logfCalls))
				for _, want := range c.wantLog {
					ck.StrContains(rec.logfCalls[0], want, "Logf message")
				}
				return
			}

			ck.Require().Empty(rec.logfCalls, "Logf called with")
			ck.Require().
				Len(rec.fatalfCalls, 1, "Fatalf called %d times, want exactly 1", len(rec.fatalfCalls))
			for _, want := range c.wantFatal {
				ck.StrContains(rec.fatalfCalls[0], want, "Fatalf message")
			}
		})
	}
}

func TestKnownDefectOutcome(t *testing.T) {
	cases := []struct {
		name      string
		ref       string
		err       error
		wantLog   []string
		wantFatal []string
	}{
		{
			name:    "error present logs and does not fail",
			ref:     "MGO-123",
			err:     errors.New("status never reaches Ready"),
			wantLog: []string{"MGO-123", "status never reaches Ready"},
		},
		{
			name: "error nil fails as fixed",
			ref:  "MGO-123",
			err:  nil,
			wantFatal: []string{
				"MGO-123",
				"appears fixed",
				"replace this pin with a positive assertion",
			},
		},
		{
			name:      "empty ref fails regardless of check",
			ref:       "",
			err:       errors.New("status never reaches Ready"),
			wantFatal: []string{"ref must not be empty"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)

			log, fatal := knownDefectOutcome(c.ref, c.err)

			if len(c.wantFatal) == 0 {
				ck.Require().Eq("", fatal, "fatal")
				for _, want := range c.wantLog {
					ck.StrContains(log, want, "log")
				}
				return
			}

			ck.Require().
				NotEq("", fatal, "fatal is empty, want non-empty containing %v", c.wantFatal)
			for _, want := range c.wantFatal {
				ck.StrContains(fatal, want, "fatal")
			}
			// A stray log alongside a fatal is invisible today, since Fatalf
			// exits before KnownDefect's own Logf runs, but this pins the
			// decision function's own contract in case that glue changes.
			ck.Eq("", log, "log")
		})
	}
}

// TestKnownDefectOutcomeEmptyRefFailsEvenWithoutAnError pins the guard by name:
// deleting it falls through to the "appears fixed" branch, which is also
// non-empty, so asserting only fatal != "" would not catch the guard's
// removal.
func TestKnownDefectOutcomeEmptyRefFailsEvenWithoutAnError(t *testing.T) {
	c := assert.NewAborting(t)

	_, fatal := knownDefectOutcome("", nil)
	c.StrContains(fatal, "ref must not be empty", "fatal")
}

// TestKnownDefectDelegates covers the exported wrapper's own body, which the
// fataller indirection narrowed to one statement but did not reach. Emptying
// that statement turns every pin in the repository into a silent no-op, and
// nothing else in this file notices: the glue tests all drive knownDefect
// directly.
//
// A pin whose defect is live passes, so this is the one case that can be
// called with the real *testing.T, and whether check ran is the one effect a
// passing test can observe from outside.
func TestKnownDefectDelegates(t *testing.T) {
	c := assert.NewAborting(t)

	called := false
	KnownDefect(t, "MGO-DELEGATION", func() error {
		called = true
		return errors.New("defect still present")
	})
	c.True(
		called,
		"KnownDefect never ran check, so it is not delegating to knownDefect and every pin is a silent no-op",
	)
}
