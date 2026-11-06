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

// Package observedonly flags StatusConditions() calls without options that
// only read the result. Without status.WithObservedOnly(), operatorpkg's
// ConditionTypes.For sets every missing root and dependent condition to
// Unknown on the object it is called on. On an object from the informer cache
// or from cluster state, that read is an unsynchronized write to shared memory.
//
// Inside a controller-runtime predicate the object is always the shared cached
// object, so any StatusConditions() call there without options is flagged.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3372: the reboot watch predicate called
//     nc.StatusConditions().Get(...) on the cached NodeClaim, writing Unknown
//     conditions into the informer cache.
//   - kubernetes-sigs/karpenter#2962 review: a NodePool from the informer
//     cache was mutated without a DeepCopy. pkg/controllers/static/provisioning
//     hand-rolls a condition scan to avoid this and 12 "DO NOT MUTATE" comments
//     guard the same invariant.
package observedonly

import (
	"fmt"
	"go/ast"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const (
	statusPkg    = "github.com/awslabs/operatorpkg/status"
	predicatePkg = "sigs.k8s.io/controller-runtime/pkg/predicate"
)

var Analyzer = &analysis.Analyzer{
	Name:     "observedonly",
	Doc:      "flags read-only or predicate StatusConditions() calls that initialize conditions on shared objects; use status.WithObservedOnly() (#3372)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

var readMethods = map[string]bool{"Get": true, "IsTrue": true, "Root": true, "List": true, "IsDependentCondition": true}

var writeMethods = map[string]bool{
	"Set": true, "SetTrue": true, "SetTrueWithReason": true, "SetFalse": true,
	"SetUnknown": true, "SetUnknownWithReason": true, "Clear": true,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.WithStack([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node, push bool, stack []ast.Node) bool {
		// Test fixtures build their own objects, so initializing them is harmless.
		if !push || strings.HasSuffix(pass.Fset.Position(n.Pos()).Filename, "_test.go") {
			return true
		}
		call := n.(*ast.CallExpr)
		recv, ok := statusConditionsRecv(pass, call)
		if !ok || len(call.Args) != 0 {
			return true
		}
		method := chainedMethod(call, stack)
		if inPredicate(pass, stack) {
			if writeMethods[method] {
				pass.Reportf(call.Pos(), "StatusConditions().%s in a predicate writes to the shared cached object; predicates must not mutate (#3372)", method)
				return true
			}
			report(pass, stack, call, "StatusConditions() in a predicate initializes conditions on the shared cached object; pass status.WithObservedOnly() (#3372)")
			return true
		}
		if !readMethods[method] {
			return true
		}
		if fn := enclosingFunc(stack); fn != nil && writesConditions(pass, fn, types.ExprString(recv)) {
			return true
		}
		report(pass, stack, call, fmt.Sprintf("StatusConditions().%s only reads, but initializes missing conditions on %s; pass status.WithObservedOnly() (#3372)", method, types.ExprString(recv)))
		return true
	})
	return nil, nil
}

// statusConditionsRecv returns the receiver of X.StatusConditions(...) when the
// method is operatorpkg's StatusConditions(...status.ForOption) status.ConditionSet.
func statusConditionsRecv(pass *analysis.Pass, call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "StatusConditions" {
		return nil, false
	}
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok {
		return nil, false
	}
	sig := fn.Signature()
	if !sig.Variadic() || sig.Params().Len() != 1 {
		return nil, false
	}
	slice, ok := sig.Params().At(0).Type().(*types.Slice)
	if !ok || !isNamed(slice.Elem(), statusPkg, "ForOption") {
		return nil, false
	}
	return sel.X, true
}

func chainedMethod(call *ast.CallExpr, stack []ast.Node) string {
	if len(stack) < 3 {
		return ""
	}
	sel, ok := stack[len(stack)-2].(*ast.SelectorExpr)
	if !ok || sel.X != call {
		return ""
	}
	if outer, ok := stack[len(stack)-3].(*ast.CallExpr); !ok || outer.Fun != sel {
		return ""
	}
	return sel.Sel.Name
}

func writesConditions(pass *analysis.Pass, fn ast.Node, recv string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !writeMethods[sel.Sel.Name] {
			return !found
		}
		if inner, ok := sel.X.(*ast.CallExpr); ok {
			if r, ok := statusConditionsRecv(pass, inner); ok && types.ExprString(r) == recv {
				found = true
			}
		}
		return !found
	})
	return found
}

func inPredicate(pass *analysis.Pass, stack []ast.Node) bool {
	for i := len(stack) - 1; i > 0; i-- {
		if _, ok := stack[i].(*ast.FuncLit); !ok {
			continue
		}
		switch parent := stack[i-1].(type) {
		case *ast.KeyValueExpr:
			if i >= 2 {
				if lit, ok := stack[i-2].(*ast.CompositeLit); ok {
					if t := pass.TypesInfo.TypeOf(lit); t != nil && (isNamed(t, predicatePkg, "Funcs") || isNamed(t, predicatePkg, "TypedFuncs")) {
						return true
					}
				}
			}
		case *ast.CallExpr:
			if fn := calleeFunc(pass, parent); fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == predicatePkg &&
				(fn.Name() == "NewPredicateFuncs" || fn.Name() == "NewTypedPredicateFuncs") {
				return true
			}
		}
	}
	return false
}

func calleeFunc(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	fun := ast.Unparen(call.Fun)
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return nil
	}
	fn, _ := pass.TypesInfo.Uses[id].(*types.Func)
	return fn
}

func enclosingFunc(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return stack[i]
		}
	}
	return nil
}

func report(pass *analysis.Pass, stack []ast.Node, call *ast.CallExpr, msg string) {
	d := analysis.Diagnostic{Pos: call.Pos(), End: call.End(), Message: msg}
	if file, ok := stack[0].(*ast.File); ok {
		if name := importName(file, statusPkg); name != "" {
			d.SuggestedFixes = []analysis.SuggestedFix{{
				Message:   "pass status.WithObservedOnly()",
				TextEdits: []analysis.TextEdit{{Pos: call.Rparen, End: call.Rparen, NewText: []byte(name + ".WithObservedOnly()")}},
			}}
		}
	}
	pass.Report(d)
}

func importName(file *ast.File, path string) string {
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == path {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return "status"
		}
	}
	return ""
}

func isNamed(t types.Type, pkg, name string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := n.Obj()
	return obj.Name() == name && obj.Pkg() != nil && obj.Pkg().Path() == pkg
}
