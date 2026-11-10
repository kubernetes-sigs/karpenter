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

// Package nsnamekey flags maps and unique handles keyed by the bare name of a
// namespaced Kubernetes object. Two objects with the same name in different
// namespaces collide; key by namespace and name (types.NamespacedName or
// client.ObjectKeyFromObject).
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3102: ResourceClaims were indexed with
//     unique.Make(claim.Name), so same-named claims in two namespaces collided
//     and Commit could panic on a duplicate key.
package nsnamekey

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "nsnamekey",
	Doc:      "flags maps and unique handles keyed by the bare name of a namespaced object; include the namespace (#3102)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// namespaced lists built-in namespaced kinds by package path.
var namespaced = map[string]map[string]bool{
	"k8s.io/api/core/v1": {
		"Pod": true, "PersistentVolumeClaim": true, "ConfigMap": true, "Secret": true,
		"Service": true, "ServiceAccount": true, "Endpoints": true, "Event": true,
	},
	"k8s.io/api/apps/v1": {
		"Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true,
	},
	"k8s.io/api/batch/v1":  {"Job": true, "CronJob": true},
	"k8s.io/api/policy/v1": {"PodDisruptionBudget": true},
	"k8s.io/api/resource/v1": {
		"ResourceClaim": true, "ResourceClaimTemplate": true,
	},
	"k8s.io/api/resource/v1beta1": {
		"ResourceClaim": true, "ResourceClaimTemplate": true,
	},
	"k8s.io/api/resource/v1beta2": {
		"ResourceClaim": true, "ResourceClaimTemplate": true,
	},
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.IndexExpr)(nil), (*ast.CallExpr)(nil)}, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.IndexExpr:
			if _, ok := types.Unalias(pass.TypesInfo.TypeOf(e.X)).Underlying().(*types.Map); ok {
				check(pass, e.Index, "map key")
			}
		case *ast.CallExpr:
			if isUniqueMake(pass, e) && len(e.Args) == 1 {
				check(pass, e.Args[0], "unique handle")
			}
		}
	})
	return nil, nil
}

func check(pass *analysis.Pass, key ast.Expr, what string) {
	owner := nameOwner(key)
	if owner == nil {
		return
	}
	t := pass.TypesInfo.TypeOf(owner)
	if t == nil {
		return
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Pkg() == nil || !namespaced[named.Obj().Pkg().Path()][named.Obj().Name()] {
		return
	}
	pass.Reportf(key.Pos(), "%s uses only the name of namespaced %s; same-named objects in different namespaces collide, key by namespace/name (#3102)", what, named.Obj().Name())
}

// nameOwner returns x for x.Name, x.ObjectMeta.Name, and x.GetName().
func nameOwner(e ast.Expr) ast.Expr {
	e = ast.Unparen(e)
	if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 0 {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "GetName" {
			return sel.X
		}
		return nil
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Name" {
		return nil
	}
	if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "ObjectMeta" {
		return inner.X
	}
	return sel.X
}

func isUniqueMake(pass *analysis.Pass, call *ast.CallExpr) bool {
	fun := ast.Unparen(call.Fun)
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	return ok && fn.Name() == "Make" && fn.Pkg() != nil && fn.Pkg().Path() == "unique"
}
