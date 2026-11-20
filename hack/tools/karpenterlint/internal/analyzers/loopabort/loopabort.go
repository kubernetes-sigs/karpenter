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

// Package loopabort flags returning a non-nil error from inside a loop over
// NodePools, NodeClaims, StateNodes or disruption Candidates. Those items are
// independent, so one bad item should be skipped (and surfaced with an event,
// metric or log) rather than abort the pass for every other item.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3002: one NodePool's failure in the nodeoverlay
//     controller stopped overlays from being applied to every NodePool.
//   - kubernetes-sigs/karpenter#3398 / #3403: #3398 added `return []Command{}, err`
//     after staticReservations failed inside the NodePool loop in static drift and
//     the candidate loop in repair; #3403 fixed it the next day. Static drift also
//     leaked node-limit reservations already taken by earlier NodePools.
//
// Returns of ctx.Err() are allowed, since cancellation should end the pass.
// Aggregators that must fail as a whole take
// `//nolint:karpenterlint // <why the whole pass must fail>`.
package loopabort

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "loopabort",
	Doc:      "flags error returns inside loops over NodePools, NodeClaims, StateNodes or Candidates, where one item aborts all (#3002, #3403)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

type item struct{ pkgSuffix, name string }

var items = []item{
	{"karpenter/pkg/apis/v1", "NodePool"},
	{"karpenter/pkg/apis/v1", "NodeClaim"},
	{"karpenter/pkg/controllers/disruption", "Candidate"},
	{"karpenter/pkg/controllers/state", "StateNode"},
}

var errorType = types.Universe.Lookup("error").Type()

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	reported := map[*ast.ReturnStmt]bool{}
	ins.Preorder([]ast.Node{(*ast.RangeStmt)(nil)}, func(n ast.Node) {
		rs := n.(*ast.RangeStmt)
		if strings.HasSuffix(pass.Fset.Position(rs.Pos()).Filename, "_test.go") {
			return
		}
		kind := itemKind(pass.TypesInfo.TypeOf(rs.X))
		if kind == "" {
			return
		}
		ast.Inspect(rs.Body, func(m ast.Node) bool {
			switch x := m.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ReturnStmt:
				if len(x.Results) == 0 {
					return true
				}
				last := x.Results[len(x.Results)-1]
				if isNil(last) || isCtxErr(last) {
					return true
				}
				if t := pass.TypesInfo.TypeOf(last); t != nil && types.Identical(t, errorType) && !reported[x] {
					reported[x] = true
					pass.Reportf(x.Pos(), "error return inside a loop over %s aborts every remaining %s; skip this one and surface the error instead (#3002, #3403)", kind, kind)
				}
			}
			return true
		})
	})
	return nil, nil
}

// itemKind returns the item name when t is a collection whose elements (or
// whose map values' elements) are one of the tracked types.
func itemKind(t types.Type) string {
	el := elemOf(t)
	if el == nil {
		return ""
	}
	if k := match(el); k != "" {
		return k
	}
	if inner := elemOf(el); inner != nil {
		if _, isPtr := el.(*types.Pointer); !isPtr {
			return match(inner)
		}
	}
	return ""
}

func match(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return ""
	}
	for _, it := range items {
		if n.Obj().Name() == it.name && strings.HasSuffix(n.Obj().Pkg().Path(), it.pkgSuffix) {
			return it.name
		}
	}
	return ""
}

func elemOf(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	switch u := t.Underlying().(type) {
	case *types.Slice:
		return u.Elem()
	case *types.Array:
		return u.Elem()
	case *types.Map:
		return u.Elem()
	case *types.Pointer:
		if a, ok := u.Elem().Underlying().(*types.Array); ok {
			return a.Elem()
		}
	}
	return nil
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func isCtxErr(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Err" && len(call.Args) == 0
}
