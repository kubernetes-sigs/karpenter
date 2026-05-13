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

// Package cel provides CEL expression compilation and evaluation for NodeOverlay price expressions.
//
// Expressions have access to a single variable `self` with a `price` field (double) representing
// the instance type's base price. The expression must return a numeric value.
//
// Example: "(self.price * 0.9 + 0.05) * 1.03"
package cel

import (
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

var (
	priceEnv  *cel.Env
	envOnce   sync.Once
	envErr    error
)

func env() (*cel.Env, error) {
	envOnce.Do(func() {
		priceEnv, envErr = cel.NewEnv(
			// self is a map<string, dyn> so that self.price resolves to a double.
			cel.Variable("self", cel.MapType(cel.StringType, cel.DynType)),
		)
	})
	return priceEnv, envErr
}

// PriceExpression holds a compiled CEL program for evaluating price expressions.
type PriceExpression struct {
	prog cel.Program
	expr string
}

// Compile parses and type-checks a price CEL expression.
// The expression has access to self.price (float64) and must return a numeric value.
func Compile(expr string) (*PriceExpression, error) {
	e, err := env()
	if err != nil {
		return nil, fmt.Errorf("initializing CEL environment: %w", err)
	}
	ast, issues := e.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("invalid price expression %q: %w", expr, issues.Err())
	}
	outType := ast.OutputType()
	if outType != cel.DoubleType && outType != cel.IntType && outType != cel.UintType && outType.TypeName() != "dyn" {
		return nil, fmt.Errorf("price expression must return a numeric value, got %s", outType)
	}
	prog, err := e.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("building price expression program: %w", err)
	}
	return &PriceExpression{prog: prog, expr: expr}, nil
}

// Evaluate evaluates the price expression against the given base price and returns the result.
// Returns an error if the expression evaluates to a negative value.
func (p *PriceExpression) Evaluate(price float64) (float64, error) {
	out, _, err := p.prog.Eval(map[string]any{
		"self": map[string]any{"price": price},
	})
	if err != nil {
		return 0, fmt.Errorf("evaluating price expression %q: %w", p.expr, err)
	}
	result, err := toFloat64(out)
	if err != nil {
		return 0, fmt.Errorf("price expression %q: %w", p.expr, err)
	}
	if result < 0 {
		return 0, fmt.Errorf("price expression %q evaluated to negative value %f", p.expr, result)
	}
	return result, nil
}

func (p *PriceExpression) String() string {
	return p.expr
}

func toFloat64(v ref.Val) (float64, error) {
	switch val := v.(type) {
	case types.Double:
		return float64(val), nil
	case types.Int:
		return float64(val), nil
	case types.Uint:
		return float64(val), nil
	default:
		return 0, fmt.Errorf("unexpected return type %T, must be numeric", v)
	}
}
