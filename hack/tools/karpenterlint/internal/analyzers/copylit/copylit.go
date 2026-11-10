/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package copylit flags a struct literal that copies a value field by field
// (`T{A: x.A, B: x.B}` with x of type T) but leaves fields of T unset. When
// someone adds a field to T, every hand-written copy silently drops it.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3278: the nodeoverlay apply copied InstanceType
//     field by field and dropped the new DynamicResources field.
//   - Still on main after #3278: the price overlay copy of Offering drops
//     CapacityOverride and OverheadOverride (pkg/controllers/nodeoverlay/store.go),
//     and newStateFromNodeClaim drops podDisruptionCosts (pkg/controllers/state/cluster.go).
//
// The capacity buffer template bugs (#3166, #3174, #3189) are the same class
// but convert between types (PodTemplateSpec to PodSpec), which this analyzer
// does not follow.
//
// Fields assigned to the same variable later in the function count as set.
// Intentional partial copies take `//karpenterlint:partial-copy <reason>` on
// the literal's line or the line above.
package copylit

import (
	"go/ast"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "copylit",
	Doc:      "flags field-by-field struct copies that leave fields unset (#3278)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const marker = "//karpenterlint:partial-copy"

// minCopied is how many `F: x.F` entries make a literal a copy rather than a
// constructor that happens to read one field.
const minCopied = 2

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	suppressed := suppressedLines(pass)
	ins.WithStack([]ast.Node{(*ast.CompositeLit)(nil)}, func(n ast.Node, push bool, stack []ast.Node) bool {
		if !push {
			return true
		}
		cl := n.(*ast.CompositeLit)
		pos := pass.Fset.Position(cl.Pos())
		if strings.HasSuffix(pos.Filename, "_test.go") || strings.Contains(pos.Filename, "zz_generated") {
			return true
		}
		if suppressed[pos.Filename][pos.Line] || suppressed[pos.Filename][pos.Line-1] {
			return true
		}
		named, st := structOf(pass.TypesInfo.TypeOf(cl))
		if st == nil {
			return true
		}
		set := map[string]bool{}
		copied := map[string]int{}
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			k, ok := kv.Key.(*ast.Ident)
			if !ok {
				return true
			}
			set[k.Name] = true
			if sel, ok := kv.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == k.Name {
				if src, _ := structOf(pass.TypesInfo.TypeOf(sel.X)); src != nil && types.Identical(src, named) {
					copied[types.ExprString(sel.X)]++
				}
			}
		}
		for _, f := range assignedLater(pass, cl, stack) {
			set[f] = true
		}
		for src, c := range copied {
			if c < minCopied {
				continue
			}
			var missing []string
			for i := range st.NumFields() {
				f := st.Field(i)
				if set[f.Name()] || skipField(f) || (!f.Exported() && f.Pkg() != pass.Pkg) {
					continue
				}
				missing = append(missing, f.Name())
			}
			if len(missing) > 0 {
				pass.Reportf(cl.Pos(), "%s is copied field by field from %s but drops %s; set them, or mark %s <reason> (#3278)",
					named.Obj().Name(), src, strings.Join(missing, ", "), marker)
			}
		}
		return true
	})
	return nil, nil
}

func structOf(t types.Type) (*types.Named, *types.Struct) {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok {
		return nil, nil
	}
	s, ok := n.Underlying().(*types.Struct)
	if !ok {
		return nil, nil
	}
	return n, s
}

func skipField(f *types.Var) bool {
	t := f.Type().String()
	return f.Name() == "_" || strings.HasPrefix(t, "sync.") || strings.HasPrefix(t, "sync/atomic.") || strings.Contains(t, "noCopy") ||
		(f.Embedded() && f.Name() == "TypeMeta")
}

// assignedLater returns field names set as `v.F = ...` in the enclosing
// function, where v is the variable the literal is assigned to.
func assignedLater(pass *analysis.Pass, cl *ast.CompositeLit, stack []ast.Node) []string {
	var target types.Object
	var body *ast.BlockStmt
	for i := len(stack) - 2; i >= 0; i-- {
		switch p := stack[i].(type) {
		case *ast.AssignStmt:
			if target == nil {
				for j, r := range p.Rhs {
					if j < len(p.Lhs) && (r == cl || unaddr(r) == cl) {
						if id, ok := p.Lhs[j].(*ast.Ident); ok {
							target = pass.TypesInfo.ObjectOf(id)
						}
					}
				}
			}
		case *ast.ValueSpec:
			if target == nil {
				for j, v := range p.Values {
					if j < len(p.Names) && (v == cl || unaddr(v) == cl) {
						target = pass.TypesInfo.Defs[p.Names[j]]
					}
				}
			}
		case *ast.FuncDecl:
			body = p.Body
		case *ast.FuncLit:
			body = p.Body
		}
		if body != nil {
			break
		}
	}
	if target == nil || body == nil {
		return nil
	}
	var fields []string
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, l := range as.Lhs {
			sel, ok := l.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); ok && pass.TypesInfo.Uses[id] == target && !slices.Contains(fields, sel.Sel.Name) {
				fields = append(fields, sel.Sel.Name)
			}
		}
		return true
	})
	return fields
}

func unaddr(e ast.Expr) ast.Expr {
	if u, ok := e.(*ast.UnaryExpr); ok {
		return u.X
	}
	return e
}

func suppressedLines(pass *analysis.Pass) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, f := range pass.Files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.HasPrefix(c.Text, marker) {
					pos := pass.Fset.Position(c.Pos())
					if out[pos.Filename] == nil {
						out[pos.Filename] = map[int]bool{}
					}
					out[pos.Filename][pos.Line] = true
				}
			}
		}
	}
	return out
}
