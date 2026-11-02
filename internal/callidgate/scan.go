// SPDX-License-Identifier: Apache-2.0

package callidgate

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	key       = "callId"
	uuidPath  = "github.com/google/uuid"
	slogPath  = "log/slog"
	otelAttrs = "go.opentelemetry.io/otel/attribute"
)

// Offence is one "callId" attribute whose value is not a domain call id.
type Offence struct {
	File   string
	Line   int
	Reason string
}

func (o Offence) String() string { return fmt.Sprintf("%s:%d: %s", o.File, o.Line, o.Reason) }

// Scan loads the packages matching patterns (relative to dir, test files
// excluded) with full type information and reports every "callId" attribute
// whose value is not a uuid.UUID or the String() of one. Offence files are
// relative to dir. A package that fails to load or type-check is an error.
func Scan(dir string, patterns ...string) ([]Offence, error) {
	cfg := &packages.Config{
		Dir:   dir,
		Tests: false,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			problems = append(problems, fmt.Sprintf("%s: %s", p.PkgPath, e.Msg))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("cannot type-check, so cannot vouch for the code:\n  %s", strings.Join(problems, "\n  "))
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var out []Offence
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			s := &scanner{info: p.TypesInfo, fset: p.Fset}
			ast.Inspect(f, s.visit)
			for _, o := range s.out {
				if rel, err := filepath.Rel(absDir, o.File); err == nil {
					o.File = rel
				}
				out = append(out, o)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

type scanner struct {
	info *types.Info
	fset *token.FileSet
	out  []Offence
}

func (s *scanner) visit(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.CallExpr:
		s.call(n)
	case *ast.CompositeLit:
		s.literal(n)
	}
	return true
}

func (s *scanner) isKey(e ast.Expr) bool {
	tv, ok := s.info.Types[e]
	return ok && tv.Value != nil && tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) == key
}

func (s *scanner) report(at ast.Expr, why string) {
	p := s.fset.Position(at.Pos())
	s.out = append(s.out, Offence{File: p.Filename, Line: p.Line, Reason: why})
}

// pair checks the value that follows the key at args[i].
func (s *scanner) pair(args []ast.Expr, i int) {
	if i+1 >= len(args) {
		s.report(args[i], `"callId" has no value`)
		return
	}
	s.value(args[i], args[i+1])
}

func (s *scanner) value(keyExpr, v ast.Expr) {
	if !s.isDomain(v) {
		s.report(keyExpr, fmt.Sprintf(`"callId" holds %s, not a uuid.UUID or its String()`, s.typeOf(v)))
	}
}

func (s *scanner) typeOf(e ast.Expr) string {
	if t := s.info.TypeOf(e); t != nil {
		return t.String()
	}
	return "an untyped value"
}

func isUUID(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == uuidPath && n.Obj().Name() == "UUID"
}

func (s *scanner) isDomain(e ast.Expr) bool {
	e = ast.Unparen(e)
	if t := s.info.TypeOf(e); t != nil && isUUID(t) {
		return true
	}
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "String" {
		return false
	}
	t := s.info.TypeOf(sel.X)
	return t != nil && isUUID(t)
}

func isEmptyInterface(t types.Type) bool {
	if t == nil {
		return false
	}
	i, ok := t.Underlying().(*types.Interface)
	return ok && i.NumMethods() == 0
}

func (s *scanner) literal(c *ast.CompositeLit) {
	t := s.info.TypeOf(c)
	if t == nil {
		return
	}
	sl, ok := t.Underlying().(*types.Slice)
	if !ok || !isEmptyInterface(sl.Elem()) {
		return
	}
	for i, e := range c.Elts {
		if _, kv := e.(*ast.KeyValueExpr); kv {
			continue
		}
		if s.isKey(e) {
			s.pair(c.Elts, i)
		}
	}
}

func (s *scanner) call(c *ast.CallExpr) {
	// append into a []any.
	if id, ok := ast.Unparen(c.Fun).(*ast.Ident); ok {
		if b, ok := s.info.Uses[id].(*types.Builtin); ok && b.Name() == "append" {
			if len(c.Args) > 0 {
				if sl, ok := typeUnder(s.info.TypeOf(c.Args[0])).(*types.Slice); ok && isEmptyInterface(sl.Elem()) {
					for i := 1; i < len(c.Args); i++ {
						if s.isKey(c.Args[i]) {
							s.pair(c.Args, i)
						}
					}
				}
			}
			return
		}
	}
	// attribute.Key("callId").String(v) and friends.
	if sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr); ok {
		if conv, ok := ast.Unparen(sel.X).(*ast.CallExpr); ok && len(conv.Args) == 1 && s.isKey(conv.Args[0]) {
			if t := s.info.TypeOf(conv); t != nil {
				if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == otelAttrs && n.Obj().Name() == "Key" {
					if len(c.Args) == 1 {
						s.value(conv.Args[0], c.Args[0])
					}
					return
				}
			}
		}
	}
	// slog / attribute constructors: key string first, value second.
	if fn := s.callee(c); fn != nil && fn.Pkg() != nil && (fn.Pkg().Path() == slogPath || fn.Pkg().Path() == otelAttrs) {
		sig := fn.Type().(*types.Signature)
		if sig.Recv() == nil && sig.Params().Len() == 2 && !sig.Variadic() && len(c.Args) == 2 && s.isKey(c.Args[0]) {
			if b, ok := sig.Params().At(0).Type().Underlying().(*types.Basic); ok && b.Kind() == types.String {
				s.value(c.Args[0], c.Args[1])
				return
			}
		}
	}
	// A key in an any parameter position.
	sig, ok := typeUnder(s.info.TypeOf(c.Fun)).(*types.Signature)
	if !ok {
		return
	}
	for i, a := range c.Args {
		if !s.isKey(a) {
			continue
		}
		if isEmptyInterface(paramType(sig, i)) {
			s.pair(c.Args, i)
		}
	}
}

func (s *scanner) callee(c *ast.CallExpr) *types.Func {
	switch f := ast.Unparen(c.Fun).(type) {
	case *ast.Ident:
		fn, _ := s.info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := s.info.Uses[f.Sel].(*types.Func)
		return fn
	}
	return nil
}

func typeUnder(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	return t.Underlying()
}

// paramType is the type the i-th argument is assigned to, the element type
// for a variadic tail.
func paramType(sig *types.Signature, i int) types.Type {
	n := sig.Params().Len()
	if n == 0 {
		return nil
	}
	if sig.Variadic() && i >= n-1 {
		if sl, ok := sig.Params().At(n - 1).Type().(*types.Slice); ok {
			return sl.Elem()
		}
		return nil
	}
	if i >= n {
		return nil
	}
	return sig.Params().At(i).Type()
}
