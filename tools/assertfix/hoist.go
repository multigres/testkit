// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// stmtLoc is a statement's position within its immediate enclosing block:
// the block itself, and the index into its List.
type stmtLoc struct {
	block *ast.BlockStmt
	index int
}

// indexParents maps every statement in body to the block and index it lives
// at. Built once per collect() call and used by hoist safety, which has to
// look at what comes after an if statement in the same block.
func indexParents(body *ast.BlockStmt) map[ast.Stmt]stmtLoc {
	parent := map[ast.Stmt]stmtLoc{}
	ast.Inspect(body, func(n ast.Node) bool {
		blk, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i, st := range blk.List {
			parent[st] = stmtLoc{blk, i}
		}
		return true
	})
	return parent
}

// hoistSafe reports whether an if statement's init clause can be moved to a
// standalone statement immediately before it, using type information rather
// than text matching, as the tool's own value depends on: a hoist that
// shadows or collides compiles anyway and gives a wrong answer silently.
//
// Two things have to hold for every non-blank name the init declares:
//
//   - No scope enclosing the if already has a name of the same spelling,
//     which hoisting would either shadow or collide with.
//   - No later statement in the same block, including inside a nested
//     block, mentions the same name at all: that is what "widening the
//     scope" would actually change, whether by shadowing a still-live outer
//     variable or by capturing a reference that used to mean something
//     else.
//
// A third condition might seem needed: that the init's `:=` genuinely
// introduces every name here, rather than reusing one already declared via
// Go's redeclaration rule (at least one new name on the left is enough to
// reuse an old one within the same block). It does not need checking. An if
// statement's init clause is its own implicit block, per the language spec,
// so there is never an earlier `:=` in that exact scope for one here to
// redeclare against; every non-blank name in it is always a fresh
// declaration.
//
// The last check is deliberately coarser than it has to be: a same-named
// variable confined to its own nested if or for would only ever shadow the
// hoisted one, never collide with it, but this declines anyway rather than
// distinguish the two. One consequence worth knowing: it also means that of
// several sequential `if err := f(); err != nil { ... }` sites sharing a
// block, at most the last can ever hoist, since every earlier one always
// finds a later same-named init. Correspondingly, two such sites can never
// both reach hoisting, so there is nothing here to track across sites.
func hoistSafe(
	pass *analysis.Pass,
	ifs *ast.IfStmt,
	names []*ast.Ident,
	parent map[ast.Stmt]stmtLoc,
) bool {
	// hoistEdit inserts at the start of the if's line, which is only where
	// the if actually begins when nothing else shares that line before it.
	// gofmt'd input always satisfies this; input that is not can put a
	// hoisted statement before code that was meant to run first.
	if !startsOwnLine(pass, ifs.Pos()) {
		return false
	}
	loc, ok := parent[ifs]
	if !ok {
		return false
	}
	scope := pass.TypesInfo.Scopes[ifs]
	if scope == nil {
		return false
	}
	for _, id := range names {
		if id.Name == "_" {
			continue
		}
		for s := scope.Parent(); s != nil; s = s.Parent() {
			if s.Lookup(id.Name) != nil {
				return false
			}
		}
		if laterReference(pass, loc.block, loc.index, id.Name) {
			return false
		}
	}
	return true
}

// startsOwnLine reports whether pos is preceded only by whitespace back to
// the start of its line: the assumption hoistEdit's insertion point rests
// on. gofmt'd source always satisfies it, since a compound statement like
// `t.Run("sub", func(t *testing.T) { if err := f(); ... })` never survives
// gofmt on one line; input that has not been through gofmt yet can still
// reach this tool, and inserting at the raw line start there would put the
// hoisted statement before whatever else shares the line, running it
// before code that was meant to run first.
func startsOwnLine(pass *analysis.Pass, pos token.Pos) bool {
	position := pass.Fset.Position(pos)
	content, err := pass.ReadFile(position.Filename)
	if err != nil {
		return false
	}
	tf := pass.Fset.File(pos)
	lineStartOffset := pass.Fset.Position(tf.LineStart(position.Line)).Offset
	if lineStartOffset < 0 || position.Offset < lineStartOffset || position.Offset > len(content) {
		return false
	}
	for _, b := range content[lineStartOffset:position.Offset] {
		if b != ' ' && b != '\t' {
			return false
		}
	}
	return true
}

// laterReference reports whether name is declared or used anywhere among
// block's statements after index, recursing into nested blocks: a variable
// hoisted to block stays in scope for the rest of it, nested nested blocks
// included, so a later occurrence there is exactly what widening the scope
// would reach that it does not reach today.
//
// A struct field of the same spelling does not count, whether named as a
// selector (tc.want) or a composite literal key ({want: 1}): both live in a
// completely different namespace from a local variable, so a later one
// neither collides with nor shadows a hoisted "want", and the table-test
// shape that names both a field and a local "want" in the same block is
// exactly where treating them as the same name would cost real coverage.
// isField, not the position or node type an identifier appears at, is what
// actually distinguishes them: the same *ast.Ident node shape names a field
// in "tc.want" and a package-level function in "somepkg.Compute", and only
// the resolved object says which.
//
// Beyond that, this checks for the name existing at all, not for whether a
// given occurrence resolves to some other, unrelated declaration (a later
// closure's own same-named local, say). That is a deliberately coarser
// question than the real one, and answering only the coarser one is safe in
// the direction that matters: it only ever declines a hoist that might in
// fact be fine, never accepts one that is not.
func laterReference(pass *analysis.Pass, block *ast.BlockStmt, index int, name string) bool {
	for _, st := range block.List[index+1:] {
		found := false
		ast.Inspect(st, func(n ast.Node) bool {
			if found {
				return false
			}
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != name {
				return true
			}
			if isField(pass, id) {
				return true
			}
			if pass.TypesInfo.Uses[id] != nil || pass.TypesInfo.Defs[id] != nil {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// isField reports whether id resolves to a struct field rather than a
// variable, function or other package-level object.
func isField(pass *analysis.Pass, id *ast.Ident) bool {
	obj := pass.TypesInfo.Uses[id]
	if obj == nil {
		obj = pass.TypesInfo.Defs[id]
	}
	v, ok := obj.(*types.Var)
	return ok && v.IsField()
}

// hoistEdit inserts stmt as a standalone statement on its own line
// immediately before node, indented to match it. Column, not a raw source
// slice, gives the indentation: go/token's Position.Column counts bytes
// rather than expanding tabs, which for gofmt'd Go source (tabs only) is
// exactly the number of leading tabs.
func hoistEdit(pass *analysis.Pass, node ast.Node, stmt string) analysis.TextEdit {
	pos := node.Pos()
	lineStart := pass.Fset.File(pos).LineStart(pass.Fset.Position(pos).Line)
	indent := strings.Repeat("\t", pass.Fset.Position(pos).Column-1)
	return analysis.TextEdit{
		Pos:     lineStart,
		End:     lineStart,
		NewText: []byte(indent + stmt + "\n"),
	}
}
