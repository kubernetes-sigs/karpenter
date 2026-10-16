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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1/cel"
)

var _ = Describe("Normalized price evaluator", func() {
	DescribeTable("preserves legacy semantics and submitted specs", func(spec v1alpha1.NodeOverlaySpec, basePrice, expected float64) {
		original := spec.DeepCopy()
		evaluator := newPriceEvaluator(spec, nil)
		price, err := evaluator.Evaluate(basePrice)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(BeNumerically("~", expected, 1e-9))
		Expect(spec).To(Equal(*original))
	},
		Entry("fixed price", v1alpha1.NodeOverlaySpec{Price: new("0.5")}, 2.0, 0.5),
		Entry("discount", v1alpha1.NodeOverlaySpec{PriceAdjustment: new("-10%")}, 2.0, 1.8),
		Entry("surcharge", v1alpha1.NodeOverlaySpec{PriceAdjustment: new("+10%")}, 2.0, 2.2),
		Entry("fixed addition", v1alpha1.NodeOverlaySpec{PriceAdjustment: new("+0.5")}, 2.0, 2.5),
		Entry("fixed subtraction", v1alpha1.NodeOverlaySpec{PriceAdjustment: new("-0.5")}, 2.0, 1.5),
		Entry("legacy zero clamp", v1alpha1.NodeOverlaySpec{PriceAdjustment: new("-3")}, 2.0, 0.0),
		Entry("full discount", v1alpha1.NodeOverlaySpec{PriceAdjustment: new("-100%")}, 2.0, 0.0),
	)
	It("keeps expression prices negative and uses the current provider price", func() {
		spec := v1alpha1.NodeOverlaySpec{PriceExpression: new("price - 3.0")}
		compiled, err := cel.Compile(*spec.PriceExpression)
		Expect(err).NotTo(HaveOccurred())
		evaluator := newPriceEvaluator(spec, compiled)
		price, err := evaluator.Evaluate(2.0)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(Equal(-1.0))
		price, err = evaluator.Evaluate(4.0)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(Equal(1.0))
	})
})
