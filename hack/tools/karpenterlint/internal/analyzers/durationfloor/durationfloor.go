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

// Package durationfloor flags a time difference (t.Sub, time.Until,
// clock.Since) used as a requeue delay or converted to whole seconds without
// a floor. The difference goes negative as soon as the deadline passes, which
// becomes a negative pod grace period (rejected by the API server) or a
// RequeueAfter <= 0 (no requeue at all).
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3032 / #3038: terminationGracePeriod minus elapsed
//     time went negative during Node Repair, so pod deletes failed and termination
//     hung. The fix was cherry-picked to 13 release branches (#3043, #3050-#3062).
//   - kubernetes-sigs/karpenter#3407: a failed reboot escalating to replacement
//     needed its drain bounded by the remaining time, clamped to zero.
//
// Wrap the value in max(..., 0) / lo.Max, or guard it with
// `if remaining := deadline.Sub(now); remaining > 0 { ... }`.
package durationfloor

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

var Analyzer = &analysis.Analyzer{
	Name:     "durationfloor",
	Doc:      "flags time differences used as RequeueAfter or integer seconds without a floor (#3038, #3407)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	nodes := []ast.Node{(*ast.CompositeLit)(nil), (*ast.CallExpr)(nil)}
	ins.WithStack(nodes, func(n ast.Node, push bool, stack []ast.Node) bool {
		if !push || strings.HasSuffix(pass.Fset.Position(n.Pos()).Filename, "_test.go") {
			return true
		}
		switch x := n.(type) {
		case *ast.CompositeLit:
			if !isReconcileResult(pass.TypesInfo.TypeOf(x)) {
				return true
			}
			for _, e := range x.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "RequeueAfter" && unfloored(pass, kv.Value, stack) {
					pass.Reportf(kv.Value.Pos(), "RequeueAfter from a time difference can be <= 0, which never requeues; floor it or guard with > 0 (#3038, #3407)")
				}
			}
		case *ast.CallExpr:
			if !isIntConversion(pass, x) {
				return true
			}
			inner, ok := x.Args[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := inner.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Seconds" && sel.Sel.Name != "Minutes") {
				return true
			}
			if isFloorArg(pass, stack) {
				return true
			}
			if unfloored(pass, sel.X, stack) {
				pass.Reportf(x.Pos(), "integer seconds from a time difference can go negative (e.g. a pod grace period); wrap in max(..., 0) (#3038)")
			}
		}
		return true
	})
	return nil, nil
}

// unfloored reports whether e is, or is an identifier defined from, a time
// difference that no enclosing `if v > 0` guard or max() call bounds.
func unfloored(pass *analysis.Pass, e ast.Expr, stack []ast.Node) bool {
	e = ast.Unparen(e)
	if isFloorCall(pass, e) {
		return false
	}
	if id, ok := e.(*ast.Ident); ok {
		obj := pass.TypesInfo.Uses[id]
		if obj == nil {
			return false
		}
		def := definingExpr(pass, obj, stack)
		if def == nil || !hasTimeDiff(pass, def) {
			return false
		}
		return !guarded(pass, obj, stack)
	}
	return hasTimeDiff(pass, e)
}

func hasTimeDiff(pass *analysis.Pass, e ast.Expr) bool {
	if isFloorCall(pass, ast.Unparen(e)) {
		return false
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isFloorCall(pass, call) {
			return false
		}
		fn, ok := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
		if !ok {
			return true
		}
		switch fn.Name() {
		case "Sub", "Until", "Since":
			if res := fn.Signature().Results(); res.Len() == 1 && isDuration(res.At(0).Type()) {
				found = true
			}
		}
		return true
	})
	return found
}

func isDuration(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "time" && n.Obj().Name() == "Duration"
}

func isFloorCall(pass *analysis.Pass, e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		_, builtin := pass.TypesInfo.Uses[fun].(*types.Builtin)
		return builtin && fun.Name == "max"
	case *ast.IndexExpr:
		return isFloorCall(pass, &ast.CallExpr{Fun: fun.X})
	case *ast.SelectorExpr:
		return fun.Sel.Name == "Max" && isPkg(pass, fun.X, "github.com/samber/lo")
	}
	return false
}

func isPkg(pass *analysis.Pass, e ast.Expr, path string) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	pn, ok := pass.TypesInfo.Uses[id].(*types.PkgName)
	return ok && pn.Imported().Path() == path
}

// isFloorArg reports whether the node on top of the stack is an argument of
// max()/lo.Max.
func isFloorArg(pass *analysis.Pass, stack []ast.Node) bool {
	for i := len(stack) - 2; i >= 0; i-- {
		switch p := stack[i].(type) {
		case *ast.CallExpr:
			if isFloorCall(pass, p) {
				return true
			}
			if !isConversion(pass, p) {
				return false
			}
		case *ast.ParenExpr:
		default:
			return false
		}
	}
	return false
}

func isConversion(pass *analysis.Pass, call *ast.CallExpr) bool {
	tv, ok := pass.TypesInfo.Types[call.Fun]
	return ok && tv.IsType()
}

func isIntConversion(pass *analysis.Pass, call *ast.CallExpr) bool {
	if len(call.Args) != 1 || !isConversion(pass, call) {
		return false
	}
	b, ok := pass.TypesInfo.Types[call.Fun].Type.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsInteger != 0
}

func isReconcileResult(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Name() == "Result" &&
		strings.HasSuffix(n.Obj().Pkg().Path(), "sigs.k8s.io/controller-runtime/pkg/reconcile")
}

// definingExpr finds the RHS that defines obj inside the enclosing function.
func definingExpr(pass *analysis.Pass, obj types.Object, stack []ast.Node) ast.Expr {
	var body ast.Node
	for i := len(stack) - 1; i >= 0; i-- {
		switch f := stack[i].(type) {
		case *ast.FuncDecl:
			body = f.Body
		case *ast.FuncLit:
			body = f.Body
		}
		if body != nil {
			break
		}
	}
	if body == nil {
		return nil
	}
	var def ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) != len(s.Rhs) {
				return true
			}
			for i, l := range s.Lhs {
				if id, ok := l.(*ast.Ident); ok && pass.TypesInfo.ObjectOf(id) == obj {
					def = s.Rhs[i]
				}
			}
		case *ast.ValueSpec:
			for i, nm := range s.Names {
				if pass.TypesInfo.Defs[nm] == obj && i < len(s.Values) {
					def = s.Values[i]
				}
			}
		}
		return true
	})
	return def
}

// guarded reports whether an enclosing if-condition bounds obj from below,
// e.g. `if remaining > 0` or `if remaining >= time.Second`.
func guarded(pass *analysis.Pass, obj types.Object, stack []ast.Node) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		ifs, ok := stack[i].(*ast.IfStmt)
		if !ok {
			continue
		}
		found := false
		ast.Inspect(ifs.Cond, func(n ast.Node) bool {
			be, ok := n.(*ast.BinaryExpr)
			if !ok {
				return true
			}
			if id, ok := be.X.(*ast.Ident); ok && pass.TypesInfo.Uses[id] == obj && (be.Op == token.GTR || be.Op == token.GEQ) {
				found = true
			}
			if id, ok := be.Y.(*ast.Ident); ok && pass.TypesInfo.Uses[id] == obj && (be.Op == token.LSS || be.Op == token.LEQ) {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}
