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

package nodeoverlay

import (
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1/cel"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// priceEvaluator always receives the raw cloud provider offering price.
type priceEvaluator interface {
	Evaluate(float64) (float64, error)
}

type legacyPriceEvaluator string

func (p legacyPriceEvaluator) Evaluate(basePrice float64) (float64, error) {
	return cloudprovider.AdjustedPrice(basePrice, string(p)), nil
}

func newPriceEvaluator(spec v1alpha1.NodeOverlaySpec, compiled *cel.PriceExpression) priceEvaluator {
	if compiled != nil {
		return compiled
	}
	if spec.Price != nil {
		return legacyPriceEvaluator(*spec.Price)
	}
	if spec.PriceAdjustment != nil {
		return legacyPriceEvaluator(*spec.PriceAdjustment)
	}
	return nil
}
