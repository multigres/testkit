// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/multigres/testkit/assert"
)

// scriptCreateConfigMap and scriptSetConfigMapKey are the whole data plane
// these tests need. Plain core objects rather than a converging cluster: the
// runner is what is under test, and a scenario that took thirty seconds to
// reconcile would only add a second thing that could have gone wrong.
func scriptCreateConfigMap(t *testing.T, ns, name string, keys ...string) error {
	t.Helper()
	data := make(map[string]string, len(keys))
	for _, key := range keys {
		data[key] = "1"
	}
	return shared.Client.Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	})
}

func scriptSetConfigMapKey(t *testing.T, ns, name, key, value string) error {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := shared.Client.Get(
		t.Context(), client.ObjectKey{Namespace: ns, Name: name}, cm,
	); err != nil {
		return err
	}
	cm.Data[key] = value
	return shared.Client.Update(t.Context(), cm)
}

// abandon closes a script that a test has deliberately left mid-failure, by
// marking it finished without asserting anything.
//
// A scenario test must never do this, and it is not part of the API. The tests
// below assert on the failure a step returns and then stop, so there is no
// clean end for Finish to assert and the backstop is right to complain about
// them. Reaching for the field here, rather than widening the backstop's quiet
// condition, is the whole point: that condition is the guarantee handed to the
// scenario tasks, and it must not be relaxed to keep this package's own tests
// green. The tests that assert the backstop through the cleanup path use
// captureInto instead and never come through here. One test,
// TestScriptLostHistoryFailsTheTestRatherThanThePin, asserts the backstop's
// message by calling unfinished directly and then abandons: the abandon
// follows the assertion and only stops a second report at cleanup, so nothing
// is voided, but the exception is worth naming rather than leaving the sentence
// above reading as absolute.
func abandon(t *testing.T, sc *Script) {
	t.Helper()
	sc.finished = true
}

// captureInto collects what the backstop reports instead of failing the test
// with it, so that a cleanup-time report can be asserted on from outside.
func captureInto(out *[]string) func(string, ...any) {
	return func(format string, args ...any) {
		*out = append(*out, fmt.Sprintf(format, args...))
	}
}

// requireContains fails when err is nil or does not mention everything a
// failure message has to name for a reader to act on it.
func requireContains(t *testing.T, err error, want ...string) {
	t.Helper()
	c := assert.NewAborting(t)

	c.Error(err, "no error, want one naming %s", strings.Join(want, ", "))
	for _, w := range want {
		c.StrContains(err.Error(), w, "error does not name %q:\n%v", w, err)
	}
}

// TestScriptStepPassesWhenItProducesExactlyWhatItPermits is the positive case
// every other test here is measured against: without it, a runner that failed
// every step would satisfy all the failure tests.
func TestScriptStepPassesWhenItProducesExactlyWhatItPermits(t *testing.T) {
	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

	sc.Step("create the config", func() error {
		return scriptCreateConfigMap(t, ns, "cfg", "a")
	}, Added("ConfigMap", "cfg"))

	sc.Step("change one key", func() error {
		return scriptSetConfigMapKey(t, ns, "cfg", "a", "2")
	}, Changed("ConfigMap", "cfg", "data.a"))

	sc.Finish(time.Second)
}

// TestScriptStepFailsOnAnExtraChange is the closed world itself. A runner that
// stopped looking once its permitted changes had all arrived would pass this
// step, and every scenario test built on it would then be asserting what
// happened rather than what was all that happened.
//
// Both positions are covered because they are two different guards. An extra
// change that lands while the step is still waiting is caught as it arrives;
// one that lands after the last permitted change is caught only because the
// step keeps listening through the settle window rather than returning the
// moment its permissions are all spent.
func TestScriptStepFailsOnAnExtraChange(t *testing.T) {
	t.Run("after the permitted changes", func(t *testing.T) {
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

		err := sc.TryStep("create the config", func() error {
			if err := scriptCreateConfigMap(t, ns, "wanted", "a"); err != nil {
				return err
			}
			return scriptCreateConfigMap(t, ns, "unwanted", "a")
		}, Added("ConfigMap", "wanted"))
		abandon(t, sc)

		requireContains(t, err, "create the config", "unexpected event", "unwanted")
	})

	t.Run("among the permitted changes", func(t *testing.T) {
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
		sc.StepTimeout = 10 * time.Second

		err := sc.TryStep("create both configs", func() error {
			for _, name := range []string{"wanted-a", "unwanted", "wanted-b"} {
				if err := scriptCreateConfigMap(t, ns, name, "a"); err != nil {
					return err
				}
			}
			return nil
		}, Added("ConfigMap", "wanted-a"), Added("ConfigMap", "wanted-b"))
		abandon(t, sc)

		requireContains(t, err, "create both configs", "unexpected event", "unwanted")
	})
}

// TestScriptStepFailsWhenAPermittedChangeNeverArrives covers the other half of
// exactness. The message has to name the permission that went unsatisfied: a
// bare timeout on a step permitting a dozen changes tells the reader nothing.
func TestScriptStepFailsWhenAPermittedChangeNeverArrives(t *testing.T) {
	c := assert.NewAborting(t)

	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
	sc.StepTimeout = 2 * time.Second

	err := sc.TryStep("create both configs", func() error {
		return scriptCreateConfigMap(t, ns, "present", "a")
	}, Added("ConfigMap", "present"), Added("ConfigMap", "absent"))
	abandon(t, sc)

	requireContains(t, err, "create both configs", "never arrived", "added ConfigMap/absent")
	c.NotStrContains(
		err.Error(),
		"ConfigMap/present",
		"the satisfied permission is reported as missing too:\n%v",
		err,
	)
}

// TestScriptQuietStepRequiresSilence runs both halves, because a Quiet() that
// failed unconditionally would satisfy the failing half on its own.
func TestScriptQuietStepRequiresSilence(t *testing.T) {
	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

	sc.Step("read something and write nothing", func() error {
		return shared.Client.List(t.Context(), &corev1.ConfigMapList{}, client.InNamespace(ns))
	}, Quiet())

	err := sc.TryStep("write something after all", func() error {
		return scriptCreateConfigMap(t, ns, "noise", "a")
	}, Quiet())
	abandon(t, sc)

	requireContains(t, err, "write something after all", "Quiet()", "noise")
}

// TestScriptStepWithNoAllowIsAProgrammingError pins the rule that silence and
// "I forgot to say" must not be the same expression. It is checked through the
// fataller seam because a real Fatalf calls runtime.Goexit.
//
// The action must not have run: a step whose declaration is malformed has no
// business mutating the cluster first, and the reader needs the complaint
// before the side effect, not after.
func TestScriptStepWithNoAllowIsAProgrammingError(t *testing.T) {
	c := assert.NewAborting(t)

	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
	rec := &fatallerRecorder{}
	sc.f = rec

	acted := false
	if err := sc.TryStep("forgot to say", func() error {
		acted = true
		return nil
	}); err != nil {
		t.Fatalf("a malformed declaration came back as an assertion failure, "+
			"which KnownDefect would read as a live defect: %v", err)
	}
	abandon(t, sc)

	c.False(acted, "the step ran its action before complaining about its declaration")
	c.Len(rec.fatalfCalls, 1, "Fatalf calls")
	for _, want := range []string{"forgot to say", "Quiet()"} {
		c.StrContains(rec.fatalfCalls[0], want, "message")
	}
}

// TestScriptAllowsTwoChangesToOneObjectInEitherOrder pins that two permissions
// for the same object assert nothing about their relative order. Five
// reconcilers run concurrently, so an accidental ordering here would make
// legitimate interleavings fail at random.
func TestScriptAllowsTwoChangesToOneObjectInEitherOrder(t *testing.T) {
	for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		t.Run(order[0]+" then "+order[1], func(t *testing.T) {
			ns := shared.Namespace(t)
			sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

			sc.Step("create the config", func() error {
				return scriptCreateConfigMap(t, ns, "cfg", "a", "b")
			}, Added("ConfigMap", "cfg"))

			sc.Step("touch both keys", func() error {
				for _, key := range order {
					if err := scriptSetConfigMapKey(t, ns, "cfg", key, "2"); err != nil {
						return err
					}
				}
				return nil
			},
				Changed("ConfigMap", "cfg", "data.a"),
				Changed("ConfigMap", "cfg", "data.b"),
			)

			sc.Finish(time.Second)
		})
	}
}

// TestScriptMatchesAmbiguousPermissionsRatherThanGreedily covers the case that
// makes the matching a search rather than a first-match loop: a broad
// permission and a narrow one that both match the first event, where spending
// the broad one on it leaves the second event with nothing to match.
//
// This is the ordinary shape of a scenario step, not a contrived one, and
// getting it wrong reports a step as failing that did exactly what it declared.
func TestScriptMatchesAmbiguousPermissionsRatherThanGreedily(t *testing.T) {
	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

	sc.Step("create the config", func() error {
		return scriptCreateConfigMap(t, ns, "cfg", "a", "b")
	}, Added("ConfigMap", "cfg"))

	sc.Step("touch the narrowly permitted key first", func() error {
		if err := scriptSetConfigMapKey(t, ns, "cfg", "b", "2"); err != nil {
			return err
		}
		return scriptSetConfigMapKey(t, ns, "cfg", "a", "2")
	},
		Changed("ConfigMap", "cfg"),
		Changed("ConfigMap", "cfg", "data.b"),
	)

	sc.Finish(time.Second)
}

// TestScriptBeforeOrdersOnlyWhereDeclared asserts the failing direction
// explicitly. An ordering combinator is the easiest thing in this file to write
// so that it passes either way, and one that never fails is worse than none:
// it reads in the test as a guarantee that is not being checked.
func TestScriptBeforeOrdersOnlyWhereDeclared(t *testing.T) {
	run := func(t *testing.T, keys [2]string) (*Script, error) {
		t.Helper()
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
		sc.StepTimeout = 10 * time.Second

		sc.Step("create the config", func() error {
			return scriptCreateConfigMap(t, ns, "cfg", "a", "b")
		}, Added("ConfigMap", "cfg"))

		return sc, sc.TryStep("touch a and then b", func() error {
			for _, key := range keys {
				if err := scriptSetConfigMapKey(t, ns, "cfg", key, "2"); err != nil {
					return err
				}
			}
			return nil
		}, Before(
			Changed("ConfigMap", "cfg", "data.a"),
			Changed("ConfigMap", "cfg", "data.b"),
		))
	}

	t.Run("declared order passes", func(t *testing.T) {
		c := assert.NewAborting(t)

		sc, err := run(t, [2]string{"a", "b"})
		c.NoError(err, "the declared order was rejected")
		sc.Finish(time.Second)
	})

	t.Run("reverse order fails", func(t *testing.T) {
		sc, err := run(t, [2]string{"b", "a"})
		abandon(t, sc)
		requireContains(t, err,
			"touch a and then b", "data.a", "before", "data.b", "arrived first")
	})
}

// TestScriptInvariantFailureNamesInvariantAndStep drives the invariant with an
// event the step in flight explicitly permits, which is the case a runner is
// most likely to get wrong: an invariant checked only against events nobody
// permitted is an invariant that never runs during a passing scenario.
func TestScriptInvariantFailureNamesInvariantAndStep(t *testing.T) {
	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{}, &corev1.SecretList{})

	sc.Invariant("no secret is ever written", func(ev Event) error {
		if ev.Kind == "Secret" {
			return fmt.Errorf("%s was written", ev.Key.Name)
		}
		return nil
	})

	sc.Step("create the config", func() error {
		return scriptCreateConfigMap(t, ns, "cfg", "a")
	}, Added("ConfigMap", "cfg"))

	err := sc.TryStep("create the secret", func() error {
		return shared.Client.Create(t.Context(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns},
			StringData: map[string]string{"password": "postgres"},
		})
	}, Added("Secret", "creds"))
	abandon(t, sc)

	requireContains(t, err,
		"no secret is ever written", "create the secret", "creds")
}

// TestScriptKnownDefectPinsAFailingStep is why TryStep exists. KnownDefect
// takes a func() error, so a step whose assertion path fataled instead of
// returning could never be pinned: Fatalf calls runtime.Goexit, and the pin
// would take the test down with it rather than record the defect.
//
// The assertion is the test passing. A fatal anywhere under TryStep fails it.
func TestScriptKnownDefectPinsAFailingStep(t *testing.T) {
	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
	sc.StepTimeout = 2 * time.Second

	KnownDefect(t, "MGO-0000", func() error {
		return sc.TryStep("create both configs", func() error {
			return scriptCreateConfigMap(t, ns, "present", "a")
		}, Added("ConfigMap", "present"), Added("ConfigMap", "absent"))
	})

	// A pinned script still has an end, and closing it is still the author's
	// job. TestScriptPinnedScriptStillMustFinish is what happens if it is left
	// out.
	sc.Finish(time.Second)
}

// TestScriptLostHistoryFailsTheTestRatherThanThePin pins the refusal to
// recover, and the refusal to let the harness's own failure stand in for the
// operator's.
//
// A relisted watch yields current state rather than the events it missed, so a
// step that waited that error out, or retried past it, would go on asserting
// over a history with a hole in it. Returning it would be the subtler mistake:
// inside a KnownDefect body a returned error reads as "the pinned defect is
// still live", so an envtest hiccup would hold a pin green, which KnownDefect's
// own doc comment names as the thing check must never do.
func TestScriptLostHistoryFailsTheTestRatherThanThePin(t *testing.T) {
	c := assert.NewAborting(t)

	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns)
	sc.StepTimeout = 30 * time.Second
	rec := &fatallerRecorder{}
	sc.f = rec

	fake := watch.NewFake()
	sc.st.addSource("ConfigMap", fake)
	fake.Stop()

	select {
	case <-sc.st.failed:
	case <-time.After(30 * time.Second):
		t.Fatal("the closed watcher never failed the stream")
	}

	started := time.Now()
	err := sc.TryStep("carry on regardless", nil, Quiet())
	c.NoError(err, "a dead stream came back as an assertion failure, which a "+
		"KnownDefect body would read as confirmation of a live defect: %v", err)
	c.Len(rec.fatalfCalls, 1, "Fatalf calls")
	for _, want := range []string{"carry on regardless", "lost history"} {
		c.StrContains(rec.fatalfCalls[0], want, "message")
	}

	// A terminal error has to be reported at once rather than waited out, or a
	// dead stream costs every remaining step its full timeout.
	c.LessOrEqual(5*time.Second, time.Since(started), "the step blocked for")

	// A dead stream does not excuse a missing Finish. The backstop says both,
	// rather than reporting the silence that draining a dead stream always
	// produces.
	if msg := sc.unfinished(); !strings.Contains(msg, "never called Finish") ||
		!strings.Contains(msg, "lost history") {
		t.Fatalf("the backstop on a dead unfinished script said %q", msg)
	}
	abandon(t, sc)
}

// TestScriptFailingActionIsNotAPinnableFailure covers the other half of the
// same contract. A do that fails is the script's own setup going wrong, not an
// observation about the operator, so it must not be capable of confirming a
// pin: a create rejected by a typo in a manifest would otherwise keep a
// KnownDefect green forever, long after the defect it names was fixed.
func TestScriptFailingActionIsNotAPinnableFailure(t *testing.T) {
	c := assert.NewAborting(t)

	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
	rec := &fatallerRecorder{}
	sc.f = rec

	err := sc.TryStep("create the config", func() error {
		return errors.New("the manifest was rejected")
	}, Added("ConfigMap", "cfg"))
	abandon(t, sc)

	c.NoError(err, "a failing action came back as an assertion failure, which a "+
		"KnownDefect body would read as confirmation of a live defect: %v", err)
	c.Len(rec.fatalfCalls, 1, "Fatalf calls")
	for _, want := range []string{"create the config", "the manifest was rejected"} {
		c.StrContains(rec.fatalfCalls[0], want, "message")
	}
}

// TestScriptFinishFailsOnAChangeAfterTheLastStep closes the end of the script.
//
// Every step but the last has a successor to catch its stragglers, which is
// what lets the settle window be short. The last step has none, so without
// Finish a change arriving after it is dropped when the stream shuts down, and
// the script's final assertion, the one every scenario test ends on, is the
// only open-world assertion in the suite.
func TestScriptFinishFailsOnAChangeAfterTheLastStep(t *testing.T) {
	c := assert.NewAborting(t)

	ns := shared.Namespace(t)
	sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

	sc.Step("create the config", func() error {
		return scriptCreateConfigMap(t, ns, "cfg", "a")
	}, Added("ConfigMap", "cfg"))

	// Created after the step has returned, so it lands beyond that step's
	// settle window: this is the late reaction that nothing else would see.
	// Named so that no wording in the failure message can contain it by
	// accident, which is the only way this assertion means anything.
	c.NoError(scriptCreateConfigMap(t, ns, "late-arrival", "a"), "create the late arrival")

	err := sc.TryFinish(5 * time.Second)
	requireContains(t, err, "after the script's last step", "late-arrival")
}

// TestScriptUnfinishedScriptIsReportedAtCleanup makes forgetting Finish loud.
// A backstop that authors can silently skip is not a backstop, and a cheap
// model writing the fourth scenario test will skip anything that costs nothing
// to skip.
//
// Driven through the reportf seam and a subtest, because the report is made
// from a t.Cleanup: a real one would fail the subtest, and the assertion here
// is that it happened, not that the suite goes red.
func TestScriptUnfinishedScriptIsReportedAtCleanup(t *testing.T) {
	c := assert.NewAborting(t)

	var forgotten, closed []string

	t.Run("a script that forgets to Finish", func(t *testing.T) {
		c := assert.NewAborting(t)

		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
		sc.reportf = captureInto(&forgotten)

		sc.Step("create the config", func() error {
			return scriptCreateConfigMap(t, ns, "cfg", "a")
		}, Added("ConfigMap", "cfg"))

		c.NoError(scriptCreateConfigMap(t, ns, "late-arrival", "a"), "create the late arrival")
	})

	c.Len(forgotten, 1, "cleanup reports")
	for _, want := range []string{"never called Finish", "late-arrival"} {
		c.StrContains(forgotten[0], want, "message")
	}

	// The other direction: a backstop that fired on every script would satisfy
	// the half above while making Finish pointless.
	t.Run("a script that calls Finish", func(t *testing.T) {
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
		sc.reportf = captureInto(&closed)

		sc.Step("create the config", func() error {
			return scriptCreateConfigMap(t, ns, "cfg", "a")
		}, Added("ConfigMap", "cfg"))

		sc.Finish(time.Second)
	})

	c.Empty(closed, "a script that called Finish was reported anyway")
}

// TestScriptPinnedScriptStillMustFinish is the case the backstop is most
// likely to be switched off in and least able to afford it.
//
// A KnownDefect body turns a failing step into a passing test on purpose, and
// pinning is what a scenario test is told to do when it finds a defect. So a
// quiet condition that treated "a step returned an error" as reason enough to
// say nothing would switch the net off in exactly the scripts that are already
// known to be walking past one defect, and the guarantee those scenario tests
// are written against, that forgetting Finish fails the test, would be false
// precisely where it is most needed.
//
// Both directions, because a backstop that fired on a correct pinned script
// would make pinning unusable and the first author to hit it would work around
// it rather than report it.
func TestScriptPinnedScriptStillMustFinish(t *testing.T) {
	c := assert.NewAborting(t)

	var forgotten, closed []string

	pinOneStep := func(t *testing.T, sc *Script, ns string) {
		t.Helper()
		sc.StepTimeout = 2 * time.Second
		KnownDefect(t, "MGO-0000", func() error {
			return sc.TryStep("create both configs", func() error {
				return scriptCreateConfigMap(t, ns, "present", "a")
			}, Added("ConfigMap", "present"), Added("ConfigMap", "absent"))
		})
	}

	t.Run("a pinned script that forgets to Finish", func(t *testing.T) {
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
		sc.reportf = captureInto(&forgotten)

		pinOneStep(t, sc, ns)
	})

	c.Len(forgotten, 1, "cleanup reports = %v, want exactly one: the pin confirmed, so the "+
		"subtest passed, and nothing else would have caught the missing Finish", forgotten)
	c.StrContains(forgotten[0], "never called Finish", "message")

	t.Run("a pinned script that calls Finish", func(t *testing.T) {
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})
		sc.reportf = captureInto(&closed)

		pinOneStep(t, sc, ns)
		sc.Finish(time.Second)
	})

	c.Empty(closed, "a pinned script that called Finish was reported anyway")
}

// TestScriptChangedDoesNotMatchCreatesOrDeletes pins the event-type vocabulary.
// Changed sits between Added and Deleted and reads as "modified"; a Changed
// that also matched creates and deletes would be a silent superset of both,
// and a step asserting that the operator updated a field would pass when the
// operator deleted the object instead, since a delete's projected diff names
// every leaf that went away.
func TestScriptChangedDoesNotMatchCreatesOrDeletes(t *testing.T) {
	t.Run("a create does not satisfy Changed", func(t *testing.T) {
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

		err := sc.TryStep("create the config", func() error {
			return scriptCreateConfigMap(t, ns, "cfg", "a")
		}, Changed("ConfigMap", "cfg", "data.a"))
		abandon(t, sc)

		requireContains(t, err, "create the config", "unexpected event", "added")
	})

	t.Run("a delete does not satisfy Changed", func(t *testing.T) {
		ns := shared.Namespace(t)
		sc := shared.NewScript(t, ns, &corev1.ConfigMapList{})

		sc.Step("create the config", func() error {
			return scriptCreateConfigMap(t, ns, "cfg", "a")
		}, Added("ConfigMap", "cfg"))

		err := sc.TryStep("update the config", func() error {
			return shared.Client.Delete(t.Context(), &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: ns},
			})
		}, Changed("ConfigMap", "cfg", "data.a"))
		abandon(t, sc)

		requireContains(t, err, "update the config", "unexpected event", "deleted")
	})
}

// TestScriptMalformedDeclarationsAreProgrammingErrors covers the declarations
// that are wrong before any event arrives. The zero-value case is the one that
// matters most: flattenAllows contributes no permission for an Allow that
// skipped the constructors, so an unrejected Allow{} is a silent Quiet(),
// which is the one thing a step must never mean by accident.
//
// Driven against validateAllows rather than through a step, because the wiring
// from a step to this function is already pinned by the no-allow test.
func TestScriptMalformedDeclarationsAreProgrammingErrors(t *testing.T) {
	const where = `script step "x"`

	cases := []struct {
		name  string
		allow []Allow
		want  string
	}{
		{
			name:  "no permission at all",
			allow: nil,
			want:  "Quiet()",
		},
		{
			name:  "a zero-value Allow",
			allow: []Allow{{}},
			want:  "zero-value Allow",
		},
		{
			name:  "Quiet combined with a permission",
			allow: []Allow{Quiet(), Added("ConfigMap", "cfg")},
			want:  "combines Quiet()",
		},
		{
			name:  "Quiet inside Before",
			allow: []Allow{Before(Quiet(), Added("ConfigMap", "cfg"))},
			want:  "Quiet() inside Before()",
		},
		{
			name:  "an empty kind",
			allow: []Allow{Changed("", "cfg")},
			want:  "empty kind or name",
		},
		{
			name:  "an empty name",
			allow: []Allow{Added("ConfigMap", "")},
			want:  "empty kind or name",
		},
		{
			name:  "an empty name nested in Before",
			allow: []Allow{Before(Added("ConfigMap", "cfg"), Deleted("ConfigMap", ""))},
			want:  "empty kind or name",
		},
		{
			name: "a well formed declaration",
			allow: []Allow{Added("ConfigMap", "cfg"), Before(
				Changed("Secret", "creds", "data.password"),
				Deleted("ConfigMap", "cfg"),
			)},
			want: "",
		},
		{
			name:  "a well formed Quiet",
			allow: []Allow{Quiet()},
			want:  "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ck := assert.NewAborting(t)

			got := validateAllows(where, c.allow)
			if c.want == "" {
				ck.Eq("", got, "a well formed declaration was rejected")
				return
			}
			ck.NotEq(
				"",
				got,
				"a malformed declaration was accepted, want a message naming %q",
				c.want,
			)
			ck.StrContains(got, c.want, "message")
			ck.StrContains(got, where, "message")
		})
	}
}

// overlappingLeaves builds the worst shape for the assignment search: n
// permissions on one object that every one of n events satisfies, so every
// permission is a candidate for every event.
func overlappingLeaves(n int) ([]leafAllow, []Event) {
	leaves := make([]leafAllow, 0, n)
	paths := make([]string, 0, n)
	for i := range n {
		leaves = append(leaves, leafAllow{
			typ: "modified", kind: "Shard", name: "s",
			changed: []string{fmt.Sprintf("status.p%d", i)},
		})
		paths = append(paths, fmt.Sprintf("status.p%d", i))
	}
	events := make([]Event, 0, n)
	for range n {
		events = append(events, Event{
			Type: "modified", Kind: "Shard",
			Key:     client.ObjectKey{Namespace: "ns", Name: "s"},
			Changed: paths,
		})
	}
	return leaves, events
}

// The assignment search used to be one exhaustive backtracking pass over
// events and orderings together, whose cost was factorial in the number of
// permissions matching the same event. Measured then: 10 permissions took
// 239ms to conclude "no fit", 11 took 2.8s and 12 took 47s. That is the
// unexpected-event path, so the runner was slowest exactly when it had
// something to report.
//
// The budget here is wall clock rather than a node count on purpose. The
// guarantee worth pinning is that the runner answers, and a future rewrite
// that is polynomial by some other route should pass this unchanged.
func TestScriptAssignmentDoesNotBlowUpOnOverlappingPermissions(t *testing.T) {
	c := assert.NewAborting(t)

	for _, n := range []int{12, 20, 40} {
		leaves, events := overlappingLeaves(n)

		// The case that used to be factorial: one event nothing permits, so
		// every assignment of the rest has to be ruled out.
		unpermitted := append(append([]Event(nil), events...), Event{
			Type: "modified", Kind: "Other",
			Key:     client.ObjectKey{Namespace: "ns", Name: "x"},
			Changed: []string{"a"},
		})

		start := time.Now()
		at, decided := assignEvents(unpermitted, leaves, nil)
		elapsed := time.Since(start)

		c.True(decided, "n=%d: an unordered assignment is always decidable", n)
		c.Nil(at, "n=%d: an event nothing permits cannot fit", n)
		c.Less(time.Second, elapsed, "n=%d: took %s to rule out a fit", n, elapsed)

		// And the case that does fit still fits, so the speed-up is not the
		// search having quietly stopped looking.
		at, decided = assignEvents(events, leaves, nil)
		c.True(decided, "n=%d", n)
		c.NotNil(at, "n=%d: every event is permitted by some leaf", n)
	}
}

// matchAll has to find an assignment wherever one exists, not merely a greedy
// one. TestScriptMatchesAmbiguousPermissionsRatherThanGreedily covers that
// end to end through a real stream; this covers the cases that are awkward to
// stage against an API server.
func TestScriptAssignmentIsExactNotGreedy(t *testing.T) {
	c := assert.NewCollecting(t)

	broad := leafAllow{typ: "modified", kind: "Shard", name: "s"}
	narrow := leafAllow{
		typ: "modified", kind: "Shard", name: "s",
		changed: []string{"status.phase"},
	}
	plain := Event{
		Type: "modified", Kind: "Shard",
		Key:     client.ObjectKey{Namespace: "ns", Name: "s"},
		Changed: []string{"spec.replicas"},
	}
	phase := Event{
		Type: "modified", Kind: "Shard",
		Key:     client.ObjectKey{Namespace: "ns", Name: "s"},
		Changed: []string{"status.phase"},
	}

	// Greedy on the first event would spend the broad permission on the phase
	// change and leave the plain one unpermitted.
	at, decided := assignEvents([]Event{phase, plain}, []leafAllow{broad, narrow}, nil)
	c.True(decided)
	c.NotNil(at, "both events fit, one permission each")

	// More permissions than events is fine; the surplus stays unassigned.
	at, decided = assignEvents([]Event{phase}, []leafAllow{broad, narrow}, nil)
	c.True(decided)
	c.NotNil(at)

	// More events than permissions can never fit.
	at, decided = assignEvents([]Event{phase, plain, phase}, []leafAllow{broad, narrow}, nil)
	c.True(decided)
	c.Nil(at)

	// No events is a vacuous fit, which is what a Quiet() step relies on.
	at, decided = assignEvents(nil, []leafAllow{broad}, nil)
	c.True(decided)
	c.NotNil(at)
}

// A step whose declared orderings the search cannot resolve within its budget
// reports that it could not decide, rather than guessing or hanging. The
// shape below is the cheapest way to exhaust the budget: every permission
// matches every event, and the orderings are mutually unsatisfiable, so no
// assignment terminates the search early.
func TestScriptUndecidableOrderingIsAHarnessFailureNotAVerdict(t *testing.T) {
	c := assert.NewAborting(t)

	leaves, events := overlappingLeaves(14)
	// A cycle: leaf 0 before leaf 1, and leaf 1 before leaf 0. Nothing
	// satisfies it, so the search explores every assignment there is.
	order := [][2]int{{0, 1}, {1, 0}}

	at, decided := assignEvents(events, leaves, order)
	c.False(decided, "the search should give up rather than enumerate 14!")
	c.Nil(at)

	// Whereas the same orderings over a step small enough to decide are
	// decided, and correctly refused.
	small, smallEvents := overlappingLeaves(3)
	at, decided = assignEvents(smallEvents, small, order)
	c.True(decided)
	c.Nil(at, "a cyclic ordering is unsatisfiable")
}
