# testkit

Go testing packages for the Multigres projects.

Three packages, one module:

- **`assert`** is a typed assertion vocabulary for ordinary unit tests. No
  Kubernetes, no controller-runtime; stdlib and `go-cmp` only.
- **`golden`** compares a value's YAML marshalling against a checked-in file,
  for tests that would otherwise assert field-by-field against a literal
  struct. Rewrite the files with `-golden.update`. Also a leaf: stdlib,
  `go-cmp` and `sigs.k8s.io/yaml`, and notably not `testing`.
- **`ctrltest`** is a multi-controller test harness built on `envtest`: write
  attribution per controller, reconcile interception, a closed-world step
  script, quiescence detection and a core-kind data plane simulator.

They ship together because `ctrltest` hangs its own vocabulary on the same
receiver `assert` provides, so a test says `c.Eq(...)` and `c.Get(key, obj)`
against one object. Importing `assert` alone is supported and costs you none of
the harness.

## Versioning

`v0`, and expect it to stay there for a while. Under SemVer that means any
release may break you, which is an honest description rather than a
disclaimer: this is a young harness whose shape is still being decided by the
suites using it.

```
go get github.com/multigres/testkit@latest
go install github.com/multigres/testkit/tools/assertfix@latest
```

`tools/assertfix` is a separate module and carries its own tags, prefixed with
its directory (`tools/assertfix/v0.1.0`). A root `v0.1.0` says nothing about
it, which is the usual surprise with nested modules.

Code `assertfix` converts using `ErrorWhen`, `EqDiffOpts` or `EventuallyTrue`
needs `testkit` at whichever release first adds them (`v0.2.0` and later),
not just any `v0`: pin the root module to that version alongside the tool.

## Requirements

**Go 1.27.** `assert`'s comparisons are generic *methods*, which the language
gained in 1.27, so the floor is not optional: setting a lower `go` directive
and running `go mod tidy` rewrites it back.

What the generics buy is a compile-time check on the comparison itself.

```go
c.Eq(1, int64(1))   // does not compile
```

`ctrltest` additionally requires **controller-runtime v0.25.1 or newer**,
stated here rather than left to arrive as a transitive surprise inside
somebody's dependency bump.

Running `ctrltest`'s own tests needs the envtest binaries. `make setup-envtest`
fetches them into `./bin`; `make test` does it for you.

## What `assert` covers

Equality, errors, nil, length and emptiness, containment over slices and
strings, ordered comparisons, zero values, pointer identity, unordered slice
equality, panics, file and directory existence, and polling with `Eventually`
or, on a receiver whose collecting-versus-aborting mode should decide how a
timeout fails, `EventuallyTrue`. The set was chosen by counting what the test
suites in these projects actually call, so it is sized to real use rather than
to completeness. That is also the bar for adding to it: show the call sites.

Three additions came out of running `assertfix` over `multigres-operator`,
each closing a decline the tool found rather than one guessed at:

- **`ErrorWhen(wantErr bool, err error, ...)`** for the
  `if (err != nil) != wantErr` shape a table test writes when one row wants
  an error and the next does not (26 sites). A bare `Eq(wantErr, err != nil)`
  compiles and asserts the same thing, but its failure message cannot say
  which direction went wrong; this one can.
- **`EqDiffOpts`** is `EqDiff` with a `[]cmp.Option`, for a comparison that
  needs to ignore a field or supply its own comparator (13 sites). Taken as
  its own slice parameter rather than folded into `EqDiff`'s variadic
  `msgAndArgs`, since a `cmp.Option` satisfies `any` exactly as well as a
  format string does.
- **`EventuallyTrue(timeout, tick time.Duration, cond func() bool, ...)`**
  maps from testify's `assert.Eventually`/`require.Eventually` (18 sites),
  which take an explicit tick and a bool predicate where `Eventually` takes a
  fixed poll interval and a `func() error`. Unlike `Eventually`, it fails
  through the receiver's own mode: collecting continues, aborting aborts.
  `Eventually` itself is unchanged, including that it always aborts even on
  a collecting receiver.

Two omissions are deliberate rather than pending:

- **Type-coercing equality.** An assertion that accepts `1` against
  `int64(1)` gives up the compile-time check that the generic signatures
  exist for. Convert explicitly at the call site instead.
- **`IsType` and `Implements`.** Generics turned these into compile-time
  concerns, so there is nothing left for a runtime assertion to add.

Assertions abort by default, which matches C's `assert` and Go's own
`t.Fatal`. Neither constructor is spelled `New`: the two modes are used about
equally across these projects, so making either the unmarked default would
mean every reader of a call site had to remember which it was.

```go
c := assert.NewAborting(t)     // first failure ends the test
c := assert.NewCollecting(t)   // failures accumulate
c.Require().NoError(err)       // flip one call to aborting
c.Check().Eq(want, got)        // flip one call to collecting
```

Every positive assertion has its negative, which is why `NotErrorIs` and
`NotEqDiff` exist alongside `ErrorIs` and `EqDiff`. `NotEq` is not the
negation of a deep comparison: two distinct pointers to identical structs
pass it and fail `NotEqDiff`.

`Nil` and `NotNil` take `any` rather than a type parameter, because Go cannot
express "nilable" as a constraint. They look through to the dynamic value, so
an interface holding a nil pointer reads as nil here, where `!= nil` reports
it as non-nil and everything downstream then dereferences it. `Len`, `Empty`
and `NotEmpty` take `any` for the same reason.

## Using `ctrltest`: the one prerequisite

**Every reconciler under test needs a substitution seam in its production
source.** `ctrltest` wraps the reconcile boundary to attribute writes, gate by
namespace and compress requeues, which means the controller must accept a
reconciler other than its receiver:

```go
func (r *T) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerReconciler(mgr, r)
}

func (r *T) SetupWithManagerReconciler(mgr ctrl.Manager, reconciler reconcile.Reconciler) error {
	// ... builder configuration ...
	return b.Complete(reconciler)
}
```

This cannot be avoided by a cleverer harness. The receiver has to stay live on
the enqueue path, because predicates and map functions bind to it when the
builder runs, so the same pointer must be both the receiver and whatever the
substitute delegates to. Wrapping from outside would mean reimplementing the
builder configuration in the test, where it would drift from production.

It is about fifteen lines per reconciler. A controller that cannot take the
seam still gets write attribution and the data plane simulator, but not gating,
requeue compression or reconcile records.

## More than one operator

`Options.Managers` takes one entry per operator, and the reason to need two is
usually not scale. Two operators that both know a CRD might register different
Go types for it, if the consuming one wants a few status fields rather than a
dependency on the whole module, and `runtime.Scheme` refuses a second Go type
for a kind it already holds. Where that happens they cannot share a manager,
which is also how they run in production.

```go
suite, teardown, err := ctrltest.Boot(ctrltest.Options{
	Scheme:       harnessScheme,
	CRDPaths:     []string{aCRDs, bCRDs},
	WatchedKinds: []client.ObjectList{ /* ... */ },
	Managers: []ctrltest.ManagerOptions{
		{Name: "a", Scheme: aScheme, CacheOptions: aCache, Register: registerA},
		{Name: "b", Scheme: bScheme, CacheOptions: bCache, Register: registerB},
	},
})
```

The recorder and the interceptor stay suite-wide, which is what makes running
operators together useful: the op log is one interleaved timeline, quiescence
means every operator has settled, and an assertion can name what one operator
did in response to another. Both key attribution by the controller name passed to
`Register`, so name controllers per operator or two of them will be credited
with each other's writes.

Reads are the one thing that cannot be shared, since a Go type in one manager's
scheme is not resolvable by another's client. A case reads through the harness
scheme by default and crosses over with `c.Via("b")`.

## Development

```
make help        # every target
make check       # lint and test this module, the pre-push gate
make check-all   # plus the tools module, which is everything CI runs bar race
make test        # envtest-backed, -p 1
make test-race
```

`tools/assertfix` is a separate Go module, deliberately: it needs
`golang.org/x/tools`, and nothing importing `testkit/assert` should inherit an
analysis framework. The consequence is that the root `./...` does not reach
it, so it has its own targets (`make check-tools`) and its own CI job.

golangci-lint is pinned at 2.13.2 or newer, and that floor is load bearing:
2.12.2 panics on Linux under Go 1.27 before it lints anything. See the comment
on the `lint` target.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Commits are Conventional Commits and
must be signed off (`git commit -s`); the DCO check is required.

## License

Apache 2.0. Every source file carries `// SPDX-License-Identifier: Apache-2.0`,
and `make license-check` fails if one does not.
