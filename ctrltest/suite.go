// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Options is everything Boot needs from the consumer. Nothing here has a
// harness-supplied default that could silently diverge from the operator the
// consumer is actually testing: the schemes, the CRDs, the cache config and
// the client rate limits are all the consumer's, because a harness that
// guessed any of them would make every test a claim about the guess.
type Options struct {
	// Scheme is the harness's own: the namespaces it creates, the event
	// stream Watch builds, the kinds quiescence resolves, the data plane
	// simulator, and the client a Case reads through unless it says
	// otherwise. It is not automatically a manager's scheme, though a
	// single-operator consumer will usually let its one manager default to
	// it.
	//
	// Boot adds nothing to it, so a kind missing here is a kind neither
	// Watch nor quiescence can resolve.
	Scheme *runtime.Scheme

	// CRDPaths are directories of CRD manifests installed into envtest.
	// Empty is legal and means a consumer testing over built-in kinds only.
	// With several operators under test this is the union of theirs.
	CRDPaths []string

	// WatchedKinds is every kind RequireQuiescent watches for churn. A kind
	// missing here is a kind whose churn quiescence cannot see.
	WatchedKinds []client.ObjectList

	// SimInterval is how often the data plane simulator sweeps. It runs for
	// the life of the suite across every namespace and every manager.
	// Non-positive means no simulator at all, for a consumer that models no
	// data plane.
	SimInterval time.Duration

	// Managers is one entry per operator under test, started in order.
	//
	// A slice rather than a map so that start order is the order written
	// here. Deterministic sequencing is worth more in a harness than the
	// lookup sugar a map would give, and Suite.Manager covers lookup.
	//
	// Several entries is not a scaling knob, it is the only thing that
	// works once two operators disagree about a GroupVersionKind:
	// runtime.Scheme refuses a second Go type for a kind it already knows,
	// so two operators with different views of the same CRD cannot share
	// one. Which is also what production runs.
	Managers []ManagerOptions
}

// ManagerOptions is one operator under test: its manager, its scheme, and the
// reconcilers it registers.
type ManagerOptions struct {
	// Name identifies this manager to Suite.Manager and C.Via. It is also
	// what a reader sees when a test crosses from one operator to another,
	// so prefer the operator's name over a position.
	Name string

	// Scheme is this manager's view of the world. Nil means Options.Scheme,
	// which is the single-operator case and the reason that spelling stays
	// short.
	//
	// Two managers may hold different Go types for the same
	// GroupVersionKind, which is the whole point: an operator that consumes
	// another's CRD may register a minimal view of it rather than taking on
	// a dependency on the whole module.
	Scheme *runtime.Scheme

	// CacheOptions is this manager's cache config, which should be the same
	// one its production main uses. A default cache makes every cached read
	// behave differently from production and voids the premise that these
	// are the real controllers wired as in main.
	CacheOptions cache.Options

	// OperatorNamespace stands in for the namespace this operator deploys
	// into. A production cache usually treats it specially (unfiltered), so
	// the suite has to have one for that config to mean anything. Boot
	// creates it, and creating the same one twice across managers is not an
	// error. Empty means this operator's cache does not single one out.
	OperatorNamespace string

	// QPS and Burst are applied to this manager's rest config, and should
	// match what its own main sets.
	QPS   float32
	Burst int

	// Register wires this operator's reconcilers onto mgr. It runs with the
	// suite fully built and no manager yet started, which is the only window
	// in which both s.Ops.For and s.Reconciles.Wrap can be used and the
	// controllers can still be added.
	//
	// The recorder and the interceptor are shared across every manager, and
	// both key by the controller name passed here. So name controllers per
	// operator, "mgo/shard" rather than "shard", or two operators with a
	// similarly-named controller will attribute writes to each other.
	Register func(ctx context.Context, mgr manager.Manager, s *Suite) error
}

// Manager is one registered operator: its controller-runtime manager, the
// scheme it sees the world through, and a direct client on that scheme.
type Manager struct {
	Name   string
	Mgr    manager.Manager
	Scheme *runtime.Scheme

	// Client reads and writes directly on this manager's scheme, bypassing
	// its cache. Use it for a kind the harness scheme does not hold, which
	// is what C.Via hands back.
	Client client.Client

	stopped chan error
}

// Suite is one envtest apiserver, one or more managers, and whatever
// reconcilers the consumer registered, shared by every test in the consumer's
// package. Tests isolate themselves with Namespace, not by booting their own.
//
// The recorder and the interceptor are suite-wide rather than per-manager, so
// with several operators under test the op log is one interleaved timeline
// and quiescence means every operator has settled. That is the reason to run
// them together at all: the interesting failures are in the protocol between
// two operators, and neither one's own suite can see it.
type Suite struct {
	Cfg *rest.Config

	// Scheme and Client are the harness's own, on Options.Scheme. They are
	// what Namespace, Watch, quiescence and the data plane simulator use,
	// and what a Case reads through until it says otherwise.
	//
	// Client bypasses every manager cache. Tests want this: a cache is
	// filtered exactly as in production, so a cached read from a test would
	// lie about anything the filter excludes. That same reasoning is why
	// Watch builds its event stream on this client rather than on an
	// informer, which is what the WithWatch is for.
	Scheme *runtime.Scheme
	Client client.WithWatch

	// Ops attributes every write to the controller that issued it, across
	// every manager.
	Ops *Recorder

	// Reconciles sits at every wrapped controller's reconcile boundary: it
	// gates by namespace, compresses requeues, and records each pass. It
	// answers what Ops cannot, namely that a controller ran and decided to do
	// nothing.
	Reconciles *Interceptor

	env *envtest.Environment

	// watchedKinds is every kind RequireQuiescent watches for churn. Set once
	// in Boot from Options.
	watchedKinds []client.ObjectList

	managers []*Manager
	byName   map[string]*Manager

	// simDone closes when the data plane simulator's goroutine has returned.
	// Nil when no simulator was started.
	simDone chan struct{}

	cancel context.CancelFunc
}

// Manager returns the registered manager of that name.
//
// It panics rather than returning an error, and rather than taking a TB so it
// could Fatalf: the name is a constant written next to the Options that
// declared it, so a miss is a typo in the test's own source rather than
// anything a run could produce. Naming the known managers in the message is
// what makes that typo a two-second fix.
func (s *Suite) Manager(name string) *Manager {
	m, ok := s.byName[name]
	if !ok {
		known := make([]string, 0, len(s.managers))
		for _, m := range s.managers {
			known = append(known, m.Name)
		}
		panic(fmt.Sprintf("ctrltest: no manager named %q; registered: %v", name, known))
	}
	return m
}

// Managers returns every registered manager, in the order Options declared
// them.
func (s *Suite) Managers() []*Manager {
	out := make([]*Manager, len(s.managers))
	copy(out, s.managers)
	return out
}

// Boot brings up envtest and every manager. The returned function tears them
// all down and must run before any goroutine leak check, since a manager owns
// goroutines that only exit once its context is cancelled.
func Boot(opts Options) (*Suite, func() error, error) {
	log.SetLogger(zap.New(zap.UseDevMode(false), zap.WriteTo(os.Stderr)))

	if len(opts.Managers) == 0 {
		return nil, nil, fmt.Errorf("no managers: a suite with nothing " +
			"registered would gate every reconcile and pass vacuously")
	}
	seen := make(map[string]bool, len(opts.Managers))
	for _, m := range opts.Managers {
		if m.Name == "" {
			return nil, nil, fmt.Errorf("manager with no Name: " +
				"Suite.Manager and C.Via address managers by name")
		}
		if seen[m.Name] {
			return nil, nil, fmt.Errorf("two managers named %q", m.Name)
		}
		seen[m.Name] = true
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: opts.CRDPaths,
		// Only meaningful when there are paths to miss. A consumer testing
		// over built-in kinds passes none and still boots.
		ErrorIfCRDPathMissing:    len(opts.CRDPaths) > 0,
		ControlPlaneStartTimeout: 120 * time.Second,
		ControlPlaneStopTimeout:  60 * time.Second,
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("envtest start: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	ops := NewRecorder()
	s := &Suite{
		Cfg:          cfg,
		Scheme:       opts.Scheme,
		Ops:          ops,
		Reconciles:   NewInterceptor(ops, RequeueClamp),
		env:          env,
		watchedKinds: opts.WatchedKinds,
		byName:       make(map[string]*Manager, len(opts.Managers)),
		cancel:       cancel,
	}

	// started is how many managers have had Start called on them, so the
	// failure path knows how many stop signals to wait for. A manager is
	// talking to the API server until Start returns, and env.Stop would
	// otherwise pull etcd out from under one, turning a legible boot error
	// into a pile of connection failures from a goroutine nobody is reading.
	started := 0
	fail := func(err error) (*Suite, func() error, error) {
		cancel()
		until := time.Now().Add(managerStopTimeout)
		for _, m := range s.managers[:started] {
			select {
			case <-m.stopped:
			case <-time.After(time.Until(until)):
			}
		}
		_ = env.Stop()
		return nil, nil, err
	}

	// Built before any Register runs, so the Suite a consumer is handed is
	// whole: a Register that wants to read or seed the API server can, and
	// none of its wiring has to be deferred to first use.
	s.Client, err = client.NewWithWatch(cfg, client.Options{Scheme: opts.Scheme})
	if err != nil {
		return fail(fmt.Errorf("direct client: %w", err))
	}

	for _, mo := range opts.Managers {
		scheme := mo.Scheme
		if scheme == nil {
			scheme = opts.Scheme
		}

		// Per manager rather than once on the shared cfg: QPS and Burst are
		// this operator's production settings, and two operators need not
		// agree. Copying is what keeps one from overwriting the other.
		mcfg := rest.CopyConfig(cfg)
		mcfg.QPS = mo.QPS
		mcfg.Burst = mo.Burst

		mgr, err := ctrl.NewManager(mcfg, ctrl.Options{
			Scheme:         scheme,
			LeaderElection: false,
			// Off rather than on a port: several managers in one process
			// would otherwise contend for the same listen address, which
			// fails the second one with an error about the first.
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   mo.CacheOptions,
		})
		if err != nil {
			return fail(fmt.Errorf("new manager %q: %w", mo.Name, err))
		}

		cl, err := client.New(mcfg, client.Options{Scheme: scheme})
		if err != nil {
			return fail(fmt.Errorf("direct client for %q: %w", mo.Name, err))
		}

		m := &Manager{
			Name:    mo.Name,
			Mgr:     mgr,
			Scheme:  scheme,
			Client:  cl,
			stopped: make(chan error, 1),
		}
		s.managers = append(s.managers, m)
		s.byName[mo.Name] = m
	}

	// Registration is a separate pass over the managers, after all of them
	// exist. A Register that reaches across to another operator, to seed an
	// object its own controller will then see, would otherwise depend on
	// declaration order.
	for i, mo := range opts.Managers {
		if mo.Register == nil {
			continue
		}
		if err := mo.Register(ctx, s.managers[i].Mgr, s); err != nil {
			return fail(fmt.Errorf("register %q: %w", mo.Name, err))
		}
	}

	for _, m := range s.managers {
		go func() { m.stopped <- m.Mgr.Start(ctx) }()
		started++
	}
	for _, m := range s.managers {
		if !m.Mgr.GetCache().WaitForCacheSync(ctx) {
			return fail(fmt.Errorf("cache sync failed for manager %q", m.Name))
		}
	}

	// Deduplicated: two operators deploying into the same namespace is
	// ordinary, and the second create would otherwise fail the boot.
	created := make(map[string]bool)
	for _, mo := range opts.Managers {
		if mo.OperatorNamespace == "" || created[mo.OperatorNamespace] {
			continue
		}
		created[mo.OperatorNamespace] = true
		if err := s.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: mo.OperatorNamespace},
		}); err != nil {
			return fail(fmt.Errorf("create operator namespace %q: %w", mo.OperatorNamespace, err))
		}
	}

	// The data plane fake runs for the life of the suite, across every
	// namespace, because the managers it feeds are also suite-wide. A
	// non-positive interval means no data plane to model, and starting it
	// anyway would panic in NewTicker from a goroutine with no t to blame.
	//
	// Joined at teardown like the managers. Cancelling its context and not
	// waiting left a goroutine that was usually gone by the time a leak check
	// looked, which is the shape of an intermittent CI failure rather than of
	// a clean shutdown.
	if opts.SimInterval > 0 {
		s.simDone = make(chan struct{})
		go func() {
			defer close(s.simDone)
			NewDataPlaneSim(s.Client, opts.SimInterval).Run(ctx)
		}()
	}

	return s, s.teardown, nil
}

// managerStopTimeout bounds how long teardown waits for everything it
// cancelled, in total rather than per manager.
//
// Per manager was wrong in the direction that matters: the managers stop
// concurrently, since one cancel goes to all of them, so a per-manager budget
// multiplies a single hang by the number of operators under test. A suite
// with four managers could spend four minutes discovering one wedged
// goroutine.
const managerStopTimeout = 60 * time.Second

// teardown stops every manager and the data plane simulator, then envtest, in
// that order, and waits for all of them. A clean sequence leaks no goroutines;
// skipping the wait is what would make a leak check report the whole manager.
//
// One cancel stops them all, because they share a context. A manager that
// fails to stop does not excuse the rest: every channel is drained before
// env.Stop, or envtest would be torn down underneath something still talking
// to it.
func (s *Suite) teardown() error {
	s.cancel()

	// An instant rather than a timer, because a timer fires once: the second
	// waiter to reach an expired time.Timer would block on it forever, which
	// would turn a bounded teardown into a hang. time.After on an elapsed
	// deadline fires immediately, so every later waiter gives up at once.
	until := time.Now().Add(managerStopTimeout)

	var errs []error
	for _, m := range s.managers {
		select {
		case err := <-m.stopped:
			if err != nil {
				errs = append(errs, fmt.Errorf("manager %q stopped with error: %w", m.Name, err))
			}
		case <-time.After(time.Until(until)):
			errs = append(errs, fmt.Errorf(
				"manager %q did not stop within %s of cancellation", m.Name, managerStopTimeout))
		}
	}
	if s.simDone != nil {
		select {
		case <-s.simDone:
		case <-time.After(time.Until(until)):
			errs = append(errs, fmt.Errorf(
				"the data plane simulator did not stop within %s of cancellation",
				managerStopTimeout))
		}
	}
	if err := s.env.Stop(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

var nsSeq struct {
	sync.Mutex
	n int
}

// Namespace creates a namespace for one test, admits it at the reconcile
// boundary, and reverses both afterwards.
//
// Admission hangs off this helper rather than being a separate call a test
// makes, because the gate defaults to admitting nothing and a test that forgot
// to register would watch a manager quietly ignore everything it created. The
// corollary is that a namespace conjured up without this helper is invisible
// to every wrapped controller.
//
// Note it does not wait for deletion to finish: envtest runs no namespace
// controller, so a terminating namespace never actually goes away. Which is
// the other reason the gate closes here: the objects outlive the test, and
// under requeue compression an ungated finished test would keep every
// reconciler busy on them for the rest of the package.
func (s *Suite) Namespace(t TB) string {
	t.Helper()
	nsSeq.Lock()
	nsSeq.n++
	name := fmt.Sprintf("t%d-%d", time.Now().UnixNano()%1e6, nsSeq.n)
	nsSeq.Unlock()

	s.Reconciles.Activate(name)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := s.Client.Create(t.Context(), ns); err != nil {
		s.Reconciles.Deactivate(name)
		t.Fatalf("create namespace %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = s.Client.Delete(context.Background(), ns)
	})
	// Registered last so it runs first: cleanups are LIFO, and closing the
	// gate before the delete keeps teardown quiet rather than kicking off a
	// round of deletion reconciles the test will never observe.
	t.Cleanup(func() {
		s.Reconciles.Deactivate(name)
	})
	return name
}
