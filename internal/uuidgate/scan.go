// SPDX-License-Identifier: Apache-2.0

package uuidgate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const uuidPath = "github.com/google/uuid"

var banned = map[string]bool{
	"New": true, "NewString": true, "NewRandom": true, "NewRandomFromReader": true,
}

// Offence is one call of a banned constructor.
type Offence struct {
	File string
	Line int
	Call string
}

func (o Offence) String() string { return fmt.Sprintf("%s:%d: uuid.%s", o.File, o.Line, o.Call) }

// Scan reports each call of a banned github.com/google/uuid function in one Go
// source file, following the file's own import name (alias or dot import).
func Scan(filename string, src []byte) ([]Offence, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	name, dot := "", false
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != uuidPath {
			continue
		}
		switch {
		case imp.Name == nil:
			name = "uuid"
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name != "_":
			name = imp.Name.Name
		}
	}
	if name == "" && !dot {
		return nil, nil
	}
	var out []Offence
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok && name != "" && id.Name == name && id.Obj == nil && banned[fn.Sel.Name] {
				out = append(out, Offence{filename, fset.Position(call.Pos()).Line, fn.Sel.Name})
			}
		case *ast.Ident:
			// A dot import brings the names into file scope; a local
			// declaration of the same name has an Obj.
			if dot && fn.Obj == nil && banned[fn.Name] {
				out = append(out, Offence{filename, fset.Position(call.Pos()).Line, fn.Name})
			}
		}
		return true
	})
	return out, nil
}

// ScanTree scans every non-test .go file under root (skipping testdata,
// vendor, node_modules and .git); offence files are relative to root.
func ScanTree(root string) ([]Offence, error) {
	var out []Offence
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		found, err := Scan(rel, src)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		out = append(out, found...)
		return nil
	})
	return out, err
}
