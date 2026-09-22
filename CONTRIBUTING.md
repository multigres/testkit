# Contributing to testkit

## Development environment

### Prerequisites

- **Go 1.27+.** Not negotiable: `assert`'s comparisons are generic *methods*,
  which the language gained in 1.27. Setting a lower `go` directive and running
  `go mod tidy` rewrites it back.
- `make`.

Nothing else. `make test` fetches the envtest binaries (an API server, an etcd
and a kubectl) into `./bin` on first run.

### Setup

```bash
git clone https://github.com/multigres/testkit.git
cd testkit

make help     # every target
make check    # lint and test, the pre-push gate
```

`make check` is what CI runs, minus the race job. Run `make test-race` too
before a change to `ctrltest`, which is where the concurrency lives.

### The two modules

`tools/assertfix` is a separate Go module, so `go test ./...` at the root does
not reach it. `make check-tools` lints, vets and tests it; `make check-all`
runs both. CI runs both.

## What a change needs

### Tests

Everything in `ctrltest` that can be tested against a fake is. `assert` and
`golden` are tested through a recorder that implements their `TB`, because
`Fatalf` calls `runtime.Goexit` and a fake that had to survive that call is a
trap this repo refuses; see `assert.TB`'s doc comment.

`assertfix` is tested with `golang.org/x/tools/go/analysis/analysistest`
against `testdata/src`. A new conversion needs a case there, and so does a new
*refusal*: the tool's value is in what it declines, and a refusal with no test
is a refusal one refactor away from becoming a silent wrong rewrite.

### Comments

This repo's comments explain *why*, at length, and that is deliberate rather
than an accident of one author. A harness whose guarantees are subtle is only
as good as a reader's ability to tell a load-bearing line from an incidental
one, and several comments here say explicitly "do not simplify this away"
because someone will otherwise, correctly-looking, break a guarantee.

If you remove a comment, say in the commit message what made it untrue.

### Measurements

Where a comment states a number ("a pod settles in three most of the time and
four often enough to matter"), that number came from a measurement. Adding one
means running the measurement. Changing behaviour that invalidates one means
re-running it or deleting the claim, not leaving it to age.

### Scope, for `assert`

The vocabulary is sized to measured use, not to completeness, and that is the
bar for adding to it: show the call sites. Two categories are closed rather
than pending, and the reasons are in `assert`'s package doc: type-coercing
equality, and `IsType`/`Implements`.

## Commits and pull requests

- **Conventional Commits**: `type(scope): lowercase imperative subject`, where
  scope is the package (`assert`, `ctrltest`, `golden`, `assertfix`, `ci`,
  `deps`).
- **Sign off every commit**: `git commit -s`. The DCO check is required and a
  missing sign-off blocks the merge. The signer must be the author.
- Branches are `type/description`, matching the Conventional Commits types.
- Open pull requests as drafts and promote them when they are ready for review.

## License

By contributing you agree that your contributions are licensed under the
Apache License 2.0. Every source file carries
`// SPDX-License-Identifier: Apache-2.0`; new files need it too.
