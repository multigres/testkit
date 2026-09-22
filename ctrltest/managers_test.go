// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/multigres/testkit/assert"
)

// TestManagerLookupNamesWhatItKnows pins the panic message rather than only
// the panic. A miss here is a typo in a test's own source, and the fix is
// obvious only if the message says what the alternatives were.
func TestManagerLookupNamesWhatItKnows(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)

	defer func() {
		r := recover()
		c.Require().NotNil(r, "expected a panic for an unregistered manager name")
		msg, ok := r.(string)
		c.Require().True(ok, "panicked with %T, want a string", r)
		for _, want := range []string{"nosuch", "primary", "secondary"} {
			c.StrContains(msg, want, "panic message")
		}
	}()
	shared.Manager("nosuch")
}

// TestManagersAreOrderedAndDistinct pins the two properties Options promises
// about a slice of managers: declaration order is start order, and each gets
// a manager of its own rather than a shared one under two names.
func TestManagersAreOrderedAndDistinct(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)

	got := shared.Managers()
	c.Require().Len(got, 2, "want 2 managers, got %d", len(got))
	if got[0].Name != "primary" || got[1].Name != "secondary" {
		t.Errorf("want [primary secondary] in declaration order, got [%s %s]",
			got[0].Name, got[1].Name)
	}
	c.NotEq(got[1].Mgr, got[0].Mgr, "both entries share one manager")
	c.NotEq(got[1].Client, got[0].Client, "both entries share one client")

	// Mutating the returned slice must not reach the suite.
	got[0] = nil
	c.NotNil(shared.Managers()[0], "Managers returned the suite's own slice")
}

// TestViaReadsThroughTheNamedManager is the multi-operator case in miniature:
// a write through one manager's client is visible to a case bound to the
// other, on the same namespace and the same T.
//
// Both managers hold the harness scheme here, so this proves the plumbing
// rather than the scheme split. The scheme split is a consumer's to create
// and cannot be reproduced without two conflicting CRD types.
func TestViaReadsThroughTheNamedManager(t *testing.T) {
	c := shared.Case(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "via-probe", Namespace: c.NS},
		Data:       map[string]string{"k": "v"},
	}
	c.NoError(c.Via("primary").Create(cm), "create through primary")

	var got corev1.ConfigMap
	key := client.ObjectKey{Namespace: c.NS, Name: "via-probe"}
	c.NoError(c.Via("secondary").Get(key, &got), "read back through secondary")
	c.Eq("v", got.Data["k"])

	// The harness client sees it too: Via moves the object verbs and nothing
	// else.
	var direct corev1.ConfigMap
	c.NoError(c.Get(key, &direct), "read back through the harness client")
	c.Eq("v", direct.Data["k"])
}

// TestViaKeepsTheCaseIntact guards the shadow. Via returns *C by value copy,
// so a bug there would silently hand back a case pointing at the wrong
// namespace, and every assertion after it would measure someone else's data.
func TestViaKeepsTheCaseIntact(t *testing.T) {
	ck := assert.NewCollecting(t)

	c := shared.Case(t)
	v := c.Via("secondary")

	ck.Eq(c.NS, v.NS, "Via changed the namespace")
	ck.Eq(c.Suite, v.Suite, "Via changed the suite")
	ck.Nil(c.mgr, "Via mutated its receiver rather than copying")
}

// TestBootRejectsMalformedManagerSets covers the three ways a consumer can
// declare managers that cannot work, all of which are silent if Boot accepts
// them: no managers at all gates every reconcile and passes vacuously, and a
// missing or duplicate name makes Suite.Manager ambiguous.
//
// These run against a real Boot rather than an extracted validator, because
// the thing worth pinning is that Boot refuses before starting envtest. Each
// case returning an error rather than a live suite is that proof: a boot that
// got as far as the control plane would take a hundred times longer.
func TestBootRejectsMalformedManagerSets(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)

	scheme := runtime.NewScheme()
	c.Require().NoError(clientgoscheme.AddToScheme(scheme), "scheme")
	noop := func(context.Context, manager.Manager, *Suite) error { return nil }

	for name, tc := range map[string]struct {
		managers []ManagerOptions
		want     string
	}{
		"none": {
			managers: nil,
			want:     "no managers",
		},
		"unnamed": {
			managers: []ManagerOptions{{Register: noop}},
			want:     "no Name",
		},
		"duplicate": {
			managers: []ManagerOptions{{Name: "a"}, {Name: "a"}},
			want:     `two managers named "a"`,
		},
	} {
		start := time.Now()
		s, teardown, err := Boot(Options{Scheme: scheme, Managers: tc.managers})
		if err == nil {
			_ = teardown()
			t.Errorf("%s: Boot accepted %d managers", name, len(tc.managers))
			continue
		}
		if s != nil || teardown != nil {
			t.Errorf("%s: Boot returned a suite alongside its error", name)
		}
		c.StrContains(err.Error(), tc.want, "%s: error %q does not mention", name, err)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("%s: refusal took %s, so envtest started before validation", name, elapsed)
		}
	}
}
