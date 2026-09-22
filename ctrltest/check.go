// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/multigres/testkit/assert"
)

// TB is the slice of *testing.T this package needs.
//
// An alias rather than a second declaration, so that a consumer can hand the
// same value to this package and to assert without thinking about which one
// owns the interface. See assert.TB for why it is an interface at all.
type TB = assert.TB

// C is one test's handle on the suite: the T and the assertions from
// assert.C, plus the namespace it owns and the harness behind it.
//
// Assertions are fatal by default; c.Check() returns a collecting view. See
// assert.C for the rest of that contract, including why this is unsafe from a
// goroutine other than the one that owns its T.
type C struct {
	*assert.C

	Suite *Suite
	NS    string

	// mgr is the manager whose client this case reads through, or nil for
	// the harness client. Set by Via.
	mgr *Manager
}

// client is what every read and write below goes through: this case's
// manager if Via bound one, otherwise the harness client.
//
// The distinction only matters once a suite runs more than one operator,
// because then there is more than one scheme, and a Go type registered in one
// is not resolvable by the other's client. A single-operator suite never
// takes the first branch.
func (c *C) client() client.Client {
	if c.mgr != nil {
		return c.mgr.Client
	}
	return c.Suite.Client
}

// Via returns this case reading through the named manager's client, keeping
// the T and the namespace.
//
// For the assertion that spans two operators: one creates an object through
// its own types, the other observes it through its own, and both halves want
// to be the same test on the same namespace with one failure dump. Without
// this a test would need a second client threaded by hand, which is what it
// does today and what buries the actual claim.
//
// Only the object reads and writes move. Watch, NewScript and quiescence stay
// on the harness scheme, deliberately: they measure churn and convergence,
// which are properties of the API server rather than of anyone's view of it.
func (c *C) Via(name string) *C {
	c.Helper()
	c.requireNS("Via")
	out := *c
	out.mgr = c.Suite.Manager(name)
	return &out
}

// Case opens a test context on its own namespace, with a failure dump
// attached.
//
// The dump is the reason to funnel every test through here. The recorder can
// render the op log interleaved with reconcile records, which is the only
// readable account of what the controllers actually did, and before this
// existed no test opted into it: DumpOnFailure, CursorT and CursorForT had
// zero callers outside this package's own tests. Registering it once per test
// costs nothing on a passing run and turns a bare assertion message into
// evidence on a failing one.
func (s *Suite) Case(t TB) *C {
	t.Helper()
	ns := s.Namespace(t)
	s.Ops.CursorT(t, ns)
	return &C{C: assert.NewAborting(t), Suite: s, NS: ns}
}

// Check returns a C whose assertions report and continue rather than abort.
//
// The returned value shares the namespace and harness, so
// c.Check().Eq(...) is a per-call decision rather than a mode the whole test
// has to adopt.
//
// This shadows assert.C.Check so that the name always hands back this
// package's C, with the harness methods still on it. Without the shadow a
// collecting assertion would silently drop to the embedded type and lose
// every method below.
func (c *C) Check() *C {
	out := *c
	out.C = c.C.Check()
	return &out
}

// Require returns a C whose assertions abort rather than report and continue.
//
// The inverse of Check, and shadowed here for the same reason: without it
// c.Require() hands back the embedded assert.C and every harness method below
// disappears, which is a compile error at the call site rather than a silent
// weakening, but a baffling one.
func (c *C) Require() *C {
	out := *c
	out.C = c.C.Require()
	return &out
}

// Bare is a C with no namespace and no suite: the assertions, and nothing
// else.
//
// For the tests in a suite package that are pure logic and never touch the
// cluster. Case allocates a real namespace against envtest and registers it
// at the reconcile gate, which is the right cost for a test that drives
// controllers and pure waste for one that calls a function and compares the
// answer. multigres-operator's identity tests are nineteen of the latter.
//
// Every namespace-scoped method fails loudly on one of these rather than
// quietly operating on the empty namespace, which would otherwise be a test
// watching a namespace nothing ever writes to and passing for the wrong
// reason.
func Bare(t TB) *C {
	return &C{C: assert.NewAborting(t)}
}

// requireNS stops a namespace-scoped method on a Bare case.
//
// The panic is not redundant with the Fatalf. Fatalf aborts by calling
// runtime.Goexit, which only a real *testing.T does; TB is an interface
// precisely so this package can be tested against a recorder that returns
// instead. Without the panic, every caller here would carry on and
// dereference a nil Suite, and the resulting nil-pointer trace points at this
// file rather than at the test that misused the API. Under a real T the
// Goexit happens first and the panic is never reached.
func (c *C) requireNS(method string) {
	c.Helper()
	if c.Suite == nil || c.NS == "" {
		msg := fmt.Sprintf("c.%s needs a namespace: this case came from Bare, "+
			"which has none. Use Case if the test drives controllers.", method)
		c.Fatalf("%s", msg)
		panic(msg)
	}
}

// Sub is this case bound to a subtest's T, keeping the namespace.
//
// The distinction that matters: Case allocates a fresh namespace, which is
// right for an independent test and wrong for a t.Run subtest that asserts
// about objects the parent created. Calling Case inside such a subtest
// produces a case pointing at an empty namespace, and the subtest then either
// reads the parent's namespace through a closure (working, but saying
// something it does not mean) or reads its own and quietly observes nothing.
//
// Shadows assert.C.Sub for the reason Check does, and adds the two things the
// embedded type cannot know about: the namespace guard, and registering the
// failure dump against the subtest, so a failing subtest prints the op log
// for the namespace it actually asserted on.
func (c *C) Sub(t TB) *C {
	c.Helper()
	c.requireNS("Sub")
	out := *c
	out.C = c.C.Sub(t)
	c.Suite.Ops.CursorT(t, c.NS)
	return &out
}

// selfTyped fails to compile if a method that should hand back this package's
// C starts handing back the embedded assert.C instead. That regression is
// otherwise silent: the call still compiles at every existing site, and only
// the harness methods quietly disappear.
//
// Every assert.C method returning a *assert.C must appear here, which is
// Check, Require and Sub. Require was missing from both this list and from
// the type for exactly one release, and c.Require().Get(...) did not compile
// in any consumer that tried it.
var _ = func(c *C) (*C, *C, *C, *C) {
	return c.Check(), c.Require(), c.Sub(nil), c.Via("")
}

// The harness entry points, re-exposed on C.
//
// Each is the free function or Suite method of the same name with the
// arguments C already knows removed: the T, and for the namespace-scoped ones
// the namespace. That is the whole value. Before this existed 112 call sites
// passed t as a leading argument and 100 passed a namespace alongside it,
// none of which is what any of those tests are about.
//
// Deliberately thin. A delegation that grew logic of its own would mean the
// same operation behaved differently depending on which spelling a test
// reached for, so anything worth doing belongs in the underlying function.

// KnownDefect pins a live operator defect. See the free function: the
// inverted polarity there is the whole point, and it surprises everyone once.
func (c *C) KnownDefect(ref string, check func() error) {
	c.Helper()
	KnownDefect(c.TB, ref, check)
}

// RequireQuiescent fails unless this case's namespace stops changing.
func (c *C) RequireQuiescent(quiet, timeout time.Duration) {
	c.Helper()
	c.requireNS("RequireQuiescent")
	c.Suite.RequireQuiescent(c.TB, c.NS, quiet, timeout)
}

// TryQuiescent is RequireQuiescent as a value, for use inside a KnownDefect
// body, where failing the test is exactly the wrong response.
func (c *C) TryQuiescent(quiet, timeout time.Duration) error {
	c.Helper()
	c.requireNS("TryQuiescent")
	return c.Suite.TryQuiescent(c.TB, c.NS, quiet, timeout)
}

// Watch opens an event stream over this case's namespace.
func (c *C) Watch(kinds ...client.ObjectList) *Stream {
	c.Helper()
	c.requireNS("Watch")
	return c.Suite.Watch(c.TB, c.NS, kinds...)
}

// NewScript opens a closed-world step script over this case's namespace.
func (c *C) NewScript(kinds ...client.ObjectList) *Script {
	c.Helper()
	c.requireNS("NewScript")
	return c.Suite.NewScript(c.TB, c.NS, kinds...)
}

// Get reads key into obj. It returns the error rather than failing.
//
// Returning is what lets one name serve both places these are used. Inside an
// Eventually body the error must propagate so the poll retries; outside one
// the caller wraps it, c.NoError(c.Get(key, obj), "get shard"). The
// alternative, a failing Get plus an error-returning TryGet, offers a choice
// whose wrong answer is silent: a failing variant inside a poll turns the
// first transient not-found into a hard failure, and the test that used to
// wait now does not.
func (c *C) Get(key client.ObjectKey, obj client.Object) error {
	c.Helper()
	c.requireNS("Get")
	return c.client().Get(c.Context(), key, obj)
}

// List reads every object of the list's kind in this case's namespace.
//
// The namespace is not optional and not a parameter, which is the point.
// envtest never actually deletes a namespace, so an unscoped List in this
// package sees every object every earlier test in the run created, and the
// resulting assertion passes or fails on somebody else's data. Every List
// here already scopes correctly; this removes the chance to forget.
//
// Extra options still compose, for label selectors and the like. Reach for
// c.Client() directly on the rare occasion a test genuinely means to look
// outside its own namespace, where saying so explicitly is right.
func (c *C) List(list client.ObjectList, opts ...client.ListOption) error {
	c.Helper()
	c.requireNS("List")
	return c.client().List(
		c.Context(),
		list,
		append([]client.ListOption{client.InNamespace(c.NS)}, opts...)...)
}

// Create, Update, Patch and Delete write through the manager's client and
// return the error, matching Get and List.
//
// Unlike List these remove no hazard: the namespace rides on the object, and
// there is nothing to forget. They exist so that the set is not half
// converted, because a reader who has seen c.Get will reach for c.Create and
// should find it rather than learn that this particular verb is spelled
// differently.
//
// All four use the case's context, which a real *testing.T cancels at test
// end. A cleanup that must outlive the test, deleting a cluster-scoped
// object the namespace teardown cannot reach, needs c.Client() and its own
// context; saying that explicitly is right, because it is a real exception.
func (c *C) Create(obj client.Object, opts ...client.CreateOption) error {
	c.Helper()
	c.requireNS("Create")
	return c.client().Create(c.Context(), obj, opts...)
}

func (c *C) Update(obj client.Object, opts ...client.UpdateOption) error {
	c.Helper()
	c.requireNS("Update")
	return c.client().Update(c.Context(), obj, opts...)
}

func (c *C) Patch(obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.Helper()
	c.requireNS("Patch")
	return c.client().Patch(c.Context(), obj, patch, opts...)
}

func (c *C) Delete(obj client.Object, opts ...client.DeleteOption) error {
	c.Helper()
	c.requireNS("Delete")
	return c.client().Delete(c.Context(), obj, opts...)
}

// Client is the manager's client, the one every controller under test writes
// through, and so the one whose writes the recorder attributes.
func (c *C) Client() client.Client {
	c.Helper()
	c.requireNS("Client")
	return c.client()
}

// Cursor is a fresh read position in the op log for this case's namespace,
// scoped to controller if one is named.
//
// Variadic rather than two methods because the one-controller form is the
// common case and CursorForT's extra argument reads as noise at the call site.
func (c *C) Cursor(controller ...string) *Cursor {
	c.Helper()
	c.requireNS("Cursor")
	if len(controller) > 0 {
		return c.Suite.Ops.CursorForT(c.TB, c.NS, controller[0])
	}
	return c.Suite.Ops.CursorT(c.TB, c.NS)
}
