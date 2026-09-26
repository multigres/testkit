// SPDX-License-Identifier: Apache-2.0

// Package cmpopts is a stub of github.com/google/go-cmp/cmp/cmpopts,
// carrying only the one option the cmp.Diff-to-EqDiff tests use.
package cmpopts

import "github.com/google/go-cmp/cmp"

func EquateEmpty() cmp.Option { return nil }
