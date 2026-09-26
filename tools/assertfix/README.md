# assertfix

Rewrites stdlib and testify test assertions into `testkit/assert` calls.

```
go install github.com/multigres/testkit/tools/assertfix@latest
go fix -fixtool=$(go env GOPATH)/bin/assertfix ./...
gofmt -w . && go mod tidy
```

It is a `go/analysis` fixtool, so `go fix` drives it with full type
information and applies its edits as byte ranges rather than reprinting
files. Comments survive untouched, which matters in a suite where they carry
the reasoning.

## What it converts

`if cond { t.Errorf(...) }` and the `Fatalf`/`Error`/`Fatal` variants, where
the body is that single call and the condition maps to an assertion with
certainty. Nil and error checks, equality, lengths and emptiness, string and
slice containment, `errors.Is`, ordered comparisons, `slices.Equal` and
`reflect.DeepEqual`.

Measured on a 3,604-assertion stdlib suite: **2,966 converted, 82%**, for a
net 3,956 fewer lines.

`if` with an init statement converts too, either by inlining the init or by
hoisting it to its own statement immediately before the assertion; see
"What it refuses" for which and why.

`if diff := cmp.Diff(a, b[, opts...]); diff != "" { ... }`, and the same
without the intervening variable, map onto `EqDiff` or `EqDiffOpts` rather
than a bool comparison against `""`, so the diff prints under its own
`(-want +got)` header instead of as a quoted value. A message already
carrying that header has it stripped, since printing it twice would be
noise; a label naming an unrecognised pair of roles is left as `Eq("",
cmp.Diff(...), ...)`, which still converts (that mapping predates this one)
but does not get the newer header treatment. An already-converted `Eq("",
cmp.Diff(...))` call from an earlier `assertfix` release upgrades to the
newer mapping the same way, so a re-run improves code this tool already
touched.

`if (err != nil) != wantErr { ... }`, and the equivalent forms with `==` or
the operands swapped, map onto `ErrorWhen(wantErr, err, ...)` rather than a
bare boolean `Eq`, so the failure message can say which direction went
wrong.

## testify

`testify/assert` and `testify/require` calls convert too, one call to one
call. `require` aborts and `assert` continues, which is exactly the
`Require()` / collecting split, so a file using both keeps its behaviour.

Conversion is **all-or-nothing per file**, because both packages want the
identifier `assert` and only one can have it. A file holding a single call
this tool declines keeps testify and is left entirely alone, including its
stdlib assertions. `testify/suite` files are skipped: that is a different
testing model, not a vocabulary.

`NotErrorIs` and the `FileExists`/`NoFileExists`/`DirExists`/`NoDirExists`
family convert too, since `testkit/assert` gained them. So does `Eventually`,
onto `EventuallyTrue`, which takes the explicit tick testify's does and fails
through the receiver's own mode rather than always aborting.

`assert.ObjectsAreEqual`, a bool helper rather than an assertion, rewrites in
place to `reflect.DeepEqual`: the two are the same claim for every operand
type, including `[]byte`, since testify's own `ObjectsAreEqual` checks
`exp == nil || act == nil` before its `bytes.Equal` special case, exactly
`reflect.DeepEqual`'s own nil-versus-non-nil-empty answer (confirmed against
testify v1.4.0 through v1.12.1). A call nested inside an `if` statement's own
init or condition, or inside another call's argument list, is left to
whichever of those a different conversion in this tool rewrites wholesale:
that conversion's own rendering already substitutes the nested call, so a
second, independent edit for the same bytes would conflict with the first
rather than compose with it.

`Equal` becomes `EqDeep`, not `Eq`. testify compares with `reflect.DeepEqual`,
which dereferences pointers where `==` compares addresses:

```
pointers:  == false   DeepEqual true
```

Most comparisons in these repos are pointers, so mapping onto `Eq` would
invert them. `ElementsMatch` splits the same way, by element type: ours hashes
elements and so needs `comparable`, which gives a `[]*Foo` address identity,
and only a pointer-free element type means the same thing under both.

Polymorphic upstream names split by operand type: `Contains` is `StrContains`,
`Contains` or `HasKey`; `Empty` is `Empty` where there is a length and `Zero`
otherwise, since testify counts `0` and `false` as empty and this package
refuses them.

Left alone deliberately: `Never` (no equivalent, and approximating one is how
a test quietly stops meaning what it said), `EqualValues` (type-coercing
equality, which the target package declines), `ErrorAs` (testify takes an
out-parameter, ours returns the match, so a rewrite would have to invent the
variable), `IsType` and `Implements` (compile-time concerns once generics
exist), and the regexp, JSON, YAML, subset and duration families.

`ASSERTFIX_DEBUG=1` prints every declined call and the file it held back,
which is the only way to find out why a file was skipped.

## Tests

```
make test-tools    # from the repo root
```

`analysistest` against `testdata/src`, with testify stubbed there rather than
depended on: the tool that exists to remove an assertion library should not
carry one. `testdata/src/stdlibcase` and `testdata/src/testifycase` have
`.golden` files holding the expected rewrite; `testdata/src/refusals` has
neither golden nor `want` comments, so any diagnostic at all fails it.

A new conversion needs a case. **So does a new refusal**, and that is the half
that matters: the value of this tool is in what it declines, and a refusal
with no test is one refactor away from becoming a silent wrong rewrite.

## What it refuses, and why that is the point

Anything it cannot map with certainty is left exactly as it was. A site left
alone costs three lines; a site converted wrongly costs a test, silently,
because a weakened assertion still passes. So multi-statement failure bodies
and calls not directly inside an `if` are skipped rather than guessed at.

**A compound condition converts whole, when its message is provably safe to
evaluate unconditionally**, as `c.False(a || b, msg)`. The condition itself
carries no new risk: the test failed when it held, so asserting it is false
says the same thing with one evaluation, and passing it through unchanged
preserves whatever `&&`/`||` short-circuiting it already had. The message is
the actual risk, since it moves from evaluating only inside the failing
branch to evaluating on every run, and a guard the condition provided there
(`p != nil && p.Name != ""`, `len(s) == 1 && s[0] != x`, and shapes far less
regular than those two) is easy to miss by trying to recognize its shape.
So this does not try: a message argument converts only when it is safe
regardless of the condition, by construction, meaning a literal, a plain
identifier, or a selector whose every step is a non-pointer, non-interface
value. Anything else, and the whole site is left alone rather than guessed
at.

**`if err := f(); err != nil { ... }` is handled**, and so is a multi-value
init: `if got, want := a, b; got != want`, the comma-ok idiom
(`if _, ok := m[k]; !ok`) and an init returning an error alongside a
discarded value (`if _, err := f(); err == nil`). A single declared name used
nowhere the message can't drop or keep cleanly is inlined, exactly as before;
everything else, including every multi-value init, is hoisted instead: the
init statement moves to its own line immediately before the assertion, which
evaluates it once either way and needs no substitution since the names stay
real.

Hoisting checks with the type checker rather than by scanning text: no name
the init declares already exists in any scope enclosing the `if` (a collision
or a shadow), none is declared or referenced anywhere later in the same
block, nested blocks included (widening its scope so it reaches something it
does not reach today), and the failure message does not dereference a name
the init declares (a selector, index or star), since that name can be the
zero value on the passing path, which the message would then evaluate
unconditionally once hoisted. It also declines outright in a function body
containing a `goto`, rather than risk jumping over the hoisted declaration,
and unless the `if` starts its own line, since the insertion point assumes
gofmt'd input. The later-reference check is deliberately coarser than it
needs to be, and one consequence is worth knowing: of several sequential
`if err := f(); err != nil { ... }` sites sharing a block, only the last can
ever hoist, because every earlier one always finds a later same-named init
and declines. A **compound condition combined with an init**
(`if exists, err := f(); err != nil || exists`) is declined outright rather
than hoisted and then declined on the condition anyway: compound conditions
are out of scope for this shape, full stop.

Four refusals come from the type checker and are the reason this is not a
regex:

- **`Eq` is generic over `comparable`.** Rewriting a slice comparison to
  `Eq` does not compile, so the type decides between `Eq` and a diffing
  assertion.
- **`Eq` and `EqDiff` each infer one type parameter.** `io.ReadCloser`
  against `http.NoBody` compiles as `!=` and not as an assertion, so
  mismatched types are skipped.
- **go-cmp panics on unexported fields where `reflect.DeepEqual` reads
  them.** A mechanical substitution there replaces an assertion with a
  panic, so the type picks `EqDiff` or `EqDeep`.
- **`==` on an interface is identity, not value equality.** `comparable` as
  a constraint refuses interfaces where `==` accepts them, so the operands
  that fall through are exactly the ones where a deep comparison would be
  asking a different question. Those keep the expression whole, as
  `c.False(a != b)`.

## What it preserves

**Abort semantics, exactly.** `t.Errorf` continues and `t.Fatalf` aborts, so a
scope with both gets `assert.NewCollecting` and the aborting sites are written
`c.Require().X(...)`. No test changes behaviour.

**The receiver name.** `c` where it is free, otherwise the first unused name,
because plenty of tests already call something `c`. A scope with exactly one
assertion gets no receiver at all, just
`assert.NewAborting(t).NoError(err)`: naming a variable to use it once costs
two lines, and table tests put one assertion in each `t.Run`.

**The receiver's place in the scope.** Inserted after a leading run of
`t.Helper()`, `t.Parallel()` and any `t.Skip*()` call, rather than at the very
top. A leading unconditional `t.Skip` makes everything after it unreachable,
which is a real shape (a test kept for a future PR to re-enable): declaring
the receiver above it left staticcheck's SA4006 reporting it as never used.

**The message, minus what the assertion now prints.** `"Server = %q, want %q"`
with the compared values becomes `"Server"`; a message carrying prose the
values do not supply is passed through whole.
