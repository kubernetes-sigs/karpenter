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

// Package errbranchuse flags reading a variable inside an `if err != nil`
// branch when the call that produced err also overwrote that variable with
// its own input, as in `x, err = f(x)`. Callers return a zero value with
// the error, so the branch sees an empty x instead of the original.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3350: multi-node consolidation calls
//     cmd.EmitRejectedEvents after `cmd, err = m.validator.Validate(ctx, cmd, ...)`
//     fails, so the rejected events are emitted for an empty command and
//     ConsolidationRejected never fires.
package errbranchuse

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "errbranchuse",
	Doc:      "flags reading x in the err branch of `x, err = f(x)`, where f returns a zero x on error (#3350)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.BlockStmt)(nil)}, func(n ast.Node) {
		stmts := n.(*ast.BlockStmt).List
		for i, s := range stmts {
			ifs, ok := s.(*ast.IfStmt)
			if !ok {
				continue
			}
			assign, _ := ifs.Init.(*ast.AssignStmt)
			if assign == nil && i > 0 {
				assign, _ = stmts[i-1].(*ast.AssignStmt)
			}
			if assign != nil {
				check(pass, assign, ifs)
			}
		}
	})
	return nil, nil
}

func check(pass *analysis.Pass, assign *ast.AssignStmt, ifs *ast.IfStmt) {
	if assign.Tok != token.ASSIGN || len(assign.Rhs) != 1 || len(assign.Lhs) < 2 {
		return
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return
	}
	errObj := objOf(pass, assign.Lhs[len(assign.Lhs)-1])
	if errObj == nil || !isErrNotNil(pass, ifs.Cond, errObj) {
		return
	}
	for _, lhs := range assign.Lhs[:len(assign.Lhs)-1] {
		obj := objOf(pass, lhs)
		if obj == nil || !usedIn(pass, call.Args, obj) {
			continue
		}
		ast.Inspect(ifs.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && pass.TypesInfo.Uses[id] == obj {
				pass.Reportf(id.Pos(), "%s was overwritten by the failing call on line %d; it holds the error-path return value, not the original (#3350)",
					id.Name, pass.Fset.Position(assign.Pos()).Line)
			}
			return true
		})
	}
}

func objOf(pass *analysis.Pass, e ast.Expr) types.Object {
	id, ok := e.(*ast.Ident)
	if !ok || id.Name == "_" {
		return nil
	}
	return pass.TypesInfo.ObjectOf(id)
}

func isErrNotNil(pass *analysis.Pass, cond ast.Expr, errObj types.Object) bool {
	be, ok := cond.(*ast.BinaryExpr)
	if !ok {
		return false
	}
	if be.Op == token.LAND || be.Op == token.LOR {
		return isErrNotNil(pass, be.X, errObj) || isErrNotNil(pass, be.Y, errObj)
	}
	if be.Op != token.NEQ {
		return false
	}
	id, ok := be.X.(*ast.Ident)
	if !ok || pass.TypesInfo.Uses[id] != errObj {
		return false
	}
	nilID, ok := be.Y.(*ast.Ident)
	return ok && nilID.Name == "nil"
}

func usedIn(pass *analysis.Pass, exprs []ast.Expr, obj types.Object) bool {
	found := false
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && pass.TypesInfo.Uses[id] == obj {
				found = true
			}
			return !found
		})
	}
	return found
}
