// SPDX-License-Identifier: Apache-2.0

package testifycase // want `imports need the testkit/assert path`

import (
	"errors"
	"testing"
	"time"

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

// testify's Eventually takes the condition first and an explicit tick;
// EventuallyTrue takes the timeout and tick first, the condition last, so
// the call is rebuilt rather than just reordered in place. require aborts
// on a timeout the same way it does everywhere else here, and assert
// continues.
func TestEventuallyMapsWithTickAndReceiverMode(t *testing.T) { // want `2 assertion\(s\) can use testkit/assert`
	assert.Eventually(t, condTrue, time.Second, 100*time.Millisecond, "should converge")
	require.Eventually(t, condTrue, time.Second, 200*time.Millisecond)
}

func condTrue() bool { return true }

// ObjectsAreEqual is always safe to become reflect.DeepEqual, verified
// against testify's own source (see the assertfix README): there is no
// operand type this needs to refuse.
func TestObjectsAreEqualBecomesReflectDeepEqual(t *testing.T) { // want `1 assertion can use testkit/assert`
	want := []string{"a", "b"}
	got := []string{"a", "c"}
	matched := assert.ObjectsAreEqual(want, got) // want `assert.ObjectsAreEqual can become reflect.DeepEqual`
	if !matched {
		t.Errorf("mismatch: want %v, got %v", want, got)
	}
}

// A call nested inside another testify call's arguments converts by
// embedding: assert.True itself becomes a site, and rendering its own
// argument substitutes the nested ObjectsAreEqual call as part of that
// rewrite, rather than as a second, independent edit that would overlap it.
func TestObjectsAreEqualEmbedsInsideAnotherTestifyCall(t *testing.T) { // want `1 assertion can use testkit/assert`
	want := []string{"a", "b"}
	got := []string{"a", "b"}
	assert.True(t, assert.ObjectsAreEqual(want, got))
}

// The same embedding reaches inside a closure passed to Eventually, since
// EventuallyTrue's own conversion renders that closure's body too.
func TestObjectsAreEqualEmbedsInsideAnEventuallyClosure(t *testing.T) { // want `1 assertion can use testkit/assert`
	want := []string{"a", "b"}
	got := []string{"a", "b"}
	require.Eventually(t, func() bool {
		return assert.ObjectsAreEqual(want, got)
	}, time.Second, 10*time.Millisecond)
}

var errSentinel = errors.New("sentinel")

func doThing() error { return nil }
