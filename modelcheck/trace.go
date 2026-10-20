// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"fmt"
	"strings"
)

// FormatViolation renders a violation as a header, the numbered steps of its
// trace, and under each step the Key lines that changed from the step before
// ("- " old, "+ " new). Step 0 is the initial state and shows its whole Key.
// A recoverable violation ends with the moves still available in the trap.
func FormatViolation[S State[S]](v Violation[S]) string {
	var b strings.Builder
	reason := "no path to goal"
	if v.Err != nil {
		reason = v.Err.Error()
	}
	fmt.Fprintf(&b, "%s %s: %s\n", v.Kind, v.Property, reason)

	var prev []string
	for i, step := range v.Trace {
		if i == 0 {
			fmt.Fprintf(&b, "  %d. initial\n", i)
		} else {
			fmt.Fprintf(&b, "  %d. %s: %s\n", i, step.Actor, step.Move)
		}
		cur := strings.Split(step.State.Key(), "\n")
		for j := 0; j < max(len(prev), len(cur)); j++ {
			hasPrev, hasCur := j < len(prev), j < len(cur)
			if hasPrev && hasCur && prev[j] == cur[j] {
				continue
			}
			if hasPrev {
				fmt.Fprintf(&b, "     - %s\n", prev[j])
			}
			if hasCur {
				fmt.Fprintf(&b, "     + %s\n", cur[j])
			}
		}
		prev = cur
	}

	if v.Kind == "recoverable" {
		b.WriteString("  exits from the trap:\n")
		for _, e := range v.Exits {
			fmt.Fprintf(&b, "    %s: %s\n", e.Actor, e.Move)
		}
	}
	return b.String()
}
