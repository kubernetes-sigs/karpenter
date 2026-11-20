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

// Package sdkoptional flags dereferences through optional pointer fields of
// AWS SDK for Go v2 structs. The SDK models almost every response member as a
// pointer, and the deserializer leaves it nil whenever EC2 (or the caller that
// built the struct) omits it, so `out.A.B` and `*out.A` panic on a response
// shape the code didn't expect. Read through the pointer with lo.FromPtr (or
// aws.ToString and friends), or check it for nil first.
//
// A dereference is not flagged when the enclosing function compares the same
// expression to nil anywhere; that is a coarse guard check that keeps the rule
// quiet on already-defended code. Test files are skipped, since tests build the
// SDK structs they read.
//
// Inspired by:
//   - aws/karpenter-provider-aws#9863: updateUnavailableOfferingsCache read
//     err.LaunchTemplateAndOverrides.Overrides.AvailabilityZone, which panics
//     the NodeClaim lifecycle reconcile for CreateFleet errors without
//     launch template overrides (EC2 fleet-level errors, and the batcher's
//     synthesized partial-fulfillment error).
package sdkoptional

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// sdkPrefix matches the AWS SDK v2 service packages, both the operation
// input/output structs (service/ec2) and the shapes (service/ec2/types).
const sdkPrefix = "github.com/aws/aws-sdk-go-v2/service/"

var Analyzer = &analysis.Analyzer{
	Name: "sdkoptional",
	Doc:  "flags dereferences through optional AWS SDK v2 pointer fields without a nil check; use lo.FromPtr or check for nil (aws/karpenter-provider-aws#9863)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		if strings.HasSuffix(pass.Fset.File(f.Pos()).Name(), "_test.go") {
			continue
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			checkFunc(pass, fn.Body)
		}
	}
	return nil, nil
}

func checkFunc(pass *analysis.Pass, body *ast.BlockStmt) {
	guarded := nilCompared(body)
	seen := map[ast.Node]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok || seen[n] {
			return true
		}
		if _, ok := e.(*ast.SelectorExpr); !ok {
			if _, ok := e.(*ast.StarExpr); !ok {
				return true
			}
		}
		// Walk the chain from its outermost node toward the root, so a chain like
		// a.B.C.D is reported once, at the first pointer that can panic.
		var first ast.Expr
		for cur := e; ; {
			seen[cur] = true
			inner, derefs := derefOperand(pass, cur)
			if inner == nil {
				break
			}
			if derefs && optionalField(pass, inner) && !guarded[types.ExprString(inner)] {
				first = inner
			}
			cur = inner
		}
		if first != nil {
			pass.Reportf(first.Pos(), "%s is an optional AWS SDK pointer and may be nil; read it with lo.FromPtr or check it for nil first", types.ExprString(first))
		}
		return true
	})
}

// derefOperand returns the operand of a selector or star expression and whether
// evaluating e dereferences that operand.
func derefOperand(pass *analysis.Pass, e ast.Expr) (ast.Expr, bool) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return x.X, false
	case *ast.StarExpr:
		return ast.Unparen(x.X), true
	case *ast.SelectorExpr:
		sel, ok := pass.TypesInfo.Selections[x]
		if !ok {
			return nil, false // package-qualified identifier
		}
		operand := ast.Unparen(x.X)
		if _, isPtr := types.Unalias(pass.TypesInfo.TypeOf(operand)).(*types.Pointer); !isPtr {
			return operand, false
		}
		switch sel.Kind() {
		case types.FieldVal:
			return operand, true
		case types.MethodVal:
			// A value receiver method copies *operand; a pointer receiver can handle nil itself.
			sig := sel.Obj().Type().(*types.Signature)
			_, ptrRecv := types.Unalias(sig.Recv().Type()).(*types.Pointer)
			return operand, !ptrRecv
		}
		return operand, false
	}
	return nil, false
}

// optionalField reports whether e selects a pointer field declared on an AWS SDK struct.
func optionalField(pass *analysis.Pass, e ast.Expr) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	sel, ok := pass.TypesInfo.Selections[s]
	if !ok || sel.Kind() != types.FieldVal {
		return false
	}
	field := sel.Obj()
	if field.Pkg() == nil || !strings.HasPrefix(field.Pkg().Path(), sdkPrefix) {
		return false
	}
	_, isPtr := types.Unalias(field.Type()).(*types.Pointer)
	return isPtr
}

// nilCompared returns the expressions compared to nil anywhere in body.
func nilCompared(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		b, ok := n.(*ast.BinaryExpr)
		if !ok || (b.Op != token.EQL && b.Op != token.NEQ) {
			return true
		}
		for _, pair := range [][2]ast.Expr{{b.X, b.Y}, {b.Y, b.X}} {
			if id, ok := ast.Unparen(pair[1]).(*ast.Ident); ok && id.Name == "nil" {
				out[types.ExprString(ast.Unparen(pair[0]))] = true
			}
		}
		return true
	})
	return out
}
