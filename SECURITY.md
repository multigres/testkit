# Security policy

## Scope

testkit is a test-only library: `assert`, `golden` and `ctrltest` are imported
from `_test.go` files, and `assertfix` is a developer tool run by hand. None of
it is intended to run in production, handle untrusted input, or hold
credentials.

That narrows what a vulnerability here looks like, and it does not narrow it to
nothing. The two shapes worth reporting:

- **`assertfix` producing a wrong rewrite.** It edits test sources with the
  type checker's blessing, and a conversion that silently weakens an assertion
  leaves a green test over broken code. That is a correctness hole in
  somebody's test suite, and we treat it as this repo's highest-severity class
  of bug whether or not it fits a CVE.
- **An assertion that can pass when it should fail.** Same reasoning: the whole
  value of this repo is that a green test means something.

Anything reachable only by a test author against their own code, such as
`golden.AssertYAML` writing to a path the test itself supplied, is a bug rather
than a vulnerability. Report it as an issue.

## Reporting

Use GitHub's private vulnerability reporting on this repository
(**Security → Report a vulnerability**). That opens a private advisory visible
only to maintainers.

Please do not open a public issue for something in the two classes above until
we have had a chance to look. Include the version or commit, a minimal
reproducer, and for an `assertfix` report the before and after source.

We aim to acknowledge within three working days.

## Supported versions

The latest tag, and `main`. Nothing older.

While this is `v0` a fix ships in the next tag rather than as a backport, so
"upgrade" is the whole remediation story. If that is ever not good enough for
something reported here, say so in the report and we will work out what a
patch release would have to look like.
