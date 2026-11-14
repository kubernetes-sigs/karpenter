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

// Package strippedread flags reads of fields Karpenter strips from cached objects
// (cachefields.Stripped in pkg/operator/cachefields). Reading a stripped field from a
// cached object silently returns the zero value instead of failing.
//
// It flags reads of the field itself and calls to its Get<Field> method on the type that
// declares the field or on an interface in the same package (e.g. metav1.Object's
// GetManagedFields). It matches the type that declares the field, so a field stripped from
// one type is flagged wherever that declaring type is used. Assignments to the field are
// not reads and are not flagged. Test files are skipped, since tests don't read from the
// operator's cache.
//
// The table in zz_generated.fields.go is generated from cachefields.Stripped by `go generate`.
//
// Inspired by review on kubernetes-sigs/karpenter#3559 and #3586: stripping fields one PR
// at a time is fragile without a check that nothing reads them.
package strippedread

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

type field struct {
	pkg, typ, name string
}

var Analyzer = &analysis.Analyzer{
	Name: "strippedread",
	Doc:  "flags reads of fields Karpenter strips from cached objects (#3586)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		if strings.HasSuffix(pass.Fset.File(f.Pos()).Name(), "_test.go") {
			continue
		}
		writes := map[*ast.SelectorExpr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if assign, ok := n.(*ast.AssignStmt); ok {
				for _, lhs := range assign.Lhs {
					if sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr); ok {
						writes[sel] = true
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || writes[sel] {
				return true
			}
			if s, ok := pass.TypesInfo.Selections[sel]; ok {
				if f, ok := strippedField(s); ok {
					pass.Reportf(sel.Sel.Pos(), "%s.%s is stripped from cached objects, so it reads as empty; remove it from cachefields.Stripped to read it (#3586)", f.typ, f.name)
				}
			}
			return true
		})
	}
	return nil, nil
}

// strippedField returns the stripped field the selection reads, either directly or through its Get<Field> method.
func strippedField(s *types.Selection) (field, bool) {
	switch obj := s.Obj().(type) {
	case *types.Var:
		if s.Kind() != types.FieldVal {
			return field{}, false
		}
		owner := declaringType(s.Recv(), s.Index())
		if owner == nil {
			return field{}, false
		}
		return lookup(owner.Obj().Pkg(), owner.Obj().Name(), obj.Name(), false)
	case *types.Func:
		recv := obj.Signature().Recv()
		if recv == nil {
			return field{}, false
		}
		owner := named(recv.Type())
		if owner == nil || owner.Obj().Pkg() == nil {
			return field{}, false
		}
		_, isInterface := owner.Underlying().(*types.Interface)
		return lookup(owner.Obj().Pkg(), owner.Obj().Name(), obj.Name(), isInterface)
	}
	return field{}, false
}

// lookup finds a stripped field declared on pkg.typ named name, or read by a getter named name. A getter declared on an
// interface matches any stripped field in the interface's package.
func lookup(pkg *types.Package, typ, name string, isInterface bool) (field, bool) {
	if pkg == nil {
		return field{}, false
	}
	for _, f := range stripped {
		if f.pkg != pkg.Path() {
			continue
		}
		if f.typ == typ && f.name == name {
			return f, true
		}
		if "Get"+f.name == name && (isInterface || f.typ == typ) {
			return f, true
		}
	}
	return field{}, false
}

// declaringType walks the embedded fields on the selection's path and returns the named struct type that declares the
// selected field.
func declaringType(recv types.Type, index []int) *types.Named {
	t := recv
	for _, i := range index[:len(index)-1] {
		st, ok := deref(t).Underlying().(*types.Struct)
		if !ok {
			return nil
		}
		t = st.Field(i).Type()
	}
	return named(t)
}

func named(t types.Type) *types.Named {
	n, _ := deref(t).(*types.Named)
	return n
}

func deref(t types.Type) types.Type {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return types.Unalias(p.Elem())
	}
	return t
}
