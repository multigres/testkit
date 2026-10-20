// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"fmt"
	"iter"
	"strings"
)

// counter is the smallest useful state: one integer.
type counter struct{ n int }

func (c counter) Clone() counter { return c }
func (c counter) Key() string {
	parity := "even"
	if c.n%2 != 0 {
		parity = "odd"
	}
	return fmt.Sprintf("n=%d\nparity=%s", c.n, parity)
}

// adder adds step to the counter, optionally only while n < limit.
type adder struct {
	name  string
	step  int
	limit int // 0 means unbounded
}

func (a adder) Name() string { return a.name }
func (a adder) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if a.limit > 0 && c.n >= a.limit {
			return
		}
		c.n += a.step
		yield(fmt.Sprintf("add %d", a.step), c)
	}
}

// vertex is a vertex of a hand-drawn graph.
type vertex string

func (n vertex) Clone() vertex { return n }
func (n vertex) Key() string   { return string(n) }

type graphMove struct {
	from vertex
	verb string
	to   vertex
}

// graph is an actor that walks a fixed edge list, in order.
type graph struct {
	name  string
	moves []graphMove
}

func (g graph) Name() string { return g.name }
func (g graph) Moves(s vertex) iter.Seq2[string, vertex] {
	return func(yield func(string, vertex) bool) {
		for _, m := range g.moves {
			if m.from == s && !yield(m.verb+" "+string(m.to), m.to) {
				return
			}
		}
	}
}

// table is the dining philosophers' table. forks[i] is the philosopher holding
// fork i, or -1. Philosopher i's left fork is i and right fork is (i+1)%3.
type table struct {
	forks [3]int
	ate   [3]bool
}

func newTable() table { return table{forks: [3]int{-1, -1, -1}} }

func (t table) Clone() table { return t }
func (t table) Key() string {
	var b strings.Builder
	for i := range t.forks {
		fmt.Fprintf(&b, "fork%d=%d\n", i, t.forks[i])
	}
	for i := range t.ate {
		fmt.Fprintf(&b, "ate%d=%v", i, t.ate[i])
		if i < len(t.ate)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (t table) anyAte() bool { return t.ate[0] || t.ate[1] || t.ate[2] }

type philosopher struct {
	id         int
	rightFirst bool
}

func (p philosopher) Name() string { return fmt.Sprintf("phil%d", p.id) }
func (p philosopher) Moves(t table) iter.Seq2[string, table] {
	return func(yield func(string, table) bool) {
		if t.ate[p.id] {
			return
		}
		first, second := p.id, (p.id+1)%3
		if p.rightFirst {
			first, second = second, first
		}
		switch {
		case t.forks[first] == -1:
			t.forks[first] = p.id
			yield(fmt.Sprintf("take fork%d", first), t)
		case t.forks[first] == p.id && t.forks[second] == -1:
			t.forks[second] = p.id
			yield(fmt.Sprintf("take fork%d", second), t)
		case t.forks[first] == p.id && t.forks[second] == p.id:
			t.forks[first], t.forks[second] = -1, -1
			t.ate[p.id] = true
			yield("eat", t)
		}
	}
}

// outer wraps a counter so a lens has something to leave untouched.
type outer struct {
	tag   string
	inner counter
}

func (o outer) Clone() outer { return o }
func (o outer) Key() string  { return o.tag + "\n" + o.inner.Key() }

// multiMover offers two differently named moves with the same verb while n < 3.
type multiMover struct{}

func (multiMover) Name() string { return "mover" }
func (multiMover) Moves(c counter) iter.Seq2[string, counter] {
	return func(yield func(string, counter) bool) {
		if c.n >= 3 {
			return
		}
		a, b := c, c
		a.n++
		b.n += 2
		if yield("inc a", a) {
			yield("inc b", b)
		}
	}
}
