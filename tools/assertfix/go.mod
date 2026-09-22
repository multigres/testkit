// A nested module on purpose: this tool needs golang.org/x/tools, and testkit
// is a library whose dependency graph is part of its pitch. Nothing that
// imports testkit/assert should inherit an analysis framework.
module github.com/multigres/testkit/tools/assertfix

go 1.27

require golang.org/x/tools v0.50.0

require (
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
)
