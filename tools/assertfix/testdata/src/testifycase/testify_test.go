// SPDX-License-Identifier: Apache-2.0

package testifycase // want `imports need the testkit/assert path`

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type box struct{ N int }

// require aborts and assert continues, which is exactly the Require() /
// collecting split, so a file using both keeps its behaviour.
func TestModesMapOntoRequireAndCollecting(t *testing.T) { // want `2 assertion\(s\) can use testkit/assert`
	err := doThing()
	require.NoError(t, err)
	assert.True(t, err == nil)
}

// Equal becomes EqDeep, not Eq: testify compares with reflect.DeepEqual,
// which dereferences pointers where == compares addresses, and most
// comparisons in these repos are pointers.
func TestEqualBecomesEqDeep(t *testing.T) { // want `1 assertion can use testkit/assert`
	a, b := &box{1}, &box{1}
	assert.Equal(t, a, b)
}

// Contains is polymorphic upstream over strings, slices and maps, and three
// separate assertions here, so the operand's type picks which.
func TestContainsSplitsByOperandType(t *testing.T) { // want `3 assertion\(s\) can use testkit/assert`
	assert.Contains(t, "hello", "ell")
	assert.Contains(t, []int{1, 2}, 2)
	assert.Contains(t, map[string]int{"a": 1}, "a")
}

// Greater takes (actual, expected) upstream and (want, got) here, so the
// arguments swap.
func TestOrderedComparisonsSwapTheirArguments(t *testing.T) { // want `1 assertion can use testkit/assert`
	require.Greater(t, 5, 3)
}

// testify counts 0 and false as empty; ours needs something with a length,
// so anything else becomes Zero.
func TestEmptySplitsByWhetherLenApplies(t *testing.T) { // want `2 assertion\(s\) can use testkit/assert`
	assert.Empty(t, []int{})
	assert.Empty(t, 0)
}

// NotErrorIs had no negated form here until the assert vocabulary grew one,
// and the filesystem four were absent for the same reason. Both were measured
// gaps rather than speculative additions; see assert/files.go.
func TestNegatedErrorAndFilesystemAssertions(t *testing.T) { // want `5 assertion\(s\) can use testkit/assert`
	err := doThing()
	assert.NotErrorIs(t, err, errSentinel)
	assert.FileExists(t, "/etc/hosts")
	assert.NoFileExists(t, "/nonexistent")
	assert.DirExists(t, "/tmp")
	assert.NoDirExists(t, "/nonexistent")
}

var errSentinel = errors.New("sentinel")

func doThing() error { return nil }
