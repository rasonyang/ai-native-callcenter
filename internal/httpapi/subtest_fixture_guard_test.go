// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A fixture keeps the *testing.T it was built with, and its helpers call
// f.t.Fatal*. Used from a subtest as built, they call FailNow on the PARENT
// from the subtest's goroutine: Go blames the parent, reports the subtest only
// as "subtest may have called FailNow on a parent test", and never runs the
// later subtests (#395, the struct form of #390).
//
// A subtest rebinds first: f := f.in(t). This test parses the package's test
// files and fails, naming file:line, when a t.Run func literal
//
//   - uses a fixture declared outside it without rebinding it, or
//   - calls a closure declared outside it that captures such a fixture.
//
// A fixture type is one with a field "t *testing.T" and a method "in". A
// fixture variable is a parameter or var of that type, or the result of a
// function that returns one (newFixture, phoneFixture, f.in(t)).
//
// It does not see: a fixture reached through a struct field, slice or map; a
// fixture passed as an argument to a named function that is itself declared
// at package level and called from a closure; a closure assigned to a
// variable twice or returned from a function; a fixture type that is not
// rebindable (dncStack has no in method, so it is not checked).
func TestNoSubtestUsesAFixtureBoundToTheParentsT(t *testing.T) {
	problems := subtestFixtureProblems(t, ".")
	if len(problems) > 0 {
		t.Fatalf("a subtest uses a fixture bound to the parent's t; start it with f := f.in(t):\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// The guard has to see the defect, or it guards nothing.
func TestTheSubtestFixtureGuardSeesTheDefect(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("base_test.go", `package httpapi
import "testing"
type fixture struct{ t *testing.T }
func (f *fixture) in(t *testing.T) *fixture { g := *f; g.t = t; return &g }
func (f *fixture) do() {}
func newFixture(t *testing.T) *fixture { return &fixture{t: t} }
`)
	write("good_test.go", `package httpapi
import "testing"
func TestGood(t *testing.T) {
	f := newFixture(t)
	reach := func(t *testing.T) { f := f.in(t); f.do() }
	t.Run("a", func(t *testing.T) {
		f := f.in(t)
		f.do()
		reach(t)
	})
	f.do()
}
`)
	write("direct_test.go", `package httpapi
import "testing"
func TestDirect(t *testing.T) {
	f := newFixture(t)
	t.Run("a", func(t *testing.T) {
		f.do()
	})
}
`)
	write("closure_test.go", `package httpapi
import "testing"
func TestClosure(t *testing.T) {
	f := newFixture(t)
	inner := func() { f.do() }
	outer := func() { inner() }
	t.Run("a", func(t *testing.T) {
		outer()
	})
}
`)
	write("nested_test.go", `package httpapi
import "testing"
func TestNested(t *testing.T) {
	f := newFixture(t)
	t.Run("a", func(t *testing.T) {
		f := f.in(t)
		t.Run("b", func(t *testing.T) {
			f.do()
		})
	})
}
`)
	got := subtestFixtureProblems(t, dir)
	want := []string{"closure_test.go:8", "direct_test.go:6", "nested_test.go:8"}
	if len(got) != len(want) {
		t.Fatalf("problems = %q, want %d (one in each of %q)", got, len(want), want)
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w+":") {
			t.Errorf("problems[%d] = %q, want %s", i, got[i], w)
		}
	}
}

// subtestFixtureProblems parses the _test.go files of dir and returns one
// "file:line:col: message" per offence, sorted.
func subtestFixtureProblems(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	g := newFixtureGuard(fset, files)
	return g.check()
}

type fixtureGuard struct {
	fset  *token.FileSet
	files []*ast.File
	// types are the rebindable fixture types; ctors maps a function name to
	// the result positions that are one.
	types map[string]bool
	ctors map[string][]int
	// scopes resolve identifiers; see declOf.
	problems map[string]bool
}

func newFixtureGuard(fset *token.FileSet, files []*ast.File) *fixtureGuard {
	g := &fixtureGuard{fset: fset, files: files, types: map[string]bool{}, ctors: map[string][]int{}, problems: map[string]bool{}}
	hasT := map[string]bool{}
	hasIn := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, fld := range st.Fields.List {
						for _, n := range fld.Names {
							if n.Name == "t" && isTestingT(fld.Type) {
								hasT[ts.Name.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil && d.Name.Name == "in" {
					if name := recvTypeName(d.Recv.List[0].Type); name != "" {
						hasIn[name] = true
					}
				}
			}
		}
	}
	for n := range hasT {
		if hasIn[n] {
			g.types[n] = true
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Type.Results == nil {
				continue
			}
			pos := 0
			for _, r := range fd.Type.Results.List {
				n := max(len(r.Names), 1)
				for i := 0; i < n; i++ {
					if g.isFixtureType(r.Type) {
						g.ctors[fd.Name.Name] = append(g.ctors[fd.Name.Name], pos)
					}
					pos++
				}
			}
		}
	}
	return g
}

func isTestingT(e ast.Expr) bool {
	s, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := s.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "T"
}

func recvTypeName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func (g *fixtureGuard) isFixtureType(e ast.Expr) bool {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	id, ok := e.(*ast.Ident)
	return ok && g.types[id.Name]
}

// binding is one declaration of a local name: where it is, and what it is.
type binding struct {
	pos       token.Pos
	fixture   bool
	closure   *ast.FuncLit // set when the name is bound to a func literal
	sc        *scope       // the scope a closure was declared in
	declScope ast.Node
}

// scope is a lexical scope with the bindings made in it so far.
type scope struct {
	parent *scope
	names  map[string]*binding
	node   ast.Node
}

func (s *scope) lookup(name string) *binding {
	for c := s; c != nil; c = c.parent {
		if b, ok := c.names[name]; ok {
			return b
		}
	}
	return nil
}

func (g *fixtureGuard) check() []string {
	for _, f := range g.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			sc := &scope{names: map[string]*binding{}, node: fd}
			g.bindFields(sc, fd.Recv)
			g.bindFields(sc, fd.Type.Params)
			g.walkBlock(fd.Body, sc, nil)
		}
	}
	out := make([]string, 0, len(g.problems))
	for p := range g.problems {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (g *fixtureGuard) bindFields(sc *scope, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, fld := range fl.List {
		for _, n := range fld.Names {
			sc.names[n.Name] = &binding{pos: n.Pos(), fixture: g.isFixtureType(fld.Type), declScope: sc.node}
		}
	}
}

// walkBlock walks statements in order, so a name is bound only after its
// declaration. sub is the innermost enclosing t.Run literal, or nil.
func (g *fixtureGuard) walkBlock(b *ast.BlockStmt, sc *scope, sub *ast.FuncLit) {
	inner := &scope{parent: sc, names: map[string]*binding{}, node: b}
	for _, st := range b.List {
		g.walkNode(st, inner, sub)
	}
}

func (g *fixtureGuard) walkNode(n ast.Node, sc *scope, sub *ast.FuncLit) {
	switch n := n.(type) {
	case nil:
		return
	case *ast.AssignStmt:
		for _, r := range n.Rhs {
			g.walkNode(r, sc, sub)
		}
		if n.Tok == token.DEFINE {
			for i, l := range n.Lhs {
				id, ok := l.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				b := &binding{pos: id.Pos(), declScope: sc.node}
				if len(n.Rhs) == len(n.Lhs) {
					b.fixture = g.yieldsFixture(n.Rhs[i], 0)
					if fl, ok := n.Rhs[i].(*ast.FuncLit); ok {
						b.closure, b.sc = fl, sc
					}
				} else if len(n.Rhs) == 1 {
					b.fixture = g.yieldsFixture(n.Rhs[0], i)
				}
				sc.names[id.Name] = b
			}
		}
		return
	case *ast.DeclStmt:
		gd, ok := n.Decl.(*ast.GenDecl)
		if !ok {
			return
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, v := range vs.Values {
				g.walkNode(v, sc, sub)
			}
			for i, id := range vs.Names {
				b := &binding{pos: id.Pos(), declScope: sc.node}
				switch {
				case vs.Type != nil:
					b.fixture = g.isFixtureType(vs.Type)
				case len(vs.Values) == len(vs.Names):
					b.fixture = g.yieldsFixture(vs.Values[i], 0)
					if fl, ok := vs.Values[i].(*ast.FuncLit); ok {
						b.closure, b.sc = fl, sc
					}
				case len(vs.Values) == 1:
					b.fixture = g.yieldsFixture(vs.Values[0], i)
				}
				sc.names[id.Name] = b
			}
		}
		return
	case *ast.FuncLit:
		fs := &scope{parent: sc, names: map[string]*binding{}, node: n}
		g.bindFields(fs, n.Type.Params)
		g.walkBlock(n.Body, fs, sub)
		return
	case *ast.BlockStmt:
		g.walkBlock(n, sc, sub)
		return
	case *ast.CallExpr:
		if lit := g.subtestLiteral(n); lit != nil {
			for _, a := range n.Args {
				if a != ast.Expr(lit) {
					g.walkNode(a, sc, sub)
				}
			}
			fs := &scope{parent: sc, names: map[string]*binding{}, node: lit}
			g.bindFields(fs, lit.Type.Params)
			g.walkBlock(lit.Body, fs, lit)
			return
		}
	case *ast.Ident:
		g.use(n, sc, sub)
		return
	case *ast.SelectorExpr:
		// x.in(...) is the rebind itself; x.Field uses x.
		if n.Sel.Name != "in" {
			g.walkNode(n.X, sc, sub)
		}
		return
	case *ast.KeyValueExpr:
		// A struct key is not a use; a map key is, but never a fixture.
		g.walkNode(n.Value, sc, sub)
		return
	}
	// Generic descent. Statements that open scopes (if, for, switch, range)
	// are walked flat; a name they declare lands in the enclosing block's
	// scope, which can only make the guard stricter, never looser.
	ast.Inspect(n, func(c ast.Node) bool {
		if c == n || c == nil {
			return true
		}
		switch c.(type) {
		case *ast.AssignStmt, *ast.DeclStmt, *ast.FuncLit, *ast.BlockStmt, *ast.CallExpr, *ast.Ident, *ast.SelectorExpr, *ast.KeyValueExpr:
			g.walkNode(c, sc, sub)
			return false
		}
		return true
	})
}

// subtestLiteral returns the func literal of a call X.Run(name, func(t *testing.T) {...}).
func (g *fixtureGuard) subtestLiteral(c *ast.CallExpr) *ast.FuncLit {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Run" || len(c.Args) != 2 {
		return nil
	}
	lit, ok := c.Args[1].(*ast.FuncLit)
	if !ok || len(lit.Type.Params.List) != 1 || !isTestingT(lit.Type.Params.List[0].Type) {
		return nil
	}
	return lit
}

// yieldsFixture reports whether expression e's result number idx is a fixture.
func (g *fixtureGuard) yieldsFixture(e ast.Expr, idx int) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		for _, p := range g.ctors[fn.Name] {
			if p == idx {
				return true
			}
		}
	case *ast.SelectorExpr:
		return fn.Sel.Name == "in" && idx == 0
	}
	return false
}

// use checks one identifier read inside sub.
func (g *fixtureGuard) use(id *ast.Ident, sc *scope, sub *ast.FuncLit) {
	if sub == nil {
		return
	}
	b := sc.lookup(id.Name)
	if b == nil || b.pos >= sub.Pos() && b.pos < sub.End() {
		return // not local, or declared inside the subtest
	}
	switch {
	case b.fixture:
		g.report(id, "fixture %s is declared outside this subtest; start the subtest with %s := %s.in(t)", id.Name, id.Name, id.Name)
	case b.closure != nil:
		if cap := g.captured(b.closure, b.sc, map[*ast.FuncLit]bool{}); cap != "" {
			g.report(id, "closure %s captures fixture %s of the parent; take the subtest's t and rebind inside it with %s := %s.in(t)", id.Name, cap, cap, cap)
		}
	}
}

// captured names a fixture that lit uses without rebinding, declared outside
// lit, directly or through another closure it calls; "" if none.
func (g *fixtureGuard) captured(lit *ast.FuncLit, sc *scope, seen map[*ast.FuncLit]bool) string {
	if seen[lit] {
		return ""
	}
	seen[lit] = true
	found := ""
	var visit func(n ast.Node, ls *scope)
	visit = func(n ast.Node, ls *scope) {
		if found != "" || n == nil {
			return
		}
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, r := range n.Rhs {
				visit(r, ls)
			}
			if n.Tok == token.DEFINE {
				for i, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						b := &binding{pos: id.Pos()}
						if len(n.Rhs) == len(n.Lhs) {
							b.fixture = g.yieldsFixture(n.Rhs[i], 0)
						}
						ls.names[id.Name] = b
					}
				}
			}
			return
		case *ast.SelectorExpr:
			if n.Sel.Name != "in" {
				visit(n.X, ls)
			}
			return
		case *ast.FuncLit:
			ns := &scope{parent: ls, names: map[string]*binding{}}
			g.bindFields(ns, n.Type.Params)
			visit(n.Body, ns)
			return
		case *ast.Ident:
			b := ls.lookup(n.Name)
			if b == nil {
				return
			}
			outside := b.pos < lit.Pos() || b.pos >= lit.End()
			if b.fixture && outside {
				found = n.Name
			} else if b.closure != nil && outside {
				if f := g.captured(b.closure, b.sc, seen); f != "" {
					found = f
				}
			}
			return
		}
		ast.Inspect(n, func(c ast.Node) bool {
			if c == n || c == nil || found != "" {
				return c == n && found == ""
			}
			switch c.(type) {
			case *ast.AssignStmt, *ast.FuncLit, *ast.Ident, *ast.SelectorExpr:
				visit(c, ls)
				return false
			}
			return true
		})
	}
	// Resolve names against the scope the literal was declared in plus its
	// own parameters. A rebinding inside the literal shadows the outer name.
	ls := &scope{parent: sc, names: map[string]*binding{}}
	g.bindFields(ls, lit.Type.Params)
	visit(lit.Body, ls)
	return found
}

func (g *fixtureGuard) report(id *ast.Ident, format string, args ...any) {
	p := g.fset.Position(id.Pos())
	g.problems[fmt.Sprintf("%s:%d:%d: %s", filepath.Base(p.Filename), p.Line, p.Column, fmt.Sprintf(format, args...))] = true
}
