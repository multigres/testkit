// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// The testify half of the tool.
//
// Mapping the modes is the easy part and falls out exactly: testify's assert
// package reports and continues, its require package aborts, which is
// NewCollecting and Require() here. A file mixing both gets a collecting
// receiver with Require() on the require calls, the same treatment
// t.Errorf and t.Fatalf get.
//
// Equality is the part that needs care. testify's Equal is
// reflect.DeepEqual for everything but []byte, and DeepEqual dereferences
// pointers where == compares addresses:
//
//	pointers:  == false   DeepEqual true
//
// So Equal maps to EqDeep and NotEqual to NotEqDeep. Mapping either onto Eq
// or NotEq would invert every pointer comparison, and in these repos most
// comparisons are pointers.

// tfix describes how one testify function becomes one of ours.
type tfix struct {
	method string
	// arity is the expected argument count after dropping t, or -1 for any.
	arity int
	swap  bool // swap the first two arguments
}

// Deliberately absent, each for a reason:
//
//	EqualValues  type-coercing equality, which the target package declines
//	ErrorAs      testify takes an out-parameter, ours returns the match, so
//	             the call shape differs and a rewrite would have to invent
//	             the variable
//	IsType,      compile-time concerns once generics exist
//	Implements
//	Eventually,  no equivalent, and approximating one in a migration is how
//	Never,       a test quietly stops meaning what it said
//	Regexp,
//	JSONEq,
//	YAMLEq,
//	Subset,
//	Condition,
//	WithinDuration,
//	InEpsilon
var testifyMap = map[string]tfix{
	"Equal":          {method: "EqDeep", arity: 2},
	"NotEqual":       {method: "NotEqDeep", arity: 2},
	"NoError":        {method: "NoError", arity: 1},
	"Error":          {method: "Error", arity: 1},
	"ErrorIs":        {method: "ErrorIs", arity: 2},
	"NotErrorIs":     {method: "NotErrorIs", arity: 2},
	"ErrorContains":  {method: "ErrorContains", arity: 2},
	"EqualError":     {method: "EqualError", arity: 2},
	"True":           {method: "True", arity: 1},
	"False":          {method: "False", arity: 1},
	"Nil":            {method: "Nil", arity: 1},
	"NotNil":         {method: "NotNil", arity: 1},
	"Zero":           {method: "Zero", arity: 1},
	"NotZero":        {method: "NotZero", arity: 1},
	"Len":            {method: "Len", arity: 2},
	"ElementsMatch":  {method: "ElementsMatch", arity: 2},
	"Panics":         {method: "Panics", arity: 1},
	"NotPanics":      {method: "NotPanics", arity: 1},
	"Fail":           {method: "Fail", arity: -1},
	"Contains":       {method: "Contains", arity: 2},
	"NotContains":    {method: "NotContains", arity: 2},
	"Empty":          {method: "Empty", arity: 1},
	"NotEmpty":       {method: "NotEmpty", arity: 1},
	"Same":           {method: "Same", arity: 2},
	"NotSame":        {method: "NotSame", arity: 2},
	"InDelta":        {method: "InDelta", arity: 3},
	"FileExists":     {method: "FileExists", arity: 1},
	"NoFileExists":   {method: "NoFileExists", arity: 1},
	"DirExists":      {method: "DirExists", arity: 1},
	"NoDirExists":    {method: "NoDirExists", arity: 1},
	"Greater":        {method: "Greater", arity: 2, swap: true},
	"GreaterOrEqual": {method: "GreaterOrEqual", arity: 2, swap: true},
	"Less":           {method: "Less", arity: 2, swap: true},
	"LessOrEqual":    {method: "LessOrEqual", arity: 2, swap: true},
}

// testifyCall recognises a testify call in this scope and returns the
// rewrite, or ok=false to leave it alone.
func testifyCall(
	pass *analysis.Pass,
	ce *ast.CallExpr,
	tName string,
) (method string, args []string, aborts, ok bool) {
	sel, is := ce.Fun.(*ast.SelectorExpr)
	if !is {
		return "", nil, false, false
	}
	pkgIdent, is := sel.X.(*ast.Ident)
	if !is {
		return "", nil, false, false
	}
	pkgName, is := pass.TypesInfo.Uses[pkgIdent].(*types.PkgName)
	if !is {
		return "", nil, false, false
	}
	path := pkgName.Imported().Path()
	switch {
	case strings.HasSuffix(path, "stretchr/testify/require"):
		aborts = true
	case strings.HasSuffix(path, "stretchr/testify/assert"):
		aborts = false
	default:
		return "", nil, false, false
	}

	// The -f variants differ only in taking a format; msgAndArgs is
	// variadic here, so they share a mapping with the plain names.
	name := strings.TrimSuffix(sel.Sel.Name, "f")
	m, found := testifyMap[name]
	if !found || m.method == "" {
		return "", nil, false, false
	}

	// The first argument is the TestingT and has to be this scope's own t,
	// or the receiver would report against a different test.
	if len(ce.Args) == 0 {
		return "", nil, false, false
	}
	if id, is := ce.Args[0].(*ast.Ident); !is || id.Name != tName {
		return "", nil, false, false
	}
	rest := ce.Args[1:]
	if m.arity >= 0 && len(rest) < m.arity {
		return "", nil, false, false
	}

	method = m.method
	fixed := rest
	if m.arity > 0 {
		fixed = rest[:m.arity]
	}
	trailing := rest[len(fixed):]

	// Type-dependent choices, the same ones the stdlib half makes.
	switch name {
	case "Equal", "NotEqual":
		if !sameType(pass, fixed[0], fixed[1]) {
			return "", nil, false, false
		}
	case "Contains", "NotContains":
		// Polymorphic upstream over strings, slices and maps; three separate
		// assertions here, so the choice is the operand's type.
		switch containerKind(pass, fixed[0]) {
		case kindString:
			method = "StrContains"
			if name == "NotContains" {
				method = "NotStrContains"
			}
		case kindSlice:
			// E is inferred from the slice, not from the element argument,
			// so []any fails the comparable constraint however concrete
			// the argument is.
			if !elemFits(pass, fixed[0], fixed[1]) {
				return "", nil, false, false
			}
		case kindMap:
			// Named here for what it actually checks, a map's keys rather
			// than its values, which is what testify means by it too.
			if !elemFits(pass, fixed[0], fixed[1]) {
				return "", nil, false, false
			}
			method = "HasKey"
			if name == "NotContains" {
				method = "NotHasKey"
			}
		default:
			return "", nil, false, false
		}
	case "Empty", "NotEmpty":
		// testify treats any zero value as empty, including 0 and false.
		// Ours needs something with a length, so anything else is Zero.
		if hasLength(pass, fixed[0]) {
			method = name
		} else {
			method = "Zero"
			if name == "NotEmpty" {
				method = "NotZero"
			}
		}
	case "ElementsMatch":
		// Upstream compares elements with DeepEqual. Ours hashes them, so
		// a []*Foo would compare addresses and fail on equal values. Only
		// pointer-free element types mean the same thing both ways.
		if !pointerFreeElem(pass, fixed[0]) {
			method = "ElementsMatchDeep"
		}
	case "Same", "NotSame":
		// Ours is typed over the pointee, so both sides must be pointers
		// to the same type.
		if !sameType(pass, fixed[0], fixed[1]) || !isPointer(pass, fixed[0]) {
			return "", nil, false, false
		}
	}

	out := make([]string, 0, len(rest))
	if m.swap && len(fixed) == 2 {
		out = append(out, render(pass, fixed[1]), render(pass, fixed[0]))
	} else {
		for _, a := range fixed {
			out = append(out, render(pass, a))
		}
	}
	for _, a := range trailing {
		out = append(out, render(pass, a))
	}
	return method, out, aborts, true
}

// usesTestifySuite reports whether the file builds on testify's suite
// package, which is a different testing model entirely and not something a
// per-assertion rewrite can move.
func usesTestifySuite(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path != nil && strings.Contains(imp.Path.Value, "stretchr/testify/suite") {
			return true
		}
	}
	return false
}

type containerK int

const (
	kindOther containerK = iota
	kindString
	kindSlice
	kindMap
)

func containerKind(pass *analysis.Pass, e ast.Expr) containerK {
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return kindOther
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		if u.Kind() == types.String {
			return kindString
		}
	case *types.Slice:
		return kindSlice
	case *types.Map:
		return kindMap
	}
	return kindOther
}

// elemFits reports whether the container's element type (a slice's element
// or a map's key) is comparable and the wanted value is assignable to it,
// which is what Contains, NotContains, HasKey and NotHasKey each need to
// infer their type parameters.
func elemFits(pass *analysis.Pass, container, want ast.Expr) bool {
	ct, wt := pass.TypesInfo.TypeOf(container), pass.TypesInfo.TypeOf(want)
	if ct == nil || wt == nil {
		return false
	}
	var elem types.Type
	switch u := ct.Underlying().(type) {
	case *types.Slice:
		elem = u.Elem()
	case *types.Map:
		elem = u.Key()
	default:
		return false
	}
	return strictlyComparable(elem) && types.AssignableTo(wt, elem)
}

// pointerFreeElem reports whether a slice's element type compares the same
// under == as under a deep comparison, which is true exactly when no pointer
// (or interface, map, slice or func) is reachable from it.
func pointerFreeElem(pass *analysis.Pass, e ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	sl, ok := t.Underlying().(*types.Slice)
	if !ok {
		return false
	}
	return pointerFree(sl.Elem(), map[types.Type]bool{})
}

func pointerFree(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Kind() != types.UnsafePointer
	case *types.Struct:
		for i := range u.NumFields() {
			if !pointerFree(u.Field(i).Type(), seen) {
				return false
			}
		}
		return true
	case *types.Array:
		return pointerFree(u.Elem(), seen)
	default:
		return false
	}
}

func isPointer(pass *analysis.Pass, e ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Pointer)
	return ok
}

// hasLength reports whether len() applies, which is what Len, Empty and
// NotEmpty need and what distinguishes them from Zero here.
func hasLength(pass *analysis.Pass, e ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Kind() == types.String
	case *types.Slice, *types.Map, *types.Array, *types.Chan:
		return true
	}
	return false
}
