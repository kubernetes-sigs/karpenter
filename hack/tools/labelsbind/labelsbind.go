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

// Package labelsbind checks that vars bound by labels.Register are read only after it runs.
//
// labels.Register writes each option's final key into the vars in its Binds, so a value derived from a bound var
// before Register runs keeps the stale key. Bound vars carry a "// +labels:bound" marker in their doc comment,
// exported as an analysis fact so that other packages see it. The analyzer reports:
//
//   - reads of, and writes to, a marked var in package-level var initializers and init() bodies, which run before
//     main;
//   - entries written to a labels.Option's Binds that aren't &X of a marked var, in a Binds field of an Option
//     literal, in a slice literal assigned to opt.Binds, and in the appended values of append(opt.Binds, ...).
//
// Limitations: function literals are checked only when they are called where they are declared, and calls to named
// functions that read a marked var aren't followed. Binds values that aren't slice literals (a variable, or
// append(opt.Binds, other...)) aren't checked.
// Ginkgo container bodies (Describe, Context, DescribeTable and Entry arguments) run while the spec tree is built,
// before a BeforeEach registers labels, so a read of a marked var there sees the key before Register too; the
// analyzer can't tell those closures apart from ones that run later and doesn't report them.
package labelsbind

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const (
	marker     = "+labels:bound"
	labelsPath = "sigs.k8s.io/karpenter/pkg/labels"
	optionName = "Option"
	bindsField = "Binds"
)

var Analyzer = &analysis.Analyzer{
	Name:      "labelsbind",
	Doc:       "flags reads of // +labels:bound vars before labels.Register and labels.Option Binds entries that aren't &X of a marked var",
	Run:       run,
	FactTypes: []analysis.Fact{new(bound)},
}

// bound marks a package-level var that labels.Register may write.
type bound struct{}

func (*bound) AFact()         {}
func (*bound) String() string { return "labels:bound" }

func run(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if !hasMarker(vs.Doc) && (gd.Lparen.IsValid() || !hasMarker(gd.Doc)) {
					continue
				}
				for _, name := range vs.Names {
					if obj := pass.TypesInfo.Defs[name]; obj != nil {
						pass.ExportObjectFact(obj, new(bound))
					}
				}
			}
		}
	}
	for _, f := range pass.Files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, v := range spec.(*ast.ValueSpec).Values {
						checkReads(pass, v, "a package-level initializer")
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "init" && d.Body != nil {
					checkReads(pass, d.Body, "init()")
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			checkBinds(pass, n)
			return true
		})
	}
	return nil, nil
}

func hasMarker(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		if strings.TrimSpace(strings.TrimPrefix(c.Text, "//")) == marker {
			return true
		}
	}
	return false
}

func isBound(pass *analysis.Pass, v *types.Var) bool {
	return v != nil && pass.ImportObjectFact(v, new(bound))
}

// referencedVar returns the var e names, as X or pkg.X, or nil.
func referencedVar(pass *analysis.Pass, e ast.Expr) *types.Var {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		v, _ := pass.TypesInfo.Uses[e].(*types.Var)
		return v
	case *ast.SelectorExpr:
		v, _ := pass.TypesInfo.Uses[e.Sel].(*types.Var)
		return v
	}
	return nil
}

// checkReads reports reads of and writes to marked vars in code that runs before main. Taking a marked var's address
// isn't reported.
func checkReads(pass *analysis.Pass, root ast.Node, where string) {
	invoked := map[*ast.FuncLit]bool{}
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if lit, ok := ast.Unparen(n.Fun).(*ast.FuncLit); ok {
				invoked[lit] = true
			}
		case *ast.FuncLit:
			return invoked[n]
		case *ast.UnaryExpr:
			if n.Op == token.AND && isBound(pass, referencedVar(pass, n.X)) {
				return false
			}
		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range n.Lhs {
				if v := referencedVar(pass, lhs); isBound(pass, v) {
					pass.Reportf(lhs.Pos(), "%s is bound by labels.Register; writing it in %s bypasses labels.Register", v.Name(), where)
				} else {
					ast.Inspect(lhs, visit)
				}
			}
			for _, rhs := range n.Rhs {
				ast.Inspect(rhs, visit)
			}
			return false
		case *ast.Ident:
			if v, ok := pass.TypesInfo.Uses[n].(*types.Var); ok && isBound(pass, v) {
				pass.Reportf(n.Pos(), "%s is bound by labels.Register; reading it in %s sees the key before Register runs", n.Name, where)
			}
		}
		return true
	}
	ast.Inspect(root, visit)
}

// checkBinds reports Binds entries that aren't &X of a marked var: in Binds fields of Option literals, in slice
// literals assigned to opt.Binds, and in values appended to opt.Binds.
func checkBinds(pass *analysis.Pass, n ast.Node) {
	switch n := n.(type) {
	case *ast.CompositeLit:
		if !isOption(pass.TypesInfo.TypeOf(n)) {
			return
		}
		for _, elt := range n.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == bindsField {
					checkBindsLiteral(pass, kv.Value)
				}
			}
		}
	case *ast.AssignStmt:
		if n.Tok != token.ASSIGN || len(n.Lhs) != len(n.Rhs) {
			return
		}
		for i, lhs := range n.Lhs {
			if isBindsField(pass, lhs) {
				checkBindsLiteral(pass, n.Rhs[i])
			}
		}
	case *ast.CallExpr:
		fn, ok := ast.Unparen(n.Fun).(*ast.Ident)
		if !ok || n.Ellipsis.IsValid() || len(n.Args) == 0 || !isBindsField(pass, n.Args[0]) {
			return
		}
		if b, ok := pass.TypesInfo.Uses[fn].(*types.Builtin); ok && b.Name() == "append" {
			for _, e := range n.Args[1:] {
				checkBind(pass, e)
			}
		}
	}
}

// checkBindsLiteral checks each element of a slice literal written to Binds; other expressions aren't checked.
func checkBindsLiteral(pass *analysis.Pass, e ast.Expr) {
	if lit, ok := ast.Unparen(e).(*ast.CompositeLit); ok {
		for _, elt := range lit.Elts {
			checkBind(pass, elt)
		}
	}
}

func checkBind(pass *analysis.Pass, e ast.Expr) {
	u, ok := ast.Unparen(e).(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		pass.Reportf(e.Pos(), "%s is in Binds but isn't &X of a var marked // %s", types.ExprString(e), marker)
		return
	}
	if !isBound(pass, referencedVar(pass, u.X)) {
		pass.Reportf(u.Pos(), "%s is in Binds but %s isn't marked // %s", types.ExprString(u), types.ExprString(u.X), marker)
	}
}

// isBindsField returns whether e is opt.Binds for a labels.Option or *labels.Option opt.
func isBindsField(pass *analysis.Pass, e ast.Expr) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != bindsField {
		return false
	}
	return isOption(pass.TypesInfo.TypeOf(sel.X))
}

// isOption returns whether t is labels.Option or *labels.Option.
func isOption(t types.Type) bool {
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == labelsPath && named.Obj().Name() == optionName
}
