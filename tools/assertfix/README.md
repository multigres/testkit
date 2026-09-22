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
family convert too, since `testkit/assert` gained them.

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

Left alone deliberately: `Eventually` and `Never` (no equivalent, and
approximating one is how a test quietly stops meaning what it said),
`EqualValues` (type-coercing equality, which the target package declines),
`ErrorAs` (testify takes an out-parameter, ours returns the match, so a
rewrite would have to invent the variable), `IsType` and `Implements`
(compile-time concerns once generics exist), and the regexp, JSON, YAML,
subset and duration families.

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

**A compound condition converts whole**, as `c.False(a || b, msg)`. That is
exact: the test failed when the condition held, so asserting it is false says
the same thing with one evaluation. Splitting it would not be. `||` splits
only by De Morgan and only if the left operand aborts, since it is usually
the nil guard that makes the right operand safe to evaluate; `&&` does not
split at all, because `!(a && b)` is a disjunction rather than two claims.

`if err := f(); err != nil { ... }` **is** handled, by inlining the init
rather than hoisting it. Hoisting puts the variable in the enclosing scope,
and two of those in one function then declare it twice and fail to compile;
inlining needs no scope analysis and reads better. It is declined where the
message still mentions the variable, since inlining would then evaluate the
call twice.

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

**The message, minus what the assertion now prints.** `"Server = %q, want %q"`
with the compared values becomes `"Server"`; a message carrying prose the
values do not supply is passed through whole.
