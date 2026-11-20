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

// Package ginkgoonly flags plain `func TestXxx(t *testing.T)` tests. Karpenter
// tests are Ginkgo specs; the only Test function in a package should be the
// suite entry point that calls RunSpecs. Benchmarks, fuzz targets and
// TestMain are exempt.
//
// Inspired by review feedback repeated across five PRs by five reviewers:
//   - kubernetes-sigs/karpenter#2894: "majority of the codebase uses Ginkgo".
//   - kubernetes-sigs/karpenter#2962: "We use Ginkgo for testing almost everywhere".
//   - kubernetes-sigs/karpenter#3028: "most of our other tests use Ginkgo".
//   - kubernetes-sigs/karpenter#3217: "Why not ginkgo?".
//   - kubernetes-sigs/karpenter#3247: "LLMs ignore ginkgo".
package ginkgoonly

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

var Analyzer = &analysis.Analyzer{
	Name: "ginkgoonly",
	Doc:  "flags func TestXxx(t *testing.T) that is not a Ginkgo suite entry point calling RunSpecs (#2894, #2962, #3028, #3217, #3247)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		if !strings.HasSuffix(pass.Fset.File(f.Pos()).Name(), "_test.go") {
			continue
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !isTestName(fn.Name.Name) || !takesTestingT(pass, fn) {
				continue
			}
			if !callsRunSpecs(fn.Body) {
				pass.Reportf(fn.Name.Pos(), "%s is a plain go test; write it as a Ginkgo spec in the package suite (#2962)", fn.Name.Name)
			}
		}
	}
	return nil, nil
}

func isTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") || name == "TestMain" {
		return false
	}
	rest := name[len("Test"):]
	return rest == "" || !(rest[0] >= 'a' && rest[0] <= 'z')
}

func takesTestingT(pass *analysis.Pass, fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	ptr, ok := pass.TypesInfo.TypeOf(params[0].Type).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "testing" && named.Obj().Name() == "T"
}

func callsRunSpecs(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			found = found || fun.Name == "RunSpecs"
		case *ast.SelectorExpr:
			found = found || fun.Sel.Name == "RunSpecs"
		}
		return !found
	})
	return found
}
