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

// Package hashunexported flags hashstructure.Hash calls whose argument type
// reaches a struct with unexported fields. hashstructure skips unexported
// fields silently, so values that differ only in those fields hash the same.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3401: TopologyGroup.Hash built the group key from
//     the wrong inputs, so a relaxed pod landed in a different topology group
//     than its siblings.
//   - TopologyGroup.Hash (pkg/controllers/provisioning/scheduling/topologygroup.go)
//     hashes []scheduling.Requirements, whose operator and values live in
//     unexported fields, so `zone In [a]`, `zone In [b]` and `zone NotIn [a]`
//     all produce the same hash. Found while researching #3401 and #3102.
//
// Types implementing hashstructure.Hashable, interface-typed arguments and
// fields tagged `hash:"ignore"` are skipped; time.Time is allowed.
package hashunexported

import (
	"go/ast"
	"go/types"
	"reflect"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

var Analyzer = &analysis.Analyzer{
	Name:     "hashunexported",
	Doc:      "flags hashstructure.Hash on types with unexported fields, which hashstructure silently ignores (#3401)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const hashstructurePkg = "github.com/mitchellh/hashstructure"

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		fn, ok := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
		if !ok || fn.Name() != "Hash" || fn.Pkg() == nil || !strings.HasPrefix(fn.Pkg().Path(), hashstructurePkg) || len(call.Args) == 0 {
			return
		}
		if strings.HasSuffix(pass.Fset.Position(call.Pos()).Filename, "_test.go") {
			return
		}
		var hidden []string
		walk(pass.TypesInfo.TypeOf(call.Args[0]), map[types.Type]bool{}, &hidden)
		if len(hidden) > 0 {
			pass.Reportf(call.Pos(), "hashstructure.Hash ignores unexported fields %s, so values differing only there collide; export them, implement Hashable, or tag hash:\"ignore\" (#3401)",
				strings.Join(hidden, ", "))
		}
	})
	return nil, nil
}

func walk(t types.Type, seen map[types.Type]bool, hidden *[]string) {
	if t == nil || seen[t] {
		return
	}
	seen[t] = true
	switch u := t.(type) {
	case *types.Alias:
		walk(types.Unalias(u), seen, hidden)
	case *types.Named:
		if allowed(u) {
			return
		}
		st, ok := u.Underlying().(*types.Struct)
		if !ok {
			walk(u.Underlying(), seen, hidden)
			return
		}
		walkStruct(st, u.Obj().Name(), seen, hidden)
	case *types.Struct:
		walkStruct(u, "struct", seen, hidden)
	case *types.Pointer:
		walk(u.Elem(), seen, hidden)
	case *types.Slice:
		walk(u.Elem(), seen, hidden)
	case *types.Array:
		walk(u.Elem(), seen, hidden)
	case *types.Map:
		walk(u.Key(), seen, hidden)
		walk(u.Elem(), seen, hidden)
	}
}

func walkStruct(st *types.Struct, owner string, seen map[types.Type]bool, hidden *[]string) {
	for i := range st.NumFields() {
		f := st.Field(i)
		if reflect.StructTag(st.Tag(i)).Get("hash") == "ignore" {
			continue
		}
		if !f.Exported() {
			if name := owner + "." + f.Name(); !slices.Contains(*hidden, name) {
				*hidden = append(*hidden, name)
			}
			continue
		}
		walk(f.Type(), seen, hidden)
	}
}

// allowed reports types hashstructure handles without reading their fields.
func allowed(n *types.Named) bool {
	if n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "time" && n.Obj().Name() == "Time" {
		return true
	}
	return implementsHashable(n) || implementsHashable(types.NewPointer(n))
}

func implementsHashable(t types.Type) bool {
	ms := types.NewMethodSet(t)
	for i := range ms.Len() {
		fn, ok := ms.At(i).Obj().(*types.Func)
		if !ok || fn.Name() != "Hash" {
			continue
		}
		sig := fn.Signature()
		if sig.Params().Len() == 0 && sig.Results().Len() == 2 {
			if b, ok := sig.Results().At(0).Type().(*types.Basic); ok && b.Kind() == types.Uint64 {
				return true
			}
		}
	}
	return false
}
